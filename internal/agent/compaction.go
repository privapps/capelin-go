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

const compactionPrompt = `Summarize the conversation below for future continuation. Preserve the user's goals and constraints, decisions, important discovered facts, relevant file paths, identifiers, URLs, commands, user preferences, unresolved questions, and unfinished work. Be concise but complete. Do not mention this summarization request or invent facts. Return only the summary text.`
