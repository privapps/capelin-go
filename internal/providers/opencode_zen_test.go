package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"capelin-go/internal/contracts"
)

func TestIsOpenCodeZenEndpointRequiresExactHTTPSEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{endpoint: "https://opencode.ai/zen/v1/chat/completions", want: true},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions/", want: true},
		{endpoint: "http://opencode.ai/zen/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai:443/zen/v1/chat/completions", want: false},
		{endpoint: "https://api.opencode.ai/zen/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions/extra", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions?model=free", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat%2Fcompletions", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions?", want: false},
		{endpoint: "https://user@opencode.ai/zen/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai.evil.example/zen/v1/chat/completions", want: false},
	}

	for _, test := range tests {
		t.Run(test.endpoint, func(t *testing.T) {
			if got := IsOpenCodeZenEndpoint(test.endpoint); got != test.want {
				t.Fatalf("IsOpenCodeZenEndpoint(%q) = %v, want %v", test.endpoint, got, test.want)
			}
		})
	}
}

func TestProviderFactoryRoutesExactZenEndpointToBufferedSSEAdapter(t *testing.T) {
	type capturedRequest struct {
		body    map[string]any
		header  http.Header
		request *http.Request
	}
	var requests []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Zen request: %v", err)
			return
		}
		requests = append(requests, capturedRequest{body: body, header: r.Header.Clone(), request: r})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w,
			`data: {"choices":[{"delta":{"role":"assistant","content":"Hello ","reasoning_content":"first "}}]}`+"\n\n",
			`data: {"choices":[{"delta":{"content":"world","reasoning_content":"thought"}}]}`+"\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint:      "https://opencode.ai/zen/v1/chat/completions",
		Token:         "public",
		Model:         "zen-model",
		HTTP:          zenTestClient(server.URL),
		FreeTierTools: testFreeTierTools,
	})
	if _, ok := provider.(*OpenCodeZen); !ok {
		t.Fatalf("factory provider = %T, want *OpenCodeZen", provider)
	}

	state := provider.Initialize(nil, "hello")
	first, err := provider.Complete(context.Background(), state, nil, "", "max")
	if err != nil {
		t.Fatalf("first Zen completion: %v", err)
	}
	second, err := provider.Complete(context.Background(), state, nil, "", "max")
	if err != nil {
		t.Fatalf("second Zen completion: %v", err)
	}
	for name, completion := range map[string]contracts.Completion{"first": first, "second": second} {
		if got := completion.Content(); got != "Hello world" {
			t.Errorf("%s content = %q, want %q", name, got, "Hello world")
		}
		if got := completion.ReasoningContent(); got != "first thought" {
			t.Errorf("%s reasoning = %q, want %q", name, got, "first thought")
		}
	}

	if len(requests) != 2 {
		t.Fatalf("captured %d requests, want 2", len(requests))
	}
	sessionPattern := regexp.MustCompile(`^ses_[0-9a-f]{12}[A-Za-z0-9]{14}$`)
	requestPattern := regexp.MustCompile(`^msg_[0-9a-f]{12}[A-Za-z0-9]{14}$`)
	var sessionID, requestID string
	for index, request := range requests {
		if request.body["stream"] != true {
			t.Errorf("request %d stream = %#v, want true", index+1, request.body["stream"])
		}
		wireTools, ok := request.body["tools"].([]any)
		if !ok {
			t.Fatalf("request %d tools = %#v, want Zen free-tier tools", index+1, request.body["tools"])
		}
		wireNames := map[string]bool{}
		for _, raw := range wireTools {
			tool, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("request %d tool = %#v, want object", index+1, raw)
			}
			function, ok := tool["function"].(map[string]any)
			if !ok {
				t.Fatalf("request %d tool function = %#v, want object", index+1, tool["function"])
			}
			name, ok := function["name"].(string)
			if !ok {
				t.Fatalf("request %d tool name = %#v, want string", index+1, function["name"])
			}
			wireNames[name] = true
		}
		if !wireNames["read"] || !wireNames["bash"] {
			t.Errorf("request %d Zen wire tools = %#v, want read and bash", index+1, wireNames)
		}
		if request.body["model"] != "zen-model" {
			t.Errorf("request %d model = %#v, want zen-model", index+1, request.body["model"])
		}
		if request.body["reasoning_effort"] != "high" {
			t.Errorf("request %d reasoning_effort = %#v, want high", index+1, request.body["reasoning_effort"])
		}
		if request.header.Get("Authorization") != "Bearer public" {
			t.Errorf("request %d authorization = %q", index+1, request.header.Get("Authorization"))
		}
		if request.header.Get("Content-Type") != "application/json" {
			t.Errorf("request %d content type = %q", index+1, request.header.Get("Content-Type"))
		}
		if !strings.HasPrefix(request.header.Get("User-Agent"), "opencode/") {
			t.Errorf("request %d user agent = %q, want OpenCode-compatible user agent", index+1, request.header.Get("User-Agent"))
		}
		if request.header.Get("x-opencode-client") != "cli" {
			t.Errorf("request %d client = %q, want cli", index+1, request.header.Get("x-opencode-client"))
		}
		if request.header.Get("x-opencode-project") != "global" {
			t.Errorf("request %d project = %q, want global", index+1, request.header.Get("x-opencode-project"))
		}
		if !sessionPattern.MatchString(request.header.Get("x-opencode-session")) {
			t.Errorf("request %d session = %q, want ses_ ID", index+1, request.header.Get("x-opencode-session"))
		}
		if !requestPattern.MatchString(request.header.Get("x-opencode-request")) {
			t.Errorf("request %d request ID = %q, want msg_ ID", index+1, request.header.Get("x-opencode-request"))
		}
		if index == 0 {
			sessionID = request.header.Get("x-opencode-session")
			requestID = request.header.Get("x-opencode-request")
		} else {
			if request.header.Get("x-opencode-session") != sessionID {
				t.Errorf("session changed from %q to %q", sessionID, request.header.Get("x-opencode-session"))
			}
			if request.header.Get("x-opencode-request") == requestID {
				t.Errorf("request ID %q was reused", requestID)
			}
		}
	}
}

func TestOpenCodeZenRejectsIncompleteSSEStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	_, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", "")
	if err == nil || !strings.Contains(err.Error(), "ended before completion") {
		t.Fatalf("incomplete Zen stream error = %v, want explicit incomplete-stream error", err)
	}
}

func TestOpenCodeZenRejectsMalformedSSEEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {not-json}\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	_, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", "")
	if err == nil || !strings.Contains(err.Error(), "invalid SSE event") {
		t.Fatalf("malformed Zen stream error = %v, want explicit malformed-event error", err)
	}
}

func TestOpenCodeZenRejectsNegativeToolIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"call-1","function":{"name":"read","arguments":"{}"}}]}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	_, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), []contracts.Tool{
		{Type: "function", Function: contracts.ToolSpec{Name: "read_file"}},
	}, "model", "")
	if err == nil || !strings.Contains(err.Error(), "negative tool-call index") {
		t.Fatalf("negative tool index error = %v, want explicit parser error", err)
	}
}

func TestOpenCodeZenRejectsUnsupportedResponseTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"bash","arguments":"{}"}}]}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	_, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), []contracts.Tool{
		{Type: "function", Function: contracts.ToolSpec{Name: "read_file"}},
	}, "model", "")
	if err == nil || !strings.Contains(err.Error(), `unsupported tool "bash"`) {
		t.Fatalf("unsupported Zen tool error = %v, want explicit mapping error", err)
	}
}

func TestOpenCodeZenMapsWriteResponseTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request zenRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Zen request: %v", err)
			return
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "write" {
			t.Errorf("wire tools = %#v, want one write tool", request.Tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-write","type":"function","function":{"name":"write","arguments":"{\"filePath\":\"notes.txt\",\"content\":\"hello\"}"}}]}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	completion, err := provider.Complete(context.Background(), provider.Initialize(nil, "write a note"), []contracts.Tool{
		{Type: "function", Function: contracts.ToolSpec{Name: "write_file"}},
	}, "model", "")
	if err != nil {
		t.Fatalf("Zen write completion: %v", err)
	}
	calls := completion.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v, want one call", calls)
	}
	if calls[0].Function.Name != "write_file" {
		t.Fatalf("normalized tool name = %q, want write_file", calls[0].Function.Name)
	}
}

func TestZenToolNameMapsOpenCodeFileToolsWithoutChangingAppendSemantics(t *testing.T) {
	tests := map[string]string{
		"read_file":       "read",
		"write_file":      "write",
		"edit_file":       "edit",
		"execute_program": "bash",
		"append_file":     "append_file",
	}
	for input, want := range tests {
		if got := zenToolName(input); got != want {
			t.Errorf("zenToolName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestZenFreeTierExecuteProgramDescriptionIsDirect(t *testing.T) {
	for _, tool := range testFreeTierTools() {
		if tool.Function.Name != "execute_program" {
			continue
		}
		for _, want := range []string{"directly", "executable", "argument vector", "never invoke or parse a shell"} {
			if !strings.Contains(tool.Function.Description, want) {
				t.Fatalf("execute_program description %q does not contain %q", tool.Function.Description, want)
			}
		}
		return
	}
	t.Fatal("Zen free-tier tools did not include execute_program")
}

func TestZenPublicTierRequiresFreeTierTools(t *testing.T) {
	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		Token:    "public",
	})
	_, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", "")
	if err == nil || !strings.Contains(err.Error(), "free-tier tool catalog is unavailable") {
		t.Fatalf("public Zen without catalog error = %v, want explicit catalog error", err)
	}
}

func testFreeTierTools() []contracts.Tool {
	return []contracts.Tool{
		{
			Type: "function",
			Function: contracts.ToolSpec{
				Name:        "read_file",
				Description: "Read a file from the workspace.",
				Parameters:  map[string]any{"type": "object"},
			},
		},
		{
			Type: "function",
			Function: contracts.ToolSpec{
				Name:        "execute_program",
				Description: "Execute a local program directly with an explicit executable and argument vector; never invoke or parse a shell.",
				Parameters:  map[string]any{"type": "object"},
			},
		},
	}
}

func TestProviderFactoryPreservesNonZenChatJSONBehavior(t *testing.T) {
	var request map[string]any
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Chat Completions request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ordinary"}}]}`)
	}))
	defer server.Close()

	provider := New(Config{Endpoint: server.URL + "/v1/chat/completions", HTTP: server.Client()})
	if _, ok := provider.(*ChatCompletions); !ok {
		t.Fatalf("factory provider = %T, want *ChatCompletions", provider)
	}
	completion, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", "")
	if err != nil {
		t.Fatalf("ordinary Chat Completions request: %v", err)
	}
	if completion.Content() != "ordinary" {
		t.Fatalf("content = %q, want ordinary", completion.Content())
	}
	if _, present := request["stream"]; present {
		t.Fatalf("ordinary request unexpectedly contains stream: %#v", request["stream"])
	}
	if headers.Get("User-Agent") != contracts.CapelinUserAgent {
		t.Fatalf("ordinary user agent = %q, want %q", headers.Get("User-Agent"), contracts.CapelinUserAgent)
	}
	if headers.Get("x-opencode-client") != "" || headers.Get("x-opencode-session") != "" {
		t.Fatalf("Zen headers leaked to ordinary endpoint: %v", headers)
	}
}

func TestOpenCodeZenMapsToolNamesAndFragmentsAcrossContinuation(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Zen request: %v", err)
			return
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			_, _ = fmt.Fprint(w,
				`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"go\""}}]}}]}`+"\n\n",
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"args\":[\"version\"]}"}}]}}]}`+"\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		_, _ = fmt.Fprint(w,
			`data: {"choices":[{"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	state := provider.Initialize(nil, "run a version check")
	tools := []contracts.Tool{
		{Type: "function", Function: contracts.ToolSpec{Name: "read_file", Description: "read", Parameters: map[string]any{"type": "object"}}},
		{Type: "function", Function: contracts.ToolSpec{Name: "execute_program", Description: "execute", Parameters: map[string]any{"type": "object"}}},
	}
	completion, err := provider.Complete(context.Background(), state, tools, "model", "")
	if err != nil {
		t.Fatalf("first Zen completion: %v", err)
	}
	calls := completion.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v, want one call", calls)
	}
	if calls[0].ID != "call-1" || calls[0].Function.Name != "execute_program" || calls[0].Function.Arguments != `{"command":"go","args":["version"]}` {
		t.Fatalf("normalized tool call = %#v", calls[0])
	}
	provider.ApplyResponse(state, completion)
	provider.ApplyToolResults(state, []contracts.ToolResult{{Call: calls[0], Output: "go version go1.25"}})
	final, err := provider.Complete(context.Background(), state, nil, "model", "")
	if err != nil {
		t.Fatalf("continuation Zen completion: %v", err)
	}
	if final.Content() != "done" {
		t.Fatalf("continuation content = %q, want done", final.Content())
	}

	if len(requests) != 2 {
		t.Fatalf("captured %d requests, want 2", len(requests))
	}
	wireTools, ok := requests[0]["tools"].([]any)
	if !ok || len(wireTools) != 2 {
		t.Fatalf("wire tools = %#v, want two tools", requests[0]["tools"])
	}
	wireNames := map[string]bool{}
	for _, raw := range wireTools {
		wireNames[raw.(map[string]any)["function"].(map[string]any)["name"].(string)] = true
	}
	if !wireNames["read"] || !wireNames["bash"] || len(wireNames) != 2 {
		t.Fatalf("wire tool names = %#v, want read and bash", wireNames)
	}

	messages := requests[1]["messages"].([]any)
	var assistant, toolMessage map[string]any
	for _, raw := range messages {
		message := raw.(map[string]any)
		switch message["role"] {
		case "assistant":
			assistant = message
		case "tool":
			toolMessage = message
		}
	}
	if assistant == nil || toolMessage == nil {
		t.Fatalf("continuation messages = %#v, want assistant and tool messages", messages)
	}
	toolCalls := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 || toolCalls[0].(map[string]any)["function"].(map[string]any)["name"] != "bash" {
		t.Fatalf("wire assistant tool calls = %#v, want bash", assistant["tool_calls"])
	}
	if toolMessage["tool_call_id"] != "call-1" || toolMessage["content"] != "go version go1.25" {
		t.Fatalf("wire tool result = %#v", toolMessage)
	}
}

func TestProviderFactoryPreservesResponsesSelection(t *testing.T) {
	provider := New(Config{Endpoint: "https://example.test/v1/responses"})
	if _, ok := provider.(*Responses); !ok {
		t.Fatalf("factory provider = %T, want *Responses", provider)
	}
}

func TestOpenCodeZenPreservesProviderErrorsAndRetryClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantBody  string
		wantRetry bool
	}{
		{
			name:      "free tier error",
			status:    http.StatusForbidden,
			body:      `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode free tier rejected request"}}`,
			wantBody:  "OpenCode free tier rejected request",
			wantRetry: false,
		},
		{
			name:      "rate limit",
			status:    http.StatusTooManyRequests,
			body:      `{"error":{"message":"slow down"}}`,
			wantBody:  "slow down",
			wantRetry: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			provider := New(Config{
				Endpoint: "https://opencode.ai/zen/v1/chat/completions",
				HTTP:     zenTestClient(server.URL),
			})
			_, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", "")
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d", test.status)) || !strings.Contains(err.Error(), test.wantBody) {
				t.Fatalf("Zen error = %v, want status %d and body %q", err, test.status, test.wantBody)
			}
			var retryable interface{ Retryable() bool }
			if got := errors.As(err, &retryable) && retryable.Retryable(); got != test.wantRetry {
				t.Fatalf("retryable=%v, want %v", got, test.wantRetry)
			}
		})
	}
}

func TestOpenCodeZenCancellationStopsStream(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer func() {
		close(released)
		server.Close()
	}()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := provider.Complete(ctx, provider.Initialize(nil, "hello"), nil, "model", "")
		done <- err
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("Zen request did not reach test server")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Zen request did not stop after cancellation")
	}
}

func TestOpenCodeZenDebugDoesNotLogAuthorizationToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() {
		os.Stderr = previous
		_ = writer.Close()
		_ = reader.Close()
	}()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		Token:    "secret-token",
		Debug:    true,
		HTTP:     zenTestClient(server.URL),
	})
	if _, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", ""); err != nil {
		t.Fatalf("debug Zen completion: %v", err)
	}
	os.Stderr = previous
	_ = writer.Close()
	logged, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "secret-token") || strings.Contains(string(logged), "Bearer secret-token") {
		t.Fatalf("debug output leaked authorization token: %s", logged)
	}
	if !strings.Contains(string(logged), "buffered Zen SSE") || !strings.Contains(string(logged), `content="ok"`) {
		t.Fatalf("debug output omitted Zen response diagnostic: %s", logged)
	}
}

func zenTestClient(target string) *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		parsed, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		clone.URL.Scheme = parsed.Scheme
		clone.URL.Host = parsed.Host
		clone.URL.Path = parsed.Path
		clone.URL.RawPath = parsed.RawPath
		clone.URL.RawQuery = parsed.RawQuery
		return http.DefaultTransport.RoundTrip(clone)
	})}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
