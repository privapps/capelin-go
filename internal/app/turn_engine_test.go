package app

import (
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type turnEventSink struct {
	mu     sync.Mutex
	events []string
}

func (s *turnEventSink) WriteContent(agentID, content string) {
	s.add("content", agentID, content)
}

func (s *turnEventSink) WriteToolCall(agentID, name, args string) {
	s.add("tool_call", agentID, name, args)
}

func (s *turnEventSink) WriteToolResult(agentID, name string, isError bool, detail string) {
	s.add("tool_result", agentID, name, fmt.Sprint(isError), detail)
}

func (s *turnEventSink) WriteSystem(agentID, message string) {
	s.add("system", agentID, message)
}

func (s *turnEventSink) add(kind string, values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, kind+":"+strings.Join(values, ":"))
}

func (s *turnEventSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestTurnEngineChatAndResponsesHaveEquivalentObservableTurns(t *testing.T) {
	workspace := t.TempDir()
	call := `{"path":"."}`
	toolCalls := []map[string]any{
		{"id": "call-1", "type": "function", "function": map[string]any{"name": toolListFiles, "arguments": call}},
		{"id": "call-2", "type": "function", "function": map[string]any{"name": toolListFiles, "arguments": call}},
	}

	chat, chatSink := runProtocolScenario(t, workspace, false, []string{
		chatTurnResponse("", "inspect", toolCalls),
		chatTurnResponse("finished", "done", nil),
	})
	responses, responsesSink := runProtocolScenario(t, workspace, true, []string{
		responsesTurnResponse("inspect", toolCalls),
		protocolFinalResponse(true, "finished"),
	})

	if chat.result != responses.result || chat.reasoning != responses.reasoning {
		t.Fatalf("protocol results differ: chat=(%q, %q), responses=(%q, %q)", chat.result, chat.reasoning, responses.result, responses.reasoning)
	}
	if !reflect.DeepEqual(chatSink.snapshot(), responsesSink.snapshot()) {
		t.Fatalf("protocol sink events differ:\nchat=%v\nresponses=%v", chatSink.snapshot(), responsesSink.snapshot())
	}
	if !reflect.DeepEqual(chat.toolResultIDs, []string{"call-1", "call-2"}) || !reflect.DeepEqual(responses.toolResultIDs, []string{"call-1", "call-2"}) {
		t.Fatalf("tool results were not sent in original order: chat=%v responses=%v", chat.toolResultIDs, responses.toolResultIDs)
	}
}

func TestTurnEngineIterationPromptsUseProtocolAdapters(t *testing.T) {
	workspace := t.TempDir()
	toolCall := []map[string]any{{
		"id": "call-1", "type": "function",
		"function": map[string]any{"name": toolListFiles, "arguments": `{"path":"."}`},
	}}
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			result := runProtocolScenarioWithConfig(t, workspace, responses, 4, []string{
				protocolToolResponse(responses, "inspect", toolCall),
				protocolFinalResponse(responses, "finished"),
			})
			if result.err != nil {
				t.Fatalf("run turn: %v", result.err)
			}
			if !result.nearLimitPrompt || result.result != "finished" {
				t.Fatalf("near-limit prompt/result mismatch: prompt=%v result=%q", result.nearLimitPrompt, result.result)
			}

			result = runProtocolScenarioWithConfig(t, workspace, responses, 1, []string{
				protocolToolResponse(responses, "inspect", toolCall),
				protocolFinalResponse(responses, "fallback finished"),
			})
			if result.err != nil {
				t.Fatalf("run capped turn: %v", result.err)
			}
			if !result.finalPrompt || result.result != "fallback finished" {
				t.Fatalf("final prompt/result mismatch: prompt=%v result=%q", result.finalPrompt, result.result)
			}
		})
	}
}

func TestTurnEnginePreservesMalformedResponseErrorsAcrossProtocols(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			result := runProtocolScenarioWithConfig(t, t.TempDir(), responses, 1, []string{"{"})
			if result.err == nil {
				t.Fatal("expected malformed model response error")
			}
		})
	}
}

func TestTurnEngineNoToolCompletionParity(t *testing.T) {
	chat := runProtocolScenarioWithConfig(t, t.TempDir(), false, 4, []string{
		protocolTextResponse(false, "finished", "thinking"),
	})
	responses := runProtocolScenarioWithConfig(t, t.TempDir(), true, 4, []string{
		protocolTextResponse(true, "finished", "thinking"),
	})

	if chat.err != nil || responses.err != nil {
		t.Fatalf("no-tool turn failed: chat=%v responses=%v", chat.err, responses.err)
	}
	if chat.result != responses.result || chat.reasoning != responses.reasoning {
		t.Fatalf("no-tool results differ: chat=(%q, %q), responses=(%q, %q)", chat.result, chat.reasoning, responses.result, responses.reasoning)
	}
	if !reflect.DeepEqual(chat.sink.snapshot(), responses.sink.snapshot()) {
		t.Fatalf("no-tool sink events differ: chat=%v responses=%v", chat.sink.snapshot(), responses.sink.snapshot())
	}
	if chat.modelRequests != 1 || responses.modelRequests != 1 {
		t.Fatalf("no-tool turn made unexpected model calls: chat=%d responses=%d", chat.modelRequests, responses.modelRequests)
	}
}

func TestTurnEngineTransientRetryParity(t *testing.T) {
	chat, chatRequests := runProtocolTransientRetryScenario(t, false)
	responses, responsesRequests := runProtocolTransientRetryScenario(t, true)

	if chat.err != nil || responses.err != nil {
		t.Fatalf("transient retry failed: chat=%v responses=%v", chat.err, responses.err)
	}
	assertTurnParity(t, "transient retry", chat, responses)
	if chatRequests != 2 || responsesRequests != 2 {
		t.Fatalf("expected one retry per protocol: chat=%d responses=%d", chatRequests, responsesRequests)
	}
	if !strings.Contains(strings.Join(chat.sink.snapshot(), "\n"), "model request failed") ||
		!strings.Contains(strings.Join(responses.sink.snapshot(), "\n"), "model request failed") {
		t.Fatalf("shared model retry event missing: chat=%v responses=%v", chat.sink.snapshot(), responses.sink.snapshot())
	}
}

func TestTurnEngineTimeoutRetryParity(t *testing.T) {
	originalPrivateFetch := allowPrivateFetch
	allowPrivateFetch = true
	defer func() { allowPrivateFetch = originalPrivateFetch }()

	chat := runProtocolTimeoutRetryScenario(t, false)
	responses := runProtocolTimeoutRetryScenario(t, true)
	if chat.err != nil || responses.err != nil {
		t.Fatalf("timeout retry failed: chat=%v responses=%v", chat.err, responses.err)
	}
	assertTurnParity(t, "timeout retry", chat, responses)
	for _, result := range []turnRunResult{chat, responses} {
		if result.err != nil || result.result != "finished" {
			t.Fatalf("timeout retry failed: result=%q err=%v", result.result, result.err)
		}
		if !strings.Contains(strings.Join(result.sink.snapshot(), "\n"), "timed out, retrying") {
			t.Fatalf("timeout retry event missing: %v", result.sink.snapshot())
		}
		if result.toolRequests != 2 {
			t.Fatalf("expected one tool retry, got %d tool requests", result.toolRequests)
		}
	}
}

func TestTurnEngineIterationLimitDiagnosticRespectsEmitOutput(t *testing.T) {
	workspace := t.TempDir()
	toolCall := []map[string]any{{
		"id": "call-1", "type": "function",
		"function": map[string]any{"name": toolListFiles, "arguments": `{"path":"."}`},
	}}
	bodies := []string{
		protocolToolResponse(false, "inspect", toolCall),
		protocolFinalResponse(false, "fallback finished"),
	}

	// A visible run (root interactive/one-shot) still reports the exhaustion
	// diagnostic once on its own stream.
	visible := runProtocolScenarioWithConfigAndSinkEmit(t, workspace, false, 1, true, bodies)
	if visible.err != nil {
		t.Fatalf("visible capped turn failed: %v", visible.err)
	}
	if got := strings.Join(visible.sink.snapshot(), "\n"); !strings.Contains(got, "Maximum tool iterations (1) reached") {
		t.Fatalf("visible run did not report the iteration-limit diagnostic: %v", visible.sink.snapshot())
	}

	// An invisible run (subagent session sharing the parent's sink) must stay
	// silent: the diagnostic is not attributed to an agent and would appear to
	// belong to the parent's own turn.
	hidden := runProtocolScenarioWithConfigAndSinkEmit(t, workspace, false, 1, false, bodies)
	if hidden.err != nil {
		t.Fatalf("hidden capped turn failed: %v", hidden.err)
	}
	if got := strings.Join(hidden.sink.snapshot(), "\n"); strings.Contains(got, "Maximum tool iterations") {
		t.Fatalf("hidden run leaked the iteration-limit diagnostic: %v", hidden.sink.snapshot())
	}
	if hidden.result != "fallback finished" {
		t.Fatalf("hidden capped turn result=%q, want %q", hidden.result, "fallback finished")
	}
}

func TestTurnEngineFinalOnlyParity(t *testing.T) {
	chat := runProtocolFinalOnlyScenario(t, false)
	responses := runProtocolFinalOnlyScenario(t, true)
	want := []string{"content:root:finished"}
	if !reflect.DeepEqual(chat, want) || !reflect.DeepEqual(responses, want) {
		t.Fatalf("final-only output mismatch: chat=%v responses=%v", chat, responses)
	}
}

func TestTurnEngineSubagentExecutionParity(t *testing.T) {
	chat := runProtocolSubagentScenario(t, false)
	responses := runProtocolSubagentScenario(t, true)
	if chat.result != responses.result || chat.result != "subagent finished" {
		t.Fatalf("subagent results differ: chat=%q responses=%q", chat.result, responses.result)
	}
	if chat.requests != 1 || responses.requests != 1 {
		t.Fatalf("subagent made unexpected model calls: chat=%d responses=%d", chat.requests, responses.requests)
	}
	if !chat.questionSeen || !responses.questionSeen {
		t.Fatalf("subagent question was not translated into either protocol request: chat=%v responses=%v", chat.questionSeen, responses.questionSeen)
	}
}

func assertTurnParity(t *testing.T, scenario string, chat, responses turnRunResult) {
	t.Helper()
	if chat.result != responses.result || normalizeTurnParityText(chat.reasoning) != normalizeTurnParityText(responses.reasoning) {
		t.Fatalf("%s results differ: chat=(%q, %q), responses=(%q, %q)", scenario, chat.result, chat.reasoning, responses.result, responses.reasoning)
	}
	chatEvents := normalizeTurnParityEvents(chat.sink.snapshot())
	responsesEvents := normalizeTurnParityEvents(responses.sink.snapshot())
	if !reflect.DeepEqual(chatEvents, responsesEvents) {
		t.Fatalf("%s sink events differ: chat=%v responses=%v", scenario, chatEvents, responsesEvents)
	}
	if chat.modelRequests != responses.modelRequests || chat.toolRequests != responses.toolRequests {
		t.Fatalf("%s call counts differ: chat=(model %d, tool %d), responses=(model %d, tool %d)", scenario, chat.modelRequests, chat.toolRequests, responses.modelRequests, responses.toolRequests)
	}
}

var turnParityURL = regexp.MustCompile(`http://127\.0\.0\.1:\d+`)

func normalizeTurnParityText(value string) string {
	return turnParityURL.ReplaceAllString(value, "http://test-server")
}

func normalizeTurnParityEvents(events []string) []string {
	result := make([]string, len(events))
	for i, event := range events {
		event = normalizeTurnParityText(event)
		if strings.Contains(event, "model request failed (429/5xx), retrying in ") {
			start := strings.Index(event, "model request failed (429/5xx), retrying in ")
			if end := strings.Index(event[start:], "…"); end >= 0 {
				event = event[:start] + "model request failed (429/5xx), retrying…" + event[start+end+len("…"):]
			}
		}
		result[i] = event
	}
	return result
}

type turnRunResult struct {
	result          string
	reasoning       string
	err             error
	nearLimitPrompt bool
	finalPrompt     bool
	toolResultIDs   []string
	modelRequests   int
	toolRequests    int
	requests        int
	questionSeen    bool
	sink            *turnEventSink
}

func runProtocolScenario(t *testing.T, workspace string, responses bool, bodies []string) (turnRunResult, *turnEventSink) {
	result := runProtocolScenarioWithConfigAndSink(t, workspace, responses, 4, bodies)
	return result, result.sink
}

func runProtocolScenarioWithConfig(t *testing.T, workspace string, responses bool, maxIterations int, bodies []string) turnRunResult {
	return runProtocolScenarioWithConfigAndSink(t, workspace, responses, maxIterations, bodies)
}

func runProtocolScenarioWithConfigAndSink(t *testing.T, workspace string, responses bool, maxIterations int, bodies []string) turnRunResult {
	return runProtocolScenarioWithConfigAndSinkEmit(t, workspace, responses, maxIterations, true, bodies)
}

// runProtocolScenarioWithConfigAndSinkEmit is the emit-aware variant of the
// shared protocol harness. The iteration-limit diagnostic is the only engine
// sink write that must respect EmitOutput: invisible runs (subagent sessions
// sharing the parent's sink) must never leak it into the parent's stream.
func runProtocolScenarioWithConfigAndSinkEmit(t *testing.T, workspace string, responses bool, maxIterations int, emitOutput bool, bodies []string) turnRunResult {
	t.Helper()
	var mu sync.Mutex
	requestIndex := 0
	result := turnRunResult{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode model request: %v", err)
			return
		}
		mu.Lock()
		index := requestIndex
		requestIndex++
		mu.Unlock()
		if index >= len(bodies) {
			t.Errorf("unexpected model request %d", index+1)
			return
		}
		if index == 1 {
			t.Logf("protocol=%v second request=%v", responses, payload)
			result.toolResultIDs = protocolToolResultIDs(t, responses, payload)
			result.nearLimitPrompt = protocolPayloadContains(payload, responses, "You have 3 iterations remaining")
			result.finalPrompt = protocolPayloadContains(payload, responses, "Maximum tool iterations reached")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[index]))
	}))
	defer server.Close()

	sink := &turnEventSink{}
	endpoint := server.URL + "/chat/completions"
	if responses {
		endpoint = server.URL + "/responses"
	}
	a := &app{
		cfg: config{
			workspaceRoot:   workspace,
			maxIterations:   maxIterations,
			toolMaxParallel: 2,
			toolTimeoutSec:  5,
			allowedTools:    map[string]bool{toolListFiles: true},
		},
		client: &client{endpoint: endpoint, model: "test-model", reasoning: "medium", http: server.Client()},
		sink:   sink,
	}
	a.toolset = buildAgentTools(a.cfg.allowedTools)
	_, result.result, result.reasoning, result.err = a.runTurnLoop(context.Background(), nil, "inspect", a.rootRuntime(), a.toolset, emitOutput)
	mu.Lock()
	result.modelRequests = requestIndex
	mu.Unlock()
	result.sink = sink
	return result
}

func chatTurnResponse(content, reasoning string, toolCalls []map[string]any) string {
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" && len(toolCalls) > 0 {
		message["reasoning"] = reasoning
	}
	if toolCalls != nil {
		message["tool_calls"] = toolCalls
	}
	return marshalTestJSON(map[string]any{"choices": []any{map[string]any{"message": message}}})
}

func responsesTurnResponse(reasoning string, toolCalls []map[string]any) string {
	output := make([]any, 0, 2)
	if reasoning != "" {
		output = append(output, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning}}})
	}
	for _, call := range toolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "call_id": call["id"], "name": call["function"].(map[string]any)["name"], "arguments": call["function"].(map[string]any)["arguments"],
		})
	}
	if len(toolCalls) == 0 {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": reasoning}}})
	}
	return marshalTestJSON(map[string]any{"output": output})
}

func protocolToolResponse(responses bool, reasoning string, calls []map[string]any) string {
	if responses {
		return responsesTurnResponse(reasoning, calls)
	}
	return chatTurnResponse("", reasoning, calls)
}

func protocolFinalResponse(responses bool, content string) string {
	if responses {
		return marshalTestJSON(map[string]any{"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content}}}}})
	}
	return chatTurnResponse(content, "", nil)
}

func protocolTextResponse(responses bool, content, reasoning string) string {
	if !responses {
		message := map[string]any{"role": "assistant", "content": content}
		if reasoning != "" {
			message["reasoning"] = reasoning
		}
		return marshalTestJSON(map[string]any{
			"choices": []any{map[string]any{"message": message}},
		})
	}
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	output = append(output, map[string]any{
		"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": content}},
	})
	return marshalTestJSON(map[string]any{"output": output})
}

func runProtocolTransientRetryScenario(t *testing.T, responses bool) (turnRunResult, int) {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		current := requests
		mu.Unlock()
		if current == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(protocolTextResponse(responses, "finished", "")))
	}))
	defer server.Close()

	endpoint := server.URL + "/chat/completions"
	if responses {
		endpoint = server.URL + "/responses"
	}
	sink := &turnEventSink{}
	a := &app{
		cfg:    config{workspaceRoot: t.TempDir(), maxIterations: 4},
		client: &client{endpoint: endpoint, model: "test-model", http: server.Client()},
		sink:   sink,
	}
	_, result, reasoning, err := a.runTurnLoop(context.Background(), nil, "inspect", a.rootRuntime(), nil, true)
	return turnRunResult{result: result, reasoning: reasoning, err: err, modelRequests: requests, sink: sink}, requests
}

func runProtocolTimeoutRetryScenario(t *testing.T, responses bool) turnRunResult {
	t.Helper()
	var toolRequests atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := toolRequests.Add(1)
		if current == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(1500 * time.Millisecond):
			}
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>tool finished</body></html>"))
	}))
	defer toolServer.Close()

	call := []map[string]any{{
		"id": "timeout-call", "type": "function",
		"function": map[string]any{
			"name": toolFetchPage,
			"arguments": marshalTestJSON(map[string]any{
				"url": toolServer.URL, "timeout_seconds": 1,
			}),
		},
	}}
	modelRequests := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelRequests++
		w.Header().Set("Content-Type", "application/json")
		if responses {
			if modelRequests == 1 {
				_, _ = w.Write([]byte(protocolToolResponse(true, "inspect", call)))
			} else {
				_, _ = w.Write([]byte(protocolTextResponse(true, "finished", "")))
			}
			return
		}
		if modelRequests == 1 {
			_, _ = w.Write([]byte(chatTurnResponse("", "inspect", call)))
		} else {
			_, _ = w.Write([]byte(chatTurnResponse("finished", "", nil)))
		}
	}))
	defer modelServer.Close()

	endpoint := modelServer.URL + "/chat/completions"
	if responses {
		endpoint = modelServer.URL + "/responses"
	}
	sink := &turnEventSink{}
	a := &app{
		cfg: config{
			workspaceRoot:      t.TempDir(),
			maxIterations:      4,
			toolMaxParallel:    1,
			toolTimeoutSec:     1,
			toolRetryOnTimeout: true,
			allowPrivateFetch:  allowPrivateFetch,
			allowedTools:       map[string]bool{toolFetchPage: true},
		},
		client: &client{endpoint: endpoint, model: "test-model", http: modelServer.Client()},
		sink:   sink,
	}
	a.toolset = buildAgentTools(a.cfg.allowedTools)
	_, result, reasoning, err := a.runTurnLoop(context.Background(), nil, "inspect", a.rootRuntime(), a.toolset, true)
	return turnRunResult{result: result, reasoning: reasoning, err: err, modelRequests: modelRequests, toolRequests: int(toolRequests.Load()), sink: sink}
}

func runProtocolFinalOnlyScenario(t *testing.T, responses bool) []string {
	t.Helper()
	call := []map[string]any{{
		"id": "final-only-call", "type": "function",
		"function": map[string]any{"name": toolListFiles, "arguments": "{\"path\":\".\"}"},
	}}
	bodies := []string{
		protocolToolResponseWithContent(responses, "intermediate", call),
		protocolTextResponse(responses, "finished", ""),
	}
	index := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[index]))
		index++
	}))
	defer server.Close()

	baseSink := &turnEventSink{}
	sink := &finalOnlySink{wrapped: baseSink}
	endpoint := server.URL + "/chat/completions"
	if responses {
		endpoint = server.URL + "/responses"
	}
	a := &app{
		cfg: config{
			workspaceRoot:   t.TempDir(),
			maxIterations:   4,
			toolMaxParallel: 1,
			toolTimeoutSec:  5,
			allowedTools:    map[string]bool{toolListFiles: true},
			finalOnly:       true,
		},
		client: &client{endpoint: endpoint, model: "test-model", http: server.Client()},
		sink:   sink,
	}
	a.toolset = buildAgentTools(a.cfg.allowedTools)
	_, _, _, err := a.runTurnLoop(context.Background(), nil, "inspect", a.rootRuntime(), a.toolset, true)
	if err != nil {
		t.Fatalf("final-only turn failed: %v", err)
	}
	sink.FlushContent()
	return baseSink.snapshot()
}

func runProtocolSubagentScenario(t *testing.T, responses bool) struct {
	result       string
	requests     int
	questionSeen bool
} {
	t.Helper()
	var requests atomic.Int32
	var questionSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode subagent model request: %v", err)
			return
		}
		questionSeen.Store(protocolPayloadContains(payload, responses, "inspect this"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(protocolTextResponse(responses, "subagent finished", "")))
	}))
	defer server.Close()

	endpoint := server.URL + "/chat/completions"
	if responses {
		endpoint = server.URL + "/responses"
	}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir(), maxIterations: 4},
		client:    &client{endpoint: endpoint, model: "test-model", http: server.Client()},
		subagents: newSubagentManager(defaultSubagentRuntimeConfig(), nil),
	}
	result, _, err := a.runSubagentSession(context.Background(), &agentRuntime{
		sessionID: "subagent-1", model: "test-model",
	}, &subagentSession{Question: "inspect this"})
	if err != nil {
		t.Fatalf("subagent turn failed: %v", err)
	}
	return struct {
		result       string
		requests     int
		questionSeen bool
	}{result: result, requests: int(requests.Load()), questionSeen: questionSeen.Load()}
}

func protocolToolResponseWithContent(responses bool, content string, calls []map[string]any) string {
	if !responses {
		return chatTurnResponse(content, "", calls)
	}
	output := []any{
		map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": content}},
		},
	}
	for _, call := range calls {
		function := call["function"].(map[string]any)
		output = append(output, map[string]any{
			"type": "function_call", "call_id": call["id"],
			"name": function["name"], "arguments": function["arguments"],
		})
	}
	return marshalTestJSON(map[string]any{"output": output})
}

func marshalTestJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func protocolPayloadContains(payload map[string]any, responses bool, want string) bool {
	if responses {
		input, _ := payload["input"].([]any)
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			if strings.Contains(fmt.Sprint(item["content"]), want) {
				return true
			}
		}
		return false
	}
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if strings.Contains(fmt.Sprint(message["content"]), want) {
			return true
		}
	}
	return false
}

func protocolToolResultIDs(t *testing.T, responses bool, payload map[string]any) []string {
	t.Helper()
	ids := []string{}
	if responses {
		input, _ := payload["input"].([]any)
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call_output" {
				ids = append(ids, fmt.Sprint(item["call_id"]))
			}
		}
		return ids
	}
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] == "tool" {
			ids = append(ids, fmt.Sprint(message["tool_call_id"]))
		}
	}
	return ids
}

var _ types.OutputSink = (*turnEventSink)(nil)
