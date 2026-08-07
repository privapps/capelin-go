package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"capelin-go/internal/contracts"
)

type testState struct{ messages []contracts.Message }

func (s *testState) Messages() []contracts.Message {
	return append([]contracts.Message(nil), s.messages...)
}

type testCompletion struct {
	content, reasoning string
	calls              []contracts.ToolCall
}

func (c testCompletion) Content() string                 { return c.content }
func (c testCompletion) ReasoningContent() string        { return c.reasoning }
func (c testCompletion) ToolCalls() []contracts.ToolCall { return c.calls }

type testProvider struct {
	responses []contracts.Completion
	states    int
	requests  int
	toolsets  [][]contracts.Tool
}

func (p *testProvider) Initialize(messages []contracts.Message, question string) contracts.TurnState {
	p.states++
	copied := append([]contracts.Message(nil), messages...)
	return &testState{messages: append(copied, contracts.Message{Role: "user", Content: question})}
}
func (p *testProvider) Complete(_ context.Context, _ contracts.TurnState, tools []contracts.Tool, _ string, _ string) (contracts.Completion, error) {
	p.requests++
	p.toolsets = append(p.toolsets, append([]contracts.Tool(nil), tools...))
	if len(p.responses) == 0 {
		return nil, errors.New("no response")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}
func (*testProvider) ApplyResponse(state contracts.TurnState, response contracts.Completion) {
	s := state.(*testState)
	s.messages = append(s.messages, contracts.Message{Role: "assistant", Content: response.Content(), ToolCalls: response.ToolCalls()})
}
func (*testProvider) ApplyToolResults(state contracts.TurnState, results []contracts.ToolResult) {
	s := state.(*testState)
	for _, result := range results {
		s.messages = append(s.messages, contracts.Message{Role: "tool", ToolCallID: result.Call.ID, Content: result.Output})
	}
}
func (*testProvider) AppendUserPrompt(state contracts.TurnState, content string) {
	s := state.(*testState)
	s.messages = append(s.messages, contracts.Message{Role: "user", Content: content})
}
func (*testProvider) AppendFinalPrompt(state contracts.TurnState) {
	s := state.(*testState)
	s.messages = append(s.messages, contracts.Message{Role: "user", Content: "final"})
}

type testRunner struct{ calls []contracts.ToolCall }

func (r *testRunner) Run(_ context.Context, calls []contracts.ToolCall) []contracts.ToolResult {
	r.calls = append(r.calls, calls...)
	out := make([]contracts.ToolResult, len(calls))
	for i, call := range calls {
		out[i] = contracts.ToolResult{Call: call, Output: "result"}
	}
	return out
}

type testCapability struct {
	tools  []contracts.Tool
	runner testRunner
}

func (c *testCapability) Tools() []contracts.Tool {
	return append([]contracts.Tool(nil), c.tools...)
}

func (c *testCapability) Run(ctx context.Context, calls []contracts.ToolCall) []contracts.ToolResult {
	return c.runner.Run(ctx, calls)
}

type retryMarker struct{}

func (retryMarker) Error() string   { return "temporary" }
func (retryMarker) Retryable() bool { return true }

func TestEngineUsesOneProviderSeamForToolContinuation(t *testing.T) {
	call := contracts.ToolCall{ID: "call-1", Type: "function", Function: contracts.FunctionCall{Name: "lookup", Arguments: "{}"}}
	provider := &testProvider{responses: []contracts.Completion{testCompletion{reasoning: "thinking", calls: []contracts.ToolCall{call}}, testCompletion{content: "done"}}}
	runner := &testRunner{}
	result, err := (&Engine{Provider: provider}).Run(context.Background(), RunOptions{Question: "question", MaxToolIterations: 4, ToolRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || result.Reasoning != "[Turn 1]\nThinking: thinking\n\n[Turn 1]\nTool calls:\n  lookup({})\n" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !reflect.DeepEqual(runner.calls, []contracts.ToolCall{call}) {
		t.Fatalf("calls=%v", runner.calls)
	}
	if provider.requests != 2 || provider.states != 1 {
		t.Fatalf("provider requests=%d states=%d", provider.requests, provider.states)
	}
}

func TestEngineUsesCapabilityCatalogAndRunner(t *testing.T) {
	call := contracts.ToolCall{ID: "call-capability", Type: "function", Function: contracts.FunctionCall{Name: "controlled", Arguments: `{"ok":true}`}}
	catalogTool := contracts.Tool{Type: "function", Function: contracts.ToolSpec{Name: "controlled", Parameters: map[string]any{"type": "object"}}}
	capability := &testCapability{tools: []contracts.Tool{catalogTool}}
	provider := &testProvider{responses: []contracts.Completion{
		testCompletion{calls: []contracts.ToolCall{call}},
		testCompletion{content: "completed"},
	}}
	result, err := (&Engine{Provider: provider}).Run(context.Background(), RunOptions{
		Question: "use the controlled adapter", MaxToolIterations: 3,
		// This static value must not override the runtime capability catalog.
		Tools:          []contracts.Tool{{Function: contracts.ToolSpec{Name: "wrong"}}},
		ToolCapability: capability,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "completed" {
		t.Fatalf("answer=%q", result.Answer)
	}
	if len(provider.toolsets) != 2 || !reflect.DeepEqual(provider.toolsets[0], []contracts.Tool{catalogTool}) {
		t.Fatalf("provider received catalog=%#v", provider.toolsets)
	}
	if !reflect.DeepEqual(capability.runner.calls, []contracts.ToolCall{call}) {
		t.Fatalf("capability runner calls=%v", capability.runner.calls)
	}
}

func TestEngineCancellationStopsBeforeProviderCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &testProvider{}
	_, err := (&Engine{Provider: provider}).Run(ctx, RunOptions{Question: "question"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if provider.requests != 0 {
		t.Fatalf("provider was called %d times", provider.requests)
	}
}

func TestEngineOnIterationLimitReportsExhaustionOnce(t *testing.T) {
	call := contracts.ToolCall{ID: "call-1", Type: "function", Function: contracts.FunctionCall{Name: "lookup", Arguments: "{}"}}
	provider := &testProvider{responses: []contracts.Completion{
		testCompletion{calls: []contracts.ToolCall{call}},
		testCompletion{content: "fallback answer"},
	}}
	var reported int
	result, err := (&Engine{Provider: provider}).Run(context.Background(), RunOptions{
		Question: "question", MaxToolIterations: 1, ToolRunner: &testRunner{},
		OnIterationLimit: func() { reported++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "fallback answer" {
		t.Fatalf("answer=%q", result.Answer)
	}
	if reported != 1 {
		t.Fatalf("OnIterationLimit called %d times, want 1", reported)
	}
}

func TestEngineOnIterationLimitNotCalledOnNormalCompletion(t *testing.T) {
	provider := &testProvider{responses: []contracts.Completion{testCompletion{content: "done"}}}
	reported := 0
	_, err := (&Engine{Provider: provider}).Run(context.Background(), RunOptions{
		Question: "question", MaxToolIterations: 4, ToolRunner: &testRunner{},
		OnIterationLimit: func() { reported++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if reported != 0 {
		t.Fatalf("OnIterationLimit called %d times on a clean turn", reported)
	}
}
