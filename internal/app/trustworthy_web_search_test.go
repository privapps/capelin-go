package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"capelin-go/internal/agent"
	"capelin-go/internal/contracts"
	"capelin-go/internal/tools"
	"capelin-go/internal/types"
)

type failingSearchTransport struct{}

func (failingSearchTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("global search client must not be used")
}

func TestRunToolWebSearchUsesApplicationHTTPClient(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			t.Fatal("MCP fallback was called after the application client returned DuckDuckGo results")
		}
		if r.Method != http.MethodPost || r.FormValue("kp") != "1" {
			t.Fatalf("unexpected DuckDuckGo request: method=%s kp=%q", r.Method, r.FormValue("kp"))
		}
		_, _ = fmt.Fprint(w, `<div class="result web-result"><a class="result__a" href="https://example.com/docs">Trusted Search</a><div class="result__snippet">trusted search evidence</div></div>`)
	}))
	defer searchServer.Close()

	tools.SetNetworkOverrides(true, &http.Client{Transport: failingSearchTransport{}}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}

	result, err := a.runTool(context.Background(), types.ToolCall{
		Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"trusted search"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Search provider: DuckDuckGo") || !strings.Contains(result, "Trusted Search") {
		t.Fatalf("web search did not use the application's HTTP client: %q", result)
	}
}

func TestRunToolWebSearchMCPFallbackThroughDispatcher(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			http.Error(w, "primary unavailable", http.StatusBadGateway)
		case "/mcp":
			if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json, text/event-stream" {
				t.Fatalf("unexpected MCP request: method=%s accept=%q", r.Method, r.Header.Get("Accept"))
			}
			if got := r.Header.Get("Authorization"); got != "Bearer parallel-secret" {
				t.Fatalf("Parallel credential = %q, want configured credential", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Parallel Trusted Search\",\"url\":\"https://parallel.example/result\",\"snippet\":\"trusted search evidence\"}]}"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	a := &app{
		cfg: config{
			workspaceRoot: t.TempDir(),
			allowedTools:  map[string]bool{toolWebSearch: true},
			searchConfig:  tools.SearchProviderConfig{ParallelAPIKey: "parallel-secret"},
		},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}
	result, err := a.runTool(context.Background(), types.ToolCall{
		Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"trusted search"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: Parallel", "Fallback: yes", "DuckDuckGo request or response failed", "Parallel Trusted Search"} {
		if !strings.Contains(result, want) {
			t.Errorf("MCP fallback output %q does not contain %q", result, want)
		}
	}
}

func TestRunToolWebSearchSelectsExaThroughDispatcher(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			http.Error(w, "primary unavailable", http.StatusBadGateway)
		case "/mcp":
			var request struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode Exa request: %v", err)
			}
			if request.Params.Name != "web_search_exa" {
				t.Errorf("MCP tool name = %q, want web_search_exa", request.Params.Name)
			}
			if got := r.Header.Get("x-api-key"); got != "exa-secret" {
				t.Errorf("Exa credential = %q, want configured credential", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"Title: Exa Trusted Search\\nURL: https://exa.example/result\\nHighlights: exa search evidence\"}]}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	a := &app{
		cfg: config{
			workspaceRoot: t.TempDir(),
			allowedTools:  map[string]bool{toolWebSearch: true},
			searchConfig:  tools.SearchProviderConfig{Provider: tools.SearchProviderExa, ExaAPIKey: "exa-secret"},
		},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}
	result, err := a.runTool(context.Background(), types.ToolCall{
		Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"exa search"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: Exa", "Fallback: yes", "Exa Trusted Search"} {
		if !strings.Contains(result, want) {
			t.Errorf("Exa output %q does not contain %q", result, want)
		}
	}
	if strings.Contains(result, "exa-secret") {
		t.Fatalf("Exa credential leaked in tool output: %q", result)
	}
}

func TestRunToolWebSearchMCPFailuresStayGenericThroughDispatcher(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	tests := []struct {
		name       string
		status     int
		mcpPayload string
	}{
		{name: "empty", status: http.StatusOK},
		{name: "malformed", status: http.StatusOK, mcpPayload: `{"secret":"provider body`},
		{name: "json rpc error", status: http.StatusOK, mcpPayload: `{"jsonrpc":"2.0","id":1,"error":{"message":"provider secret"}}`},
		{name: "provider unavailable", status: http.StatusBadGateway, mcpPayload: "provider cookies and secret"},
		{name: "oversized", status: http.StatusOK, mcpPayload: strings.Repeat("x", 512*1024+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ddg" {
					http.Error(w, "primary cookies and secret", http.StatusBadGateway)
					return
				}
				if test.status != http.StatusOK {
					w.WriteHeader(test.status)
				}
				_, _ = fmt.Fprint(w, test.mcpPayload)
			}))
			defer searchServer.Close()
			tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

			a := &app{
				cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
				client:  &client{http: &http.Client{}},
				toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
			}
			result, err := a.runTool(context.Background(), types.ToolCall{
				Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"trusted search"}`},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Search provider: none", "Search status: no trustworthy results"} {
				if !strings.Contains(result, want) {
					t.Errorf("failure output %q does not contain %q", result, want)
				}
			}
			for _, secret := range []string{"provider body", "provider secret", "provider cookies", "primary cookies"} {
				if strings.Contains(result, secret) {
					t.Errorf("failure output leaked %q: %q", secret, result)
				}
			}
		})
	}
}

func TestRunToolWebSearchHonorsCancellationThroughDispatcher(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	a := &app{
		cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := a.runTool(ctx, types.ToolCall{
		Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"cancelled search"}`},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled search error = %v, want context deadline", err)
	}
}

func TestRunToolWebSearchRejectsUntrustedMCPResults(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ddg" {
			_, _ = fmt.Fprint(w, `<div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Trusted porn result</a><div class="result__snippet">trusted search</div></div>`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"{\\\"results\\\":[{\\\"title\\\":\\\"Unrelated spam\\\",\\\"url\\\":\\\"https://spam.example/buy-now\\\",\\\"snippet\\\":\\\"nothing relevant\\\"}]}\"}]}}\n\n")
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	a := &app{
		cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}
	result, err := a.runTool(context.Background(), types.ToolCall{
		Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"trusted search"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: none", "Search status: no trustworthy results", "Rejected results are omitted."} {
		if !strings.Contains(result, want) {
			t.Errorf("no-results output %q does not contain %q", result, want)
		}
	}
	for _, rejected := range []string{"Trusted porn result", "unsafe.example", "Unrelated spam", "spam.example", "nothing relevant"} {
		if strings.Contains(result, rejected) {
			t.Errorf("no-results output exposed rejected content %q", result)
		}
	}
}

func TestRunToolWebSearchReasoningRetainsSafeSummary(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `<div class="result web-result"><a class="result__a" href="https://example.com/docs">Trusted Search</a><div class="result__snippet">trusted search evidence</div></div>`)
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	modelCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode model request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if modelCalls == 1 {
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"I need current search evidence","tool_calls":[{"id":"call-1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"trusted search\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The search is complete.","reasoning_content":"I checked the result."},"finish_reason":"stop"}]}`)
	}))
	defer modelServer.Close()

	a := &app{
		cfg:     config{workspaceRoot: t.TempDir(), toolMaxParallel: 1, toolTimeoutSec: 30, allowedTools: map[string]bool{toolWebSearch: true}},
		client:  &client{endpoint: modelServer.URL + "/chat/completions", http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}
	_, _, reasoning, err := a.runTurnLoop(context.Background(), []types.Message{{Role: "system", Content: "test"}}, "search for trusted information", a.rootRuntime(), a.toolset, true)
	if err != nil {
		t.Fatal(err)
	}
	if modelCalls != 2 || !strings.Contains(reasoning, "Trusted Search") || strings.Contains(reasoning, "Search provider:") {
		t.Fatalf("unexpected downstream reasoning: %q", reasoning)
	}
}

func TestRunToolWebSearchNormalizesParallelExcerptArraysForLongQueryThroughDispatcher(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			http.Error(w, "primary unavailable", http.StatusBadGateway)
		case "/mcp":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Aichi-Nagoya 2026 Asian Games schedule\",\"url\":\"https://games.example/schedule\",\"excerpts\":[\"The 2026 Asian Games football tournament dates and venues.\",\"The official schedule includes the men's final and medal results.\"]}]}"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	query := "2026 Asian Games men's football final venue schedule medal winners live results October broadcast timezone"
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}
	result, err := a.runTool(context.Background(), types.ToolCall{
		Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"` + query + `"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Search provider: Parallel",
		"Aichi-Nagoya 2026 Asian Games schedule",
		"The 2026 Asian Games football tournament dates and venues.",
		"The official schedule includes the men's final and medal results.",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("dispatcher search result %q does not include %q", result, want)
		}
	}
}

func TestRunToolWebSearchQualityMatrixThroughDispatcher(t *testing.T) {
	t.Run("mixed accepted rejected malformed and duplicate results", func(t *testing.T) {
		oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
		t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ddg" {
				_, _ = fmt.Fprint(w, `<div class="result web-result"><a class="result__a" href="https://example.com/accepted">Trusted Search Result</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="https://example.com/accepted#duplicate">Duplicate Search Result</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="javascript:alert(1)">Malformed Result</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="https://spam.example/buy-now">Unrelated spam</a><div class="result__snippet">nothing relevant</div></div>`)
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		tools.SetNetworkOverrides(true, &http.Client{}, server.URL+"/ddg", server.URL+"/mcp")
		a := &app{
			cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
			client:  &client{http: &http.Client{}},
			toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
		}
		result, err := a.runTool(context.Background(), types.ToolCall{
			Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"trusted search"}`},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result, "Search provider: DuckDuckGo") || strings.Count(result, "https://example.com/accepted") != 1 {
			t.Fatalf("mixed-quality result = %q, want one canonical accepted result", result)
		}
		for _, rejected := range []string{"Duplicate Search Result", "Malformed Result", "Unrelated spam", "spam.example"} {
			if strings.Contains(result, rejected) {
				t.Errorf("mixed-quality output exposed rejected result %q: %q", rejected, result)
			}
		}
	})

	t.Run("quality failure invokes MCP fallback", func(t *testing.T) {
		oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
		t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/ddg":
				_, _ = fmt.Fprint(w, `<div class="result web-result"><a class="result__a" href="https://spam.example/buy-now">Unrelated spam</a><div class="result__snippet">nothing relevant</div></div><div class="result web-result"><a class="result__a" href="not a url">Malformed Result</a><div class="result__snippet">trusted search evidence</div></div>`)
			case "/mcp":
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Trusted Search Quality Fallback\",\"url\":\"https://parallel.example/result\",\"snippet\":\"trusted search evidence\"}]}"}]}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		tools.SetNetworkOverrides(true, &http.Client{}, server.URL+"/ddg", server.URL+"/mcp")
		a := &app{
			cfg:     config{workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolWebSearch: true}},
			client:  &client{http: &http.Client{}},
			toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
		}
		result, err := a.runTool(context.Background(), types.ToolCall{
			Function: types.FunctionCall{Name: toolWebSearch, Arguments: `{"query":"trusted search"}`},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Search provider: Parallel", "Fallback: yes", "DuckDuckGo returned no trustworthy results", "Trusted Search Quality Fallback"} {
			if !strings.Contains(result, want) {
				t.Errorf("quality-fallback output %q does not contain %q", result, want)
			}
		}
	})
}

type searchBudgetRequestRecorder struct {
	mu          sync.Mutex
	duckQueries []string
	mcpCalls    int
}

func (r *searchBudgetRequestRecorder) recordDuckQuery(query string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.duckQueries = append(r.duckQueries, query)
}

func (r *searchBudgetRequestRecorder) snapshot() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.duckQueries...), r.mcpCalls
}

func newSearchBudgetFixture(t *testing.T) (*httptest.Server, *searchBudgetRequestRecorder) {
	t.Helper()
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	recorder := &searchBudgetRequestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse DuckDuckGo form: %v", err)
				return
			}
			query := r.Form.Get("q")
			recorder.recordDuckQuery(query)
			_, _ = fmt.Fprintf(w, `<div class="result web-result"><a class="result__a" href="https://example.com/search">%s</a><div class="result__snippet">%s</div></div>`, query, query)
		case "/mcp":
			recorder.mu.Lock()
			recorder.mcpCalls++
			recorder.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	tools.SetNetworkOverrides(true, &http.Client{}, server.URL+"/ddg", server.URL+"/mcp")
	return server, recorder
}

func newSearchBudgetCapability(t *testing.T, searchConfigs ...tools.SearchProviderConfig) appToolCapability {
	t.Helper()
	allowed := map[string]bool{toolWebSearch: true}
	cfg := config{
		workspaceRoot:   t.TempDir(),
		allowedTools:    allowed,
		toolMaxParallel: 4,
		toolTimeoutSec:  30,
	}
	if len(searchConfigs) > 0 {
		cfg.searchConfig = searchConfigs[0]
	}
	a := &app{
		cfg:     cfg,
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(allowed),
	}
	return newAppToolCapability(a.toolset, a, a.rootRuntime())
}

func searchBudgetCall(id, query string) types.ToolCall {
	arguments, _ := json.Marshal(map[string]string{"query": query})
	return types.ToolCall{
		ID:   id,
		Type: "function",
		Function: types.FunctionCall{
			Name:      toolWebSearch,
			Arguments: string(arguments),
		},
	}
}

func isSearchBudgetExhausted(t *testing.T, output string) bool {
	t.Helper()
	var result struct {
		Status        string `json:"status"`
		ProviderCalls int    `json:"provider_calls"`
		Limit         int    `json:"limit"`
		Guidance      string `json:"guidance"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Errorf("budget result is not structured JSON: %q (%v)", output, err)
		return false
	}
	if result.ProviderCalls != 5 || result.Limit != 5 || !strings.Contains(result.Guidance, "earlier searches") || !strings.Contains(result.Guidance, "known URL") {
		t.Errorf("budget exhaustion result omits its call count, limit, or guidance: %#v", result)
	}
	return result.Status == "budget_exhausted"
}

func TestSearchBudgetLimitsSequentialProviderCallsAndResetsForNewTurn(t *testing.T) {
	_, recorder := newSearchBudgetFixture(t)
	capability := newSearchBudgetCapability(t)
	var results []string
	for i := 1; i <= 6; i++ {
		batch := capability.Run(context.Background(), []types.ToolCall{searchBudgetCall(fmt.Sprintf("call-%d", i), fmt.Sprintf("budget query %d", i))})
		if len(batch) != 1 {
			t.Fatalf("batch %d returned %d results, want one", i, len(batch))
		}
		results = append(results, batch[0].Output)
	}
	if !isSearchBudgetExhausted(t, results[5]) {
		t.Fatalf("sixth search result = %q, want structured budget exhaustion", results[5])
	}
	queries, mcpCalls := recorder.snapshot()
	if len(queries) != 5 || mcpCalls != 0 {
		t.Fatalf("provider calls after six sequential searches: DuckDuckGo=%d Parallel=%d, want 5 and 0", len(queries), mcpCalls)
	}

	newTurn := newSearchBudgetCapability(t)
	fresh := newTurn.Run(context.Background(), []types.ToolCall{searchBudgetCall("fresh-turn", "fresh task query")})
	if len(fresh) != 1 || !strings.Contains(fresh[0].Output, "Search provider: DuckDuckGo") {
		t.Fatalf("new turn did not receive a fresh search budget: %#v", fresh)
	}
	queries, _ = recorder.snapshot()
	if len(queries) != 6 {
		t.Fatalf("provider invocation count after new turn = %d, want 6", len(queries))
	}
}

func TestSearchBudgetReusesEquivalentQueriesWithinTurn(t *testing.T) {
	_, recorder := newSearchBudgetFixture(t)
	capability := newSearchBudgetCapability(t)
	first := capability.Run(context.Background(), []types.ToolCall{searchBudgetCall("first", "budget duplicate query")})
	second := capability.Run(context.Background(), []types.ToolCall{searchBudgetCall("second", "  BUDGET   duplicate query  ")})
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("search results = %d and %d, want one each", len(first), len(second))
	}
	if first[0].Output != second[0].Output {
		t.Fatalf("equivalent query did not reuse prior result:\nfirst: %q\nsecond: %q", first[0].Output, second[0].Output)
	}
	queries, mcpCalls := recorder.snapshot()
	if len(queries) != 1 || mcpCalls != 0 {
		t.Fatalf("equivalent queries invoked providers DuckDuckGo=%d Parallel=%d, want 1 and 0", len(queries), mcpCalls)
	}
}

func TestSearchBudgetReservesParallelBatchDeterministically(t *testing.T) {
	_, recorder := newSearchBudgetFixture(t)
	capability := newSearchBudgetCapability(t)
	calls := make([]types.ToolCall, 6)
	for i := range calls {
		calls[i] = searchBudgetCall(fmt.Sprintf("parallel-%d", i+1), fmt.Sprintf("parallel topic %d", i+1))
	}

	results := capability.Run(context.Background(), calls)
	if len(results) != len(calls) {
		t.Fatalf("batch returned %d results, want %d", len(results), len(calls))
	}
	for i := range results {
		if results[i].Call.ID != calls[i].ID {
			t.Errorf("result %d call ID = %q, want %q", i, results[i].Call.ID, calls[i].ID)
		}
		if i == 5 {
			if !isSearchBudgetExhausted(t, results[i].Output) {
				t.Errorf("sixth parallel result = %q, want structured budget exhaustion", results[i].Output)
			}
		} else if !strings.Contains(results[i].Output, fmt.Sprintf("parallel topic %d", i+1)) {
			t.Errorf("result %d output = %q, missing its query evidence", i, results[i].Output)
		}
	}
	queries, mcpCalls := recorder.snapshot()
	if len(queries) != 5 || mcpCalls != 0 {
		t.Fatalf("provider calls after parallel batch: DuckDuckGo=%d Parallel=%d, want 5 and 0", len(queries), mcpCalls)
	}
	seenQueries := make(map[string]bool, len(queries))
	for _, query := range queries {
		seenQueries[query] = true
	}
	for i := 1; i <= 5; i++ {
		want := fmt.Sprintf("parallel topic %d", i)
		if !seenQueries[want] {
			t.Errorf("parallel provider calls omitted %q: %v", want, queries)
		}
	}
}

func TestSearchBudgetCountsFallbackProviderInvocations(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	recorder := &searchBudgetRequestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse DuckDuckGo form: %v", err)
				return
			}
			recorder.recordDuckQuery(r.Form.Get("q"))
			http.Error(w, "primary unavailable", http.StatusBadGateway)
		case "/mcp":
			recorder.mu.Lock()
			recorder.mcpCalls++
			recorder.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Fallback scenario\",\"url\":\"https://fallback.example/result\",\"snippet\":\"fallback scenario evidence\"}]}"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	tools.SetNetworkOverrides(true, &http.Client{}, server.URL+"/ddg", server.URL+"/mcp")
	capability := newSearchBudgetCapability(t)

	var results []string
	for i := 1; i <= 4; i++ {
		batch := capability.Run(context.Background(), []types.ToolCall{searchBudgetCall(fmt.Sprintf("fallback-%d", i), fmt.Sprintf("fallback scenario %d", i))})
		if len(batch) != 1 {
			t.Fatalf("fallback search %d returned %d results, want one", i, len(batch))
		}
		results = append(results, batch[0].Output)
	}
	for i := 0; i < 2; i++ {
		if !strings.Contains(results[i], "Search provider: Parallel") {
			t.Errorf("fallback result %d = %q, want hosted-provider evidence", i, results[i])
		}
	}
	if !isSearchBudgetExhausted(t, results[2]) || !isSearchBudgetExhausted(t, results[3]) {
		t.Fatalf("search after the fifth actual provider call did not report exhaustion: third=%q fourth=%q", results[2], results[3])
	}
	queries, mcpCalls := recorder.snapshot()
	if len(queries) != 3 || mcpCalls != 2 {
		t.Fatalf("actual provider invocations DuckDuckGo=%d Parallel=%d, want 3 and 2", len(queries), mcpCalls)
	}
}

func TestSearchBudgetResetsBetweenRealAgentTurns(t *testing.T) {
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })

	searchCalls := 0
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse DuckDuckGo form: %v", err)
			return
		}
		searchCalls++
		query := r.Form.Get("q")
		_, _ = fmt.Fprintf(w, `<div class="result web-result"><a class="result__a" href="https://example.com/fresh-turn">%s</a><div class="result__snippet">%s</div></div>`, query, query)
	}))
	defer searchServer.Close()
	tools.SetNetworkOverrides(true, &http.Client{}, searchServer.URL+"/ddg", searchServer.URL+"/mcp")

	modelCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls++
		w.Header().Set("Content-Type", "application/json")
		if modelCalls%2 == 1 {
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"search-call","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"fresh turn query\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Turn complete."},"finish_reason":"stop"}]}`)
	}))
	defer modelServer.Close()

	allowed := map[string]bool{toolWebSearch: true}
	a := &app{
		cfg: config{
			workspaceRoot:   t.TempDir(),
			allowedTools:    allowed,
			toolMaxParallel: 1,
			toolTimeoutSec:  30,
		},
		client:  &client{endpoint: modelServer.URL + "/chat/completions", model: "test", http: &http.Client{}},
		toolset: buildAgentTools(allowed),
	}
	messages := []types.Message{{Role: "system", Content: "test"}}
	runtime := a.rootRuntime()
	for turn := 0; turn < 2; turn++ {
		updated, answer, _, err := a.runTurnLoop(context.Background(), messages, "look up fresh turn query", runtime, a.toolset, true)
		if err != nil {
			t.Fatalf("turn %d failed: %v", turn+1, err)
		}
		if answer != "Turn complete." {
			t.Fatalf("turn %d answer = %q, want final answer", turn+1, answer)
		}
		messages = updated
	}
	if modelCalls != 4 || searchCalls != 2 {
		t.Fatalf("two user turns made %d model calls and %d provider calls, want 4 and 2", modelCalls, searchCalls)
	}
}

type fetchFailureOutput struct {
	Status           string `json:"status"`
	Category         string `json:"category"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	Guidance         string `json:"guidance"`
	PreviouslyFailed bool   `json:"previously_failed,omitempty"`
}

func newFetchRecoveryCapability(t *testing.T, fetchClient *http.Client) appToolCapability {
	t.Helper()
	allowed := map[string]bool{toolFetchPage: true}
	a := &app{
		cfg: config{
			workspaceRoot:     t.TempDir(),
			allowedTools:      allowed,
			allowPrivateFetch: true,
			toolMaxParallel:   4,
			toolTimeoutSec:    30,
		},
		client:    &client{http: &http.Client{}},
		fetchHTTP: fetchClient,
		toolset:   buildAgentTools(allowed),
	}
	return newAppToolCapability(a.toolset, a, a.rootRuntime())
}

func fetchPageCall(id, targetURL string) types.ToolCall {
	arguments, _ := json.Marshal(map[string]string{"url": targetURL})
	return types.ToolCall{
		ID:   id,
		Type: "function",
		Function: types.FunctionCall{
			Name:      toolFetchPage,
			Arguments: string(arguments),
		},
	}
}

func decodeFetchFailure(t *testing.T, output string) fetchFailureOutput {
	t.Helper()
	var failure fetchFailureOutput
	if err := json.Unmarshal([]byte(output), &failure); err != nil {
		t.Fatalf("fetch failure output is not structured JSON: %q (%v)", output, err)
	}
	if failure.Status != "fetch_failed" || strings.TrimSpace(failure.Guidance) == "" {
		t.Fatalf("fetch failure output lacks bounded status/guidance: %#v", failure)
	}
	return failure
}

func TestFetchFailuresAreClassifiedAndActionableThroughDispatcher(t *testing.T) {
	hits := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(http.StatusFound)
			return
		}
		statusByPath := map[string]int{
			"/missing":     http.StatusNotFound,
			"/gone":        http.StatusGone,
			"/denied":      http.StatusForbidden,
			"/limited":     http.StatusTooManyRequests,
			"/unavailable": http.StatusBadGateway,
		}
		w.WriteHeader(statusByPath[r.URL.Path])
		_, _ = fmt.Fprint(w, "upstream-secret-body")
	}))
	defer server.Close()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	capability := newFetchRecoveryCapability(t, client)
	tests := []struct {
		path       string
		category   string
		statusCode int
	}{
		{path: "/missing", category: "not_found", statusCode: http.StatusNotFound},
		{path: "/gone", category: "not_found", statusCode: http.StatusGone},
		{path: "/denied", category: "forbidden", statusCode: http.StatusForbidden},
		{path: "/limited", category: "rate_limited", statusCode: http.StatusTooManyRequests},
		{path: "/unavailable", category: "server_error", statusCode: http.StatusBadGateway},
		{path: "/redirect", category: "redirect", statusCode: http.StatusFound},
	}
	for i, test := range tests {
		batch := capability.Run(context.Background(), []types.ToolCall{fetchPageCall(fmt.Sprintf("failure-%d", i), server.URL+test.path)})
		if len(batch) != 1 {
			t.Fatalf("%s returned %d results, want one", test.path, len(batch))
		}
		failure := decodeFetchFailure(t, batch[0].Output)
		if failure.Category != test.category || failure.HTTPStatus != test.statusCode {
			t.Errorf("%s failure = %#v, want category %q and HTTP %d", test.path, failure, test.category, test.statusCode)
		}
		if strings.Contains(batch[0].Output, "upstream-secret-body") || strings.Contains(batch[0].Output, server.URL) {
			t.Errorf("%s failure exposed response body or raw URL: %q", test.path, batch[0].Output)
		}
	}
}

func TestFailedFetchURLIsSuppressedButDirectPublicURLRemainsAllowed(t *testing.T) {
	hits := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, "<html><body><p>direct public page</p></body></html>")
	}))
	defer server.Close()
	capability := newFetchRecoveryCapability(t, &http.Client{})
	first := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("first", server.URL+"/gone#one")})
	repeated := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("repeat", server.URL+"/gone#two")})
	direct := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("direct", server.URL+"/public")})
	if len(first) != 1 || len(repeated) != 1 || len(direct) != 1 {
		t.Fatalf("fetch batches returned lengths %d/%d/%d, want one each", len(first), len(repeated), len(direct))
	}
	_ = decodeFetchFailure(t, first[0].Output)
	failure := decodeFetchFailure(t, repeated[0].Output)
	if !failure.PreviouslyFailed || !strings.Contains(failure.Guidance, "choose another") {
		t.Errorf("repeated failed URL output = %#v, want a previously-failed notice and alternative guidance", failure)
	}
	if !strings.Contains(direct[0].Output, "direct public page") {
		t.Errorf("valid public URL supplied directly by the user was not fetched: %q", direct[0].Output)
	}
	if hits["/gone"] != 1 || hits["/public"] != 1 {
		t.Fatalf("server hit counts = %#v, want /gone=1 and /public=1", hits)
	}
}

func TestFetchTimeoutAndCancellationHaveBoundedCategories(t *testing.T) {
	started := make(chan struct{}, 2)
	blockingClient := func() *http.Client {
		return &http.Client{Transport: appFetchRoundTripper(func(req *http.Request) (*http.Response, error) {
			started <- struct{}{}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
	}

	timeoutCapability := newFetchRecoveryCapability(t, &http.Client{
		Timeout: 20 * time.Millisecond,
		Transport: appFetchRoundTripper(func(req *http.Request) (*http.Response, error) {
			started <- struct{}{}
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	})
	timed := timeoutCapability.Run(context.Background(), []types.ToolCall{fetchPageCall("timeout", "http://public.test/timeout")})
	if len(timed) != 1 || timed[0].Retried || decodeFetchFailure(t, timed[0].Output).Category != "timeout" {
		t.Fatalf("timeout result = %#v, want a non-retried classified timeout", timed)
	}

	cancelCapability := newFetchRecoveryCapability(t, blockingClient())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []agent.ToolResult, 1)
	go func() {
		done <- cancelCapability.Run(ctx, []types.ToolCall{fetchPageCall("cancel", "http://public.test/cancel")})
	}()
	<-started
	cancel()
	cancelled := <-done
	if len(cancelled) != 1 || decodeFetchFailure(t, cancelled[0].Output).Category != "cancelled" {
		t.Fatalf("cancellation result = %#v, want classified cancellation", cancelled)
	}
}

func TestUnsafeRedirectIsRedactedAndSuppressedWithinTask(t *testing.T) {
	sourceHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits++
		w.Header().Set("Location", "http://127.0.0.1:18080/private?token=redirect-secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	fetchClient := &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("safe-network policy denied redirect to %s", req.URL)
	}}
	capability := newFetchRecoveryCapability(t, fetchClient)
	target := server.URL + "/start?token=source-secret"
	first := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("unsafe-redirect", target)})
	repeated := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("unsafe-redirect-repeat", target)})
	if len(first) != 1 || len(repeated) != 1 {
		t.Fatalf("unsafe redirect fetches returned %d/%d results, want one each", len(first), len(repeated))
	}
	var failure fetchFailureOutput
	if err := json.Unmarshal([]byte(first[0].Output), &failure); err != nil {
		t.Fatalf("unsafe redirect result is not structured JSON: %q (%v)", first[0].Output, err)
	}
	if failure.Status != "fetch_blocked" || failure.Category != "policy_blocked" || !strings.Contains(failure.Guidance, "do not retry") {
		t.Errorf("unsafe redirect result = %#v, want bounded policy-blocked guidance", failure)
	}
	for _, secret := range []string{"source-secret", "redirect-secret", "127.0.0.1"} {
		if strings.Contains(first[0].Output, secret) {
			t.Errorf("unsafe redirect output exposed %q: %q", secret, first[0].Output)
		}
	}
	if first[0].IsError || first[0].Recovery != nil {
		t.Errorf("unsafe redirect received generic retry recovery: %#v", first[0])
	}
	var repeatedFailure fetchFailureOutput
	if err := json.Unmarshal([]byte(repeated[0].Output), &repeatedFailure); err != nil || !repeatedFailure.PreviouslyFailed {
		t.Errorf("repeated unsafe redirect result = %q, err=%v; want previously_failed", repeated[0].Output, err)
	}
	if sourceHits != 1 {
		t.Fatalf("unsafe redirect source was contacted %d times, want once", sourceHits)
	}
}

func TestFetchOriginTLSFailureIsNetworkFailureNotPolicyBlock(t *testing.T) {
	client := &http.Client{Transport: appFetchRoundTripper(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: http.MethodGet, URL: req.URL.String(), Err: x509.UnknownAuthorityError{Cert: &x509.Certificate{}}}
	})}
	capability := newFetchRecoveryCapability(t, client)
	result := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("tls-origin", "https://source.example/page?token=secret")})
	if len(result) != 1 {
		t.Fatalf("TLS fetch returned %d results, want one", len(result))
	}
	failure := decodeFetchFailure(t, result[0].Output)
	if failure.Category != "network_error" || failure.Status != "fetch_failed" {
		t.Fatalf("origin TLS failure was misclassified as policy block: %#v", failure)
	}
	for _, secret := range []string{"source.example", "token=secret", "secret"} {
		if strings.Contains(result[0].Output, secret) {
			t.Errorf("TLS failure output exposed %q: %q", secret, result[0].Output)
		}
	}
}

func TestFetchDialPolicyFailureIsSuppressedAndRedacted(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: appFetchRoundTripper(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("fetch page: refusing private or local IP 10.0.0.9")
	})}
	capability := newFetchRecoveryCapability(t, client)
	target := "https://public.example/page?token=secret-value"
	first := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("dial-policy", target)})
	repeated := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("dial-policy-repeat", target)})
	if len(first) != 1 || len(repeated) != 1 {
		t.Fatalf("policy fetch returned lengths %d/%d, want one each", len(first), len(repeated))
	}
	var firstFailure, repeatedFailure fetchFailureOutput
	if err := json.Unmarshal([]byte(first[0].Output), &firstFailure); err != nil {
		t.Fatalf("dial policy result is not structured JSON: %q (%v)", first[0].Output, err)
	}
	if err := json.Unmarshal([]byte(repeated[0].Output), &repeatedFailure); err != nil {
		t.Fatalf("repeated policy result is not structured JSON: %q (%v)", repeated[0].Output, err)
	}
	if firstFailure.Status != "fetch_blocked" || firstFailure.Category != "policy_blocked" || !repeatedFailure.PreviouslyFailed {
		t.Fatalf("policy failure results = %#v / %#v", firstFailure, repeatedFailure)
	}
	for _, secret := range []string{"10.0.0.9", "public.example", "secret-value"} {
		if strings.Contains(first[0].Output+repeated[0].Output, secret) {
			t.Errorf("policy failure exposed %q", secret)
		}
	}
	if attempts != 1 {
		t.Fatalf("blocked URL attempted %d times, want one", attempts)
	}
}

func TestFetchFailureKeepsStructuredModelOutputAndConciseInteractiveOutput(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream secret body", http.StatusNotFound)
	}))
	defer source.Close()

	call := fetchPageCall("missing-source", source.URL+"/missing")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments},
		}}),
		chatTurnResponse("done", "", nil),
	)
	testApp.app.cfg.allowPrivateFetch = true
	testApp.app.cfg.allowedTools = map[string]bool{toolFetchPage: true}
	testApp.app.fetchHTTP = source.Client()
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)

	var display string
	testApp.app.sink.(*spySink).onToolResult = func(toolName string, isError bool, detail string) {
		if toolName == toolFetchPage {
			display = detail
			if isError {
				t.Errorf("ordinary source failure was rendered as a dispatcher error: %q", detail)
			}
		}
	}

	_, answer, _, err := testApp.app.runTurnLoop(
		context.Background(), nil, "fetch the source", testApp.app.rootRuntime(), testApp.app.toolset, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q, want done", answer)
	}
	if strings.Contains(display, `"status"`) || strings.Contains(display, `"guidance"`) || strings.Contains(display, "corrected retry") || strings.Contains(display, "permission_scope") {
		t.Fatalf("interactive output exposed structured recovery details: %q", display)
	}
	for _, want := range []string{"source unavailable (HTTP 404)", "choose another URL"} {
		if !strings.Contains(display, want) {
			t.Fatalf("interactive output %q does not contain %q", display, want)
		}
	}
	modelRecovery := ""
	if len(testApp.requests) == 2 {
		for _, message := range testApp.requests[1].Messages {
			if message.Role == "tool" {
				modelRecovery += message.Content
			}
		}
	}
	if len(testApp.requests) != 2 || !strings.Contains(modelRecovery, `"category":"not_found"`) || !strings.Contains(modelRecovery, "choose another URL") {
		t.Fatalf("model did not retain structured fetch recovery: %#v", testApp.requests)
	}
}

func TestInvalidFetchArgumentKeepsCorrectedRetryRecovery(t *testing.T) {
	capability := newFetchRecoveryCapability(t, &http.Client{})
	results := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("invalid", "https://user:secret@example.com/page")})
	if len(results) != 1 {
		t.Fatalf("invalid fetch returned %d results, want one", len(results))
	}
	result := results[0]
	if !result.IsError || result.Recovery == nil {
		t.Fatalf("invalid fetch result = %#v, want corrected-retry error", result)
	}
	if result.Recovery.Phase != contracts.RecoveryPhaseToolInvocation || !strings.Contains(result.Recovery.Guidance, "corrected arguments") {
		t.Fatalf("invalid fetch recovery = %#v", result.Recovery)
	}
	for _, secret := range []string{"user:secret", "example.com/page"} {
		if strings.Contains(result.Output, secret) {
			t.Fatalf("invalid fetch output exposed %q: %q", secret, result.Output)
		}
	}
}

func TestAsianGamesRecoveryScenarioCombinesSearchEvidenceAndDeadSourceSuppression(t *testing.T) {
	var searchCalls, mcpCalls int
	hits := make(map[string]int)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			searchCalls++
			http.Error(w, "primary unavailable", http.StatusBadGateway)
		case "/mcp":
			mcpCalls++
			inner, _ := json.Marshal(map[string]any{"results": []any{map[string]any{
				"title": "Aichi-Nagoya 2026 Asian Games schedule", "url": server.URL + "/current",
				"excerpts": []string{"The 2026 Asian Games football tournament dates and venues.", "The official schedule includes the men's final and medal results."},
			}}})
			response := map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(inner)}}}}
			_ = json.NewEncoder(w).Encode(response)
		case "/stale-one", "/stale-two":
			hits[r.URL.Path]++
			http.NotFound(w, r)
		case "/current":
			hits[r.URL.Path]++
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html><body><h1>Current Asian Games source</h1></body></html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldClient, oldDDG, oldMCP := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultMCPURL()
	t.Cleanup(func() { tools.SetNetworkOverrides(false, oldClient, oldDDG, oldMCP) })
	tools.SetNetworkOverrides(true, server.Client(), server.URL+"/ddg", server.URL+"/mcp")

	allowed := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg: config{
			workspaceRoot:     t.TempDir(),
			allowedTools:      allowed,
			allowPrivateFetch: true,
			toolMaxParallel:   4,
			toolTimeoutSec:    30,
		},
		client:    &client{http: server.Client()},
		fetchHTTP: server.Client(),
		toolset:   buildAgentTools(allowed),
	}
	capability := newAppToolCapability(a.toolset, a, a.rootRuntime())
	query := "2026 Asian Games men's football final venue schedule medal winners live results October broadcast timezone"
	searchResult := capability.Run(context.Background(), []types.ToolCall{searchBudgetCall("asian-games-search", query)})
	if len(searchResult) != 1 || !strings.Contains(searchResult[0].Output, "Search provider: Parallel") || !strings.Contains(searchResult[0].Output, "Aichi-Nagoya 2026 Asian Games schedule") {
		t.Fatalf("Asian Games search result = %#v", searchResult)
	}

	for i, path := range []string{"/stale-one", "/stale-two"} {
		first := capability.Run(context.Background(), []types.ToolCall{fetchPageCall(fmt.Sprintf("stale-%d", i), server.URL+path)})
		repeated := capability.Run(context.Background(), []types.ToolCall{fetchPageCall(fmt.Sprintf("stale-repeat-%d", i), server.URL+path+"#same-source")})
		if len(first) != 1 || len(repeated) != 1 {
			t.Fatalf("stale source %s returned lengths %d/%d", path, len(first), len(repeated))
		}
		if !strings.Contains(first[0].Output, `"category":"not_found"`) || !strings.Contains(repeated[0].Output, `"previously_failed":true`) {
			t.Fatalf("stale source %s recovery outputs = %q / %q", path, first[0].Output, repeated[0].Output)
		}
	}
	current := capability.Run(context.Background(), []types.ToolCall{fetchPageCall("current", server.URL+"/current")})
	if len(current) != 1 || !strings.Contains(current[0].Output, "Current Asian Games source") {
		t.Fatalf("current source result = %#v", current)
	}
	if searchCalls != 1 || mcpCalls != 1 || hits["/stale-one"] != 1 || hits["/stale-two"] != 1 || hits["/current"] != 1 {
		t.Fatalf("scenario network counts: DDG=%d MCP=%d hits=%#v", searchCalls, mcpCalls, hits)
	}
}
