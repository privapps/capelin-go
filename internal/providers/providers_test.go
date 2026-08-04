package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"capelin-go/internal/contracts"
)

func TestChatCompletionsReplaysNativeReasoningForToolContinuation(t *testing.T) {
	call := contracts.ToolCall{
		ID: "call-1", Type: "function",
		Function: contracts.FunctionCall{Name: "lookup", Arguments: `{"key":"value"}`},
	}
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"native thought","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"key\":\"value\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"finished"}}]}`))
	}))
	defer server.Close()

	provider := NewChatCompletions(Config{Endpoint: server.URL, HTTP: server.Client()})
	state := provider.Initialize(nil, "inspect")
	response, err := provider.Complete(context.Background(), state, []contracts.Tool{{Type: "function", Function: contracts.ToolSpec{Name: "lookup"}}}, "model", "medium")
	if err != nil {
		t.Fatal(err)
	}
	if got := response.ReasoningContent(); got != "native thought" {
		t.Fatalf("reasoning=%q, want native thought", got)
	}
	provider.ApplyResponse(state, response)
	provider.ApplyToolResults(state, []contracts.ToolResult{{Call: call, Output: "tool result"}})
	if _, err := provider.Complete(context.Background(), state, nil, "model", "medium"); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(requests))
	}
	messages := requests[1]["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("continuation messages=%d, want 3: %#v", len(messages), messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "native thought" {
		t.Fatalf("assistant reasoning=%#v", assistant["reasoning_content"])
	}
	if _, present := assistant["reasoning"]; present {
		t.Fatal("normalized reasoning alias leaked onto Chat Completions wire")
	}
	if !reflect.DeepEqual(assistant["tool_calls"], []any{map[string]any{
		"id": "call-1", "type": "function",
		"function": map[string]any{"name": "lookup", "arguments": `{"key":"value"}`},
	}}) {
		t.Fatalf("assistant tool calls=%#v", assistant["tool_calls"])
	}
	tool := messages[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call-1" || tool["content"] != "tool result" {
		t.Fatalf("tool result=%#v", tool)
	}
}

func TestChatCompletionsReasoningAliasesPreferNativeAndOmitWhenAbsent(t *testing.T) {
	responses := []string{
		`{"choices":[{"message":{"role":"assistant","content":"ok","reasoning":"legacy","reasoning_content":"native"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"plain"}}]}`,
	}
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responses[len(requests)-1]))
	}))
	defer server.Close()

	provider := NewChatCompletions(Config{Endpoint: server.URL, HTTP: server.Client()})
	state := provider.Initialize(nil, "first")
	response, err := provider.Complete(context.Background(), state, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if response.ReasoningContent() != "native" {
		t.Fatalf("native alias did not take precedence: %q", response.ReasoningContent())
	}
	provider.ApplyResponse(state, response)
	provider.AppendUserPrompt(state, "second")
	response, err = provider.Complete(context.Background(), state, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if response.ReasoningContent() != "" {
		t.Fatalf("plain response reasoning=%q", response.ReasoningContent())
	}
	initial := requests[0]["messages"].([]any)[0].(map[string]any)
	if _, present := initial["reasoning_content"]; present {
		t.Fatal("reasoning_content was emitted before any reasoning was provided")
	}
}

func TestResponsesRestoresRawOutputItemsAcrossContinuationState(t *testing.T) {
	call := contracts.ToolCall{ID: "call-1", Type: "function", Function: contracts.FunctionCall{Name: "lookup", Arguments: `{}`}}
	first := `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"native thought"}]},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}]}`
	second := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}]}`
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		body := first
		if len(requests) > 1 {
			body = second
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	provider := NewResponses(Config{Endpoint: server.URL + "/responses", HTTP: server.Client()})
	state := provider.Initialize([]contracts.Message{{Role: "system", Content: "system"}}, "inspect")
	response, err := provider.Complete(context.Background(), state, nil, "model", "medium")
	if err != nil {
		t.Fatal(err)
	}
	provider.ApplyResponse(state, response)
	provider.ApplyToolResults(state, []contracts.ToolResult{{Call: call, Output: "tool result"}})
	normalized := state.Messages()
	continuation := provider.ContinuationState(state)
	if continuation == nil || continuation.Provider != responsesContinuationProvider {
		t.Fatalf("continuation=%#v", continuation)
	}

	resumed := provider.InitializeWithContinuation(normalized, "next prompt", continuation)
	if _, err := provider.Complete(context.Background(), resumed, nil, "model", "medium"); err != nil {
		t.Fatal(err)
	}
	input := requests[1]["input"].([]any)
	if len(input) != 6 {
		t.Fatalf("resumed input=%d, want 6: %#v", len(input), input)
	}
	if input[1].(map[string]any)["type"] != "user" && input[1].(map[string]any)["type"] != nil {
		// The first normalized system message remains a role item; this branch
		// only guards against accidentally reordering native output items.
		t.Fatalf("unexpected input item %#v", input[1])
	}
	if input[2].(map[string]any)["type"] != "reasoning" || input[3].(map[string]any)["type"] != "function_call" || input[4].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("native output ordering was not restored: %#v", input)
	}
	if input[5].(map[string]any)["content"] != "next prompt" {
		t.Fatalf("new prompt was not appended after restored state: %#v", input[5])
	}
}

func TestResponsesInvalidContinuationFallsBackToNormalizedMessages(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()

	reasoning := "normalized thought"
	provider := NewResponses(Config{Endpoint: server.URL + "/responses", HTTP: server.Client()})
	state := provider.InitializeWithContinuation([]contracts.Message{
		{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []contracts.ToolCall{{ID: "call-1", Type: "function", Function: contracts.FunctionCall{Name: "lookup", Arguments: `{}`}}}},
	}, "next", &contracts.ContinuationState{Provider: "other", Version: continuationVersion, Data: json.RawMessage(`{"input":[{"type":"stale"}]}`)})
	if _, err := provider.Complete(context.Background(), state, nil, "model", ""); err != nil {
		t.Fatal(err)
	}
	input := request["input"].([]any)
	if input[0].(map[string]any)["type"] != "reasoning" || input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["content"] != "next" {
		t.Fatalf("normalized fallback input=%#v", input)
	}
}
