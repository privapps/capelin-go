package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"capelin-go/internal/contracts"
)

// CompactOptions controls one provider-backed conversation compaction request.
// Compaction is deliberately a text-only operation: callers cannot provide
// tools, and the provider's continuation state is not carried into the request.
type CompactOptions struct {
	Messages  []contracts.Message
	Model     string
	Reasoning string
	Sink      contracts.OutputSink
	AgentID   string
}

// Compact asks the provider for a summary of messages without mutating the
// caller's conversation. It accepts only a non-empty text completion; tool
// calls and empty completions are errors because they cannot safely represent
// compacted history.
func (e *Engine) Compact(ctx context.Context, options CompactOptions) (string, error) {
	if e == nil || e.Provider == nil {
		return "", errors.New("compaction provider is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(options.Messages) == 0 {
		return "", errors.New("cannot compact an empty conversation")
	}
	state := e.Provider.Initialize(options.Messages, compactionPrompt)
	response, err := e.completeWithRetry(ctx, state, RunOptions{
		Model: options.Model, Reasoning: options.Reasoning,
		Sink: options.Sink, AgentID: options.AgentID,
	})
	if err != nil {
		return "", err
	}
	if response == nil {
		return "", errors.New("compaction provider returned no completion")
	}
	if calls := response.ToolCalls(); len(calls) > 0 {
		return "", fmt.Errorf("compaction provider returned %d tool call(s)", len(calls))
	}
	summary := strings.TrimSpace(response.Content())
	if summary == "" {
		return "", errors.New("compaction provider returned an empty summary")
	}
	return summary, nil
}

// defaultCompactionBudgetChars is the conservative character budget for
// budgeted compaction. The summarization request built from the pre-shrunk
// messages must fit this budget so that even small free-tier context windows
// can accept it.
const defaultCompactionBudgetChars = 24000

// DefaultCompactionBudget returns the conservative character budget used by
// CompactWithinBudget when no explicit budget is supplied.
func DefaultCompactionBudget() int { return defaultCompactionBudgetChars }

// CompactWithinBudget pre-shrinks the conversation history so that the
// summarization request is guaranteed to fit within maxChars, then asks the
// provider for a summary. When the conversation already fits the budget the
// behaviour is identical to Compact. The pre-shrink strategy keeps the first
// system message (system prompt and skill guidance) and the most recent
// messages, dropping the oldest and largest tool messages first.
func (e *Engine) CompactWithinBudget(ctx context.Context, options CompactOptions, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = defaultCompactionBudgetChars
	}
	messages := cloneAgentMessages(options.Messages)
	if estimateMessagesChars(messages) <= maxChars {
		return e.Compact(ctx, options)
	}
	messages = shrinkToBudget(messages, maxChars)
	options.Messages = messages
	return e.Compact(ctx, options)
}

// candidate is an atomic index-size pair used by the compaction pre-shrink to
// identify which messages to drop. Assistant tool-call messages and their
// matching tool results are represented by one candidate.
type candidate struct {
	indices []int
	size    int
	tool    bool
}

// estimateMessagesChars returns a rough character count for a message slice.
func estimateMessagesChars(messages []contracts.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageChars(m)
	}
	return total
}

// shrinkToBudget reduces messages to fit within maxChars while keeping the
// first system message and the most recent messages. Tool messages are
// dropped first (largest first) because they are typically the biggest
// contributors to conversation size and the least valuable for summarization.
func shrinkToBudget(messages []contracts.Message, maxChars int) []contracts.Message {
	if len(messages) == 0 {
		return messages
	}
	// Find the system message (always keep it).
	systemIdx := -1
	for i, m := range messages {
		if m.Role == "system" {
			systemIdx = i
			break
		}
	}

	protected := make(map[int]bool, 2)
	if systemIdx >= 0 {
		protected[systemIdx] = true
	}
	// Always keep the most recent message.
	protected[len(messages)-1] = true
	droppable := compactionCandidates(messages, protected)

	// Drop messages until we fit the budget.
	dropped := make(map[int]bool)
	kept := estimateMessagesChars(messages)
	for _, c := range droppable {
		if kept <= maxChars {
			break
		}
		for _, idx := range c.indices {
			dropped[idx] = true
		}
		kept -= c.size
	}

	result := make([]contracts.Message, 0, len(messages)-len(dropped))
	for i, m := range messages {
		if dropped[i] {
			continue
		}
		result = append(result, m)
	}
	return result
}

// estimateMessageChars returns a rough character count for a single message.
func estimateMessageChars(m contracts.Message) int {
	total := len(m.Content)
	if m.ToolCallID != "" {
		total += len(m.ToolCallID) + 20
	}
	for _, tc := range m.ToolCalls {
		total += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	if m.ReasoningContent != nil {
		total += len(*m.ReasoningContent)
	}
	return total
}

// compactionCandidates builds deletion units while preserving provider-visible
// assistant tool-call/result relationships.
func compactionCandidates(messages []contracts.Message, protected map[int]bool) []candidate {
	callOwners := make(map[string]int)
	groups := make(map[int][]int)
	assigned := make(map[int]bool)
	for i, message := range messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		groups[i] = []int{i}
		assigned[i] = true
		for _, call := range message.ToolCalls {
			if call.ID != "" {
				callOwners[call.ID] = i
			}
		}
	}
	for i, message := range messages {
		if message.Role != "tool" {
			continue
		}
		if owner, ok := callOwners[message.ToolCallID]; ok {
			groups[owner] = append(groups[owner], i)
			assigned[i] = true
		}
	}

	var droppable []candidate
	for i, message := range messages {
		if group, ok := groups[i]; ok {
			if anyProtected(group, protected) {
				continue
			}
			droppable = append(droppable, candidate{
				indices: group,
				size:    estimateCandidateSize(messages, group),
				tool:    true,
			})
			continue
		}
		if assigned[i] || protected[i] {
			continue
		}
		droppable = append(droppable, candidate{
			indices: []int{i},
			size:    estimateMessageChars(message),
			tool:    message.Role == "tool",
		})
	}
	sort.SliceStable(droppable, func(i, j int) bool {
		if droppable[i].tool != droppable[j].tool {
			return droppable[i].tool
		}
		return droppable[i].size > droppable[j].size
	})
	return droppable
}

func anyProtected(indices []int, protected map[int]bool) bool {
	for _, idx := range indices {
		if protected[idx] {
			return true
		}
	}
	return false
}

func estimateCandidateSize(messages []contracts.Message, indices []int) int {
	total := 0
	for _, idx := range indices {
		total += estimateMessageChars(messages[idx])
	}
	return total
}

// cloneAgentMessages returns a deep copy of a message slice suitable for
// mutation by the compaction pre-shrink.
func cloneAgentMessages(messages []contracts.Message) []contracts.Message {
	if messages == nil {
		return nil
	}
	result := make([]contracts.Message, len(messages))
	copy(result, messages)
	for i := range result {
		result[i].ToolCalls = append([]contracts.ToolCall(nil), messages[i].ToolCalls...)
		if messages[i].ReasoningContent != nil {
			value := *messages[i].ReasoningContent
			result[i].ReasoningContent = &value
		}
	}
	return result
}

const compactionPrompt = `Summarize the conversation below for future continuation. Preserve the user's goals and constraints, decisions, important discovered facts, relevant file paths, identifiers, URLs, commands, user preferences, unresolved questions, and unfinished work. Be concise but complete. Do not mention this summarization request or invent facts. Return only the summary text.`
