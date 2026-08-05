package agent

import (
	"context"
	"errors"
	"reflect"
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
