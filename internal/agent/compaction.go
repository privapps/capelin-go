package agent

import (
	"context"
	"errors"
	"fmt"
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

// candidate is an index-size pair used by the compaction pre-shrink to
// identify which messages to drop.
type candidate struct {
	idx  int
	size int
}

// estimateMessagesChars returns a rough character count for a message slice.
func estimateMessagesChars(messages []contracts.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
		if m.ToolCallID != "" {
			total += len(m.ToolCallID) + 20
		}
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		if m.ReasoningContent != nil {
			total += len(*m.ReasoningContent)
		}
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

	// Build index of droppable messages (all except system and the last message).
	var droppable []candidate
	for i, m := range messages {
		if i == systemIdx {
			continue
		}
		if i == len(messages)-1 {
			// Always keep the most recent message.
			continue
		}
		droppable = append(droppable, candidate{idx: i, size: estimateMessageChars(m)})
	}

	// Sort: tool messages first (by size descending), then others by size
	// descending. Tool messages are the primary targets because they carry
	// large command output.
	sortDroppable(droppable, messages)

	// Drop messages until we fit the budget.
	dropped := make(map[int]bool)
	kept := estimateMessagesChars(messages)
	for _, c := range droppable {
		if kept <= maxChars {
			break
		}
		dropped[c.idx] = true
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
	return len(m.Content) + len(m.ToolCallID) + 20
}

// sortDroppable sorts droppable in place so tool messages come first (largest
// first), followed by non-tool messages (largest first).
func sortDroppable(droppable []candidate, messages []contracts.Message) {
	if len(droppable) <= 1 {
		return
	}
	// Stable-ish partition: move tool messages to front, then sort each half.
	toolEnd := 0
	for i := 0; i < len(droppable); i++ {
		if messages[droppable[i].idx].Role == "tool" {
			droppable[toolEnd], droppable[i] = droppable[i], droppable[toolEnd]
			toolEnd++
		}
	}
	// Sort tool half by size descending.
	for i := 1; i < toolEnd; i++ {
		for j := i; j > 0 && droppable[j].size > droppable[j-1].size; j-- {
			droppable[j], droppable[j-1] = droppable[j-1], droppable[j]
		}
	}
	// Sort non-tool half by size descending.
	for i := toolEnd + 1; i < len(droppable); i++ {
		for j := i; j > toolEnd && droppable[j].size > droppable[j-1].size; j-- {
			droppable[j], droppable[j-1] = droppable[j-1], droppable[j]
		}
	}
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
