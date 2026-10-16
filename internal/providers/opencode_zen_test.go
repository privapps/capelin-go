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
	"reflect"
	"regexp"
	"strconv"
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
		{endpoint: "https://opencode.ai/inference/v1/chat/completions", want: true},
		{endpoint: "https://opencode.ai/inference/v1/chat/completions/", want: true},
		{endpoint: "http://opencode.ai/zen/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai:443/zen/v1/chat/completions", want: false},
		{endpoint: "https://api.opencode.ai/zen/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai/v1/chat/completions", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions/extra", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions?model=free", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat%2Fcompletions", want: false},
		{endpoint: "https://opencode.ai/zen/v1/chat/completions?", want: false},
		{endpoint: "https://opencode.ai/inference/v1/chat/completions?model=free", want: false},
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
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		Token:    "public",
		Model:    "zen-model",
		HTTP:     zenTestClient(server.URL),
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
	var previousSessionTime, previousRequestTime uint64
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
		if !wireNames["read"] || wireNames["bash"] || len(wireTools) != 1 {
			t.Errorf("request %d Zen wire tools = %#v, want only an inert read compatibility tool", index+1, wireNames)
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
		sessionTime, err := strconv.ParseUint(request.header.Get("x-opencode-session")[4:16], 16, 64)
		if err != nil {
			t.Fatalf("parse session time field: %v", err)
		}
		requestTime, err := strconv.ParseUint(request.header.Get("x-opencode-request")[4:16], 16, 64)
		if err != nil {
			t.Fatalf("parse request time field: %v", err)
		}
		if sessionTime+requestTime != (uint64(1)<<48)-1 {
			t.Errorf("request %d descending/ascending time fields = %012x/%012x, want complements", index+1, sessionTime, requestTime)
		}
		const encodedMillisMask = zenIDTimeMask >> 12
		encodedMillis := requestTime >> 12
		currentMillis := uint64(time.Now().UnixMilli()) & encodedMillisMask
		forwardDelta := (encodedMillis - currentMillis) & encodedMillisMask
		backwardDelta := (currentMillis - encodedMillis) & encodedMillisMask
		if forwardDelta > 60_000 && backwardDelta > 60_000 {
			t.Errorf("request %d ID timestamp decodes to millisecond field %012x, not near current time %012x", index+1, encodedMillis, currentMillis)
		}
		if index > 0 && (sessionTime >= previousSessionTime || requestTime <= previousRequestTime) {
			t.Errorf("request %d IDs did not advance: session=%012x request=%012x after %012x/%012x", index+1, sessionTime, requestTime, previousSessionTime, previousRequestTime)
		}
		previousSessionTime, previousRequestTime = sessionTime, requestTime
	}
}

func TestProviderFactoryRoutesInferenceZenEndpointToBufferedSSEAdapter(t *testing.T) {
	var body zenRequest
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode inference Zen request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/inference/v1/chat/completions",
		HTTP:     zenTestClient(server.URL),
	})
	if _, ok := provider.(*OpenCodeZen); !ok {
		t.Fatalf("factory provider = %T, want *OpenCodeZen", provider)
	}
	if _, err := provider.Complete(context.Background(), provider.Initialize(nil, "hello"), nil, "model", ""); err != nil {
		t.Fatalf("inference Zen completion: %v", err)
	}
	if !body.Stream || len(body.Tools) != 1 || body.Tools[0].Function.Name != "read" {
		t.Fatalf("inference Zen request = %#v, want stream and one read placeholder", body)
	}
	if !strings.HasPrefix(header.Get("User-Agent"), "opencode/") || header.Get("x-opencode-session") == "" || header.Get("x-opencode-request") == "" {
		t.Fatalf("inference Zen headers = %v, want OpenCode user agent and request IDs", header)
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
		if len(request.Tools) != 2 {
			t.Errorf("wire tools = %#v, want write plus an inert read placeholder", request.Tools)
		}
		wireNames := map[string]bool{}
		for _, tool := range request.Tools {
			wireNames[tool.Function.Name] = true
		}
		if !wireNames["write"] || !wireNames["read"] || wireNames["bash"] {
			t.Errorf("wire tool names = %#v, want write and inert read only", wireNames)
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

func TestOpenCodeZenAnonymousAndPaidToolWirePolicy(t *testing.T) {
	readSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"filePath": map[string]any{"type": "string"}},
		"required":   []any{"filePath"},
	}
	tests := []struct {
		name          string
		token         string
		tools         []contracts.Tool
		wantAuth      string
		wantNames     []string
		wantRealRead  bool
		wantInertRead bool
	}{
		{name: "public without tools", token: "public", wantAuth: "Bearer public", wantNames: []string{"read"}, wantInertRead: true},
		{name: "empty token without tools", token: "", wantNames: []string{"read"}, wantInertRead: true},
		{name: "paid without tools", token: "paid-token", wantAuth: "Bearer paid-token"},
		{
			name:  "public with real read",
			token: "public",
			tools: []contracts.Tool{{Type: "function", Function: contracts.ToolSpec{
				Name: "read_file", Description: "Read only an authorized file.", Parameters: readSchema,
			}}},
			wantAuth: "Bearer public", wantNames: []string{"read"}, wantRealRead: true,
		},
		{
			name:  "paid tool mapping",
			token: "paid-token",
			tools: []contracts.Tool{{Type: "function", Function: contracts.ToolSpec{
				Name: "write_file", Description: "Write an authorized file.", Parameters: map[string]any{"type": "object"},
			}}},
			wantAuth: "Bearer paid-token", wantNames: []string{"write"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request map[string]any
			var authorization string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorization = r.Header.Get("Authorization")
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode Zen request: %v", err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if test.wantRealRead {
					_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-read","type":"function","function":{"name":"read","arguments":"{\"filePath\":\"notes.txt\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()

			provider := New(Config{
				Endpoint: "https://opencode.ai/zen/v1/chat/completions",
				Token:    test.token,
				HTTP:     zenTestClient(server.URL),
			})
			completion, err := provider.Complete(context.Background(), provider.Initialize(nil, "inspect"), test.tools, "model", "")
			if err != nil {
				t.Fatalf("Zen completion: %v", err)
			}
			if got := completion.ToolCalls(); test.wantRealRead && (len(got) != 1 || got[0].Function.Name != "read_file") {
				t.Fatalf("real read response mapping = %#v, want read_file", got)
			}
			if request["stream"] != true {
				t.Fatalf("stream = %#v, want true", request["stream"])
			}
			wireTools, _ := request["tools"].([]any)
			var gotNames []string
			for _, raw := range wireTools {
				function := raw.(map[string]any)["function"].(map[string]any)
				gotNames = append(gotNames, function["name"].(string))
			}
			if !reflect.DeepEqual(gotNames, test.wantNames) {
				t.Fatalf("wire tool names = %#v, want %#v", gotNames, test.wantNames)
			}
			if authorization != test.wantAuth {
				t.Fatalf("authorization = %q, want %q", authorization, test.wantAuth)
			}
			if test.wantInertRead {
				function := wireTools[0].(map[string]any)["function"].(map[string]any)
				if strings.TrimSpace(function["description"].(string)) == "" || function["parameters"].(map[string]any)["additionalProperties"] != false {
					t.Fatalf("inert read schema = %#v, want a minimal closed object schema", function)
				}
			}
			if test.wantRealRead {
				function := wireTools[0].(map[string]any)["function"].(map[string]any)
				if function["description"] != "Read only an authorized file." || !reflect.DeepEqual(function["parameters"], readSchema) {
					t.Fatalf("real read schema changed on wire: %#v", function)
				}
			}
		})
	}
}

func TestOpenCodeZenRejectsInertReadCompatibilityCall(t *testing.T) {
	var names []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request zenRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Zen request: %v", err)
			return
		}
		for _, tool := range request.Tools {
			names = append(names, tool.Function.Name)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-read","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		Token:    "public",
		HTTP:     zenTestClient(server.URL),
	})
	_, err := provider.Complete(context.Background(), provider.Initialize(nil, "read a file"), nil, "model", "")
	if err == nil || !strings.Contains(err.Error(), "inert read compatibility tool") {
		t.Fatalf("inert read call error = %v, want rejection before tool execution", err)
	}
	if !reflect.DeepEqual(names, []string{"read"}) {
		t.Fatalf("advertised tool names = %#v, want only inert read", names)
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
