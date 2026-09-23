package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"capelin-go/internal/contracts"
)

func TestEngineCompactUsesNoToolsAndPreservesMessages(t *testing.T) {
	messages := []contracts.Message{
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "remember the decision"},
	}
	before := append([]contracts.Message(nil), messages...)
	provider := &testProvider{responses: []contracts.Completion{testCompletion{content: "decision summary"}}}

	summary, err := (&Engine{Provider: provider}).Compact(context.Background(), CompactOptions{
		Messages: messages, Model: "compact-model", Reasoning: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "decision summary" {
		t.Fatalf("summary=%q", summary)
	}
	if !reflect.DeepEqual(messages, before) {
		t.Fatalf("compaction mutated source messages: got %#v want %#v", messages, before)
	}
	if provider.requests != 1 {
		t.Fatalf("provider requests=%d want 1", provider.requests)
	}
	if len(provider.toolsets) != 1 || len(provider.toolsets[0]) != 0 {
		t.Fatalf("compaction tools=%#v want none", provider.toolsets)
	}
}

func TestEngineCompactRejectsEmptyAndToolCompletions(t *testing.T) {
	messages := []contracts.Message{{Role: "system", Content: "instructions"}, {Role: "user", Content: "work"}}
	call := contracts.ToolCall{ID: "call-1", Type: "function", Function: contracts.FunctionCall{Name: "read_file", Arguments: `{}`}}
	for _, test := range []struct {
		name     string
		response contracts.Completion
	}{
		{name: "empty", response: testCompletion{}},
		{name: "tools", response: testCompletion{calls: []contracts.ToolCall{call}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &testProvider{responses: []contracts.Completion{test.response}}
			_, err := (&Engine{Provider: provider}).Compact(context.Background(), CompactOptions{Messages: messages})
			if err == nil {
				t.Fatal("compaction unexpectedly succeeded")
			}
		})
	}
}

func TestShrinkToBudgetDropsAssistantToolCallAndResultsTogether(t *testing.T) {
	call := contracts.ToolCall{
		ID:       "call-1",
		Type:     "function",
		Function: contracts.FunctionCall{Name: "read_file", Arguments: `{"path":"notes.txt"}`},
	}
	messages := []contracts.Message{
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []contracts.ToolCall{call}},
		{Role: "tool", ToolCallID: call.ID, Content: strings.Repeat("large output ", 20)},
		{Role: "user", Content: "latest request"},
	}
	groupSize := estimateMessageChars(messages[2]) + estimateMessageChars(messages[3])
	shrunk := shrinkToBudget(messages, estimateMessagesChars(messages)-groupSize)

	if hasToolCallMessage(shrunk, call.ID) || hasToolResult(shrunk, call.ID) {
		t.Fatalf("compaction split or retained an incomplete tool pair: %#v", shrunk)
	}
	if len(shrunk) != 3 || shrunk[0].Role != "system" || shrunk[len(shrunk)-1].Content != "latest request" {
		t.Fatalf("compaction removed protected history: %#v", shrunk)
	}
}

func TestShrinkToBudgetDropsMultiCallGroupAtomically(t *testing.T) {
	calls := []contracts.ToolCall{
		{ID: "call-1", Type: "function", Function: contracts.FunctionCall{Name: "read_file", Arguments: `{}`}},
		{ID: "call-2", Type: "function", Function: contracts.FunctionCall{Name: "list_files", Arguments: `{}`}},
	}
	messages := []contracts.Message{
		{Role: "system", Content: "instructions"},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolCallID: calls[0].ID, Content: strings.Repeat("first ", 20)},
		{Role: "tool", ToolCallID: calls[1].ID, Content: strings.Repeat("second ", 20)},
		{Role: "user", Content: "latest request"},
	}
	groupSize := estimateMessageChars(messages[1]) + estimateMessageChars(messages[2]) + estimateMessageChars(messages[3])
	shrunk := shrinkToBudget(messages, estimateMessagesChars(messages)-groupSize)

	for _, call := range calls {
		if hasToolCallMessage(shrunk, call.ID) || hasToolResult(shrunk, call.ID) {
			t.Fatalf("multi-call group was split for %q: %#v", call.ID, shrunk)
		}
	}
	if len(shrunk) != 2 {
		t.Fatalf("unexpected retained history: %#v", shrunk)
	}
}

func hasToolCallMessage(messages []contracts.Message, callID string) bool {
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ID == callID {
				return true
			}
		}
	}
	return false
}

func hasToolResult(messages []contracts.Message, callID string) bool {
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == callID {
			return true
		}
	}
	return false
}

type blockingCompactionProvider struct {
	testProvider
	started chan struct{}
}

func (p *blockingCompactionProvider) Complete(ctx context.Context, state contracts.TurnState, tools []contracts.Tool, model, reasoning string) (contracts.Completion, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEngineCompactHonorsCancellation(t *testing.T) {
	provider := &blockingCompactionProvider{
		testProvider: testProvider{responses: []contracts.Completion{testCompletion{content: "unused"}}},
		started:      make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&Engine{Provider: provider}).Compact(ctx, CompactOptions{
			Messages: []contracts.Message{{Role: "system", Content: "instructions"}, {Role: "user", Content: "work"}},
		})
		done <- err
	}()
	<-provider.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want context cancellation", err)
	}
}
