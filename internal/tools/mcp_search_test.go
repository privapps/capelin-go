package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfiguredMCPProviderSelectsParallelAndExa(t *testing.T) {
	parallel, err := configuredMCPProvider(SearchProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if parallel.Name() != searchProviderParallel {
		t.Fatalf("default provider = %q, want Parallel", parallel.Name())
	}

	exa, err := configuredMCPProvider(SearchProviderConfig{Provider: SearchProviderExa})
	if err != nil {
		t.Fatal(err)
	}
	selected := exa.(mcpSearchProvider)
	if selected.Name() != searchProviderExa || selected.endpoint != exaSearchEndpoint || selected.toolName != "web_search_exa" {
		t.Fatalf("Exa provider = %#v", selected)
	}
	if _, err := configuredMCPProvider(SearchProviderConfig{Provider: "unknown"}); err == nil {
		t.Fatal("unknown provider was accepted")
	}
}

func TestMCPProviderSendsCredentiallessAndAuthenticatedRequests(t *testing.T) {
	oldURL := mcpSearchURL
	t.Cleanup(func() { mcpSearchURL = oldURL })

	var requests []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Clone())
		var request struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Params.Name == "web_search" && request.Params.Arguments["objective"] != "mcp query" {
			t.Fatalf("Parallel arguments = %#v", request.Params.Arguments)
		}
		if request.Params.Name == "web_search_exa" && request.Params.Arguments["query"] != "mcp query" {
			t.Fatalf("Exa arguments = %#v", request.Params.Arguments)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"MCP query result\",\"url\":\"https://example.com/result\",\"snippet\":\"mcp query evidence\"}]}"}]}}`))
	}))
	defer server.Close()
	mcpSearchURL = server.URL

	parallel, err := configuredMCPProvider(SearchProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parallel.Search(context.Background(), "mcp query", &http.Client{}); err != nil {
		t.Fatal(err)
	}
	exa, err := configuredMCPProvider(SearchProviderConfig{Provider: SearchProviderExa, ExaAPIKey: "exa-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exa.Search(context.Background(), "mcp query", &http.Client{}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if got := requests[0].Get("Authorization"); got != "" || requests[0].Get("x-api-key") != "" {
		t.Fatalf("credentialless request contained credentials: %#v", requests[0])
	}
	if got := requests[1].Get("x-api-key"); got != "exa-secret" || requests[1].Get("Authorization") != "" {
		t.Fatalf("Exa credential headers = %#v", requests[1])
	}
}

func TestParseMCPSearchResponseSupportsJSONAndSSE(t *testing.T) {
	jsonResponse := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Parallel result\",\"url\":\"https://example.com/parallel\",\"description\":\"parallel query evidence\"}]}"}]}}`)
	results, err := parseMCPSearchResponse(jsonResponse)
	if err != nil || len(results) != 1 || results[0].Title != "Parallel result" {
		t.Fatalf("JSON results = %#v, err = %v", results, err)
	}

	sse := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"Title: Exa result\\nURL: https://example.com/exa\\nHighlights: exa query evidence\"}]}}\n\n"
	results, err = parseMCPSearchResponse([]byte(sse))
	if err != nil || len(results) != 1 || results[0].Title != "Exa result" || results[0].Abstract != "exa query evidence" {
		t.Fatalf("SSE results = %#v, err = %v", results, err)
	}
}

func TestParseMCPSearchResponseRejectsMalformedEmptyAndToolErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "malformed json", raw: []byte(`{"jsonrpc":"2.0"`)},
		{name: "malformed sse", raw: []byte("data: {not-json}\n\n")},
		{name: "json rpc error", raw: []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"upstream failure"}}`)},
		{name: "tool error result", raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"do not use this"}]}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results, err := parseMCPSearchResponse(test.raw)
			if err == nil || len(results) != 0 {
				t.Fatalf("results = %#v, err = %v; want an error and no results", results, err)
			}
		})
	}
}

func TestParseMCPSearchResponseDoesNotConfuseJSONTextWithSSE(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Metadata result\",\"url\":\"https://example.com/data\",\"description\":\"metadata: data: is ordinary text\"}]}"}]}}`)
	results, err := parseMCPSearchResponse(raw)
	if err != nil || len(results) != 1 || results[0].Title != "Metadata result" {
		t.Fatalf("JSON containing data: parsed as %#v, err = %v", results, err)
	}
}

func TestParseMCPTextDoesNotTreatMarkdownAsAResultRecord(t *testing.T) {
	results, err := parseMCPText("[Untrusted prose](https://example.com)\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("markdown prose was accepted as results: %#v", results)
	}
}

func TestMCPProviderRedactsFailuresAndBoundsResponse(t *testing.T) {
	oldURL := mcpSearchURL
	t.Cleanup(func() { mcpSearchURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("provider-secret-and-cookie"))
	}))
	defer server.Close()
	mcpSearchURL = server.URL
	provider, err := configuredMCPProvider(SearchProviderConfig{ParallelAPIKey: "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Search(context.Background(), "query", &http.Client{})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("provider error leaked sensitive content: %v", err)
	}

	tooLarge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxMCPResponseBytes+1)))
	}))
	defer tooLarge.Close()
	mcpSearchURL = tooLarge.URL
	provider, err = configuredMCPProvider(SearchProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Search(context.Background(), "query", &http.Client{})
	if err == nil || !strings.Contains(err.Error(), "response limit") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestMCPProviderHonorsCancellation(t *testing.T) {
	oldURL := mcpSearchURL
	t.Cleanup(func() { mcpSearchURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	mcpSearchURL = server.URL
	provider, err := configuredMCPProvider(SearchProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = provider.Search(ctx, "query", &http.Client{})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("cancellation error = %v, duration = %s", err, time.Since(started))
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context error = %v", ctx.Err())
	}
}

func TestMCPProviderDoesNotFollowRedirectsWithCredentialsOrSearchBody(t *testing.T) {
	oldURL := mcpSearchURL
	t.Cleanup(func() { mcpSearchURL = oldURL })

	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect forwarded authorization header %q", got)
		}
		if r.Method != http.MethodGet {
			t.Errorf("redirect forwarded search method %q", r.Method)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	mcpSearchURL = redirect.URL

	provider, err := configuredMCPProvider(SearchProviderConfig{ParallelAPIKey: "provider-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Search(context.Background(), "private search query", &http.Client{})
	if err == nil {
		t.Fatal("provider redirect was followed; expected the redirect response to be rejected")
	}
	if forwarded {
		t.Fatal("search query or credential was sent to the redirect target")
	}
}

func TestSearchBatchDuplicateWaiterReleasesEarlierReservation(t *testing.T) {
	oldDDG, oldMCP := ddgSearchURL, mcpSearchURL
	t.Cleanup(func() {
		ddgSearchURL = oldDDG
		mcpSearchURL = oldMCP
	})

	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ddg" {
			http.NotFound(w, r)
			return
		}
		requests <- struct{}{}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://example.com/duplicate">duplicate batch query</a><div class="result__snippet">duplicate batch query evidence</div></div>`))
	}))
	defer server.Close()
	ddgSearchURL = server.URL + "/ddg"
	mcpSearchURL = server.URL + "/mcp"

	task := NewSearchTask()
	batch := NewSearchBatch(2)
	query := "duplicate batch query"
	type outcome struct {
		result string
		err    error
	}
	ownerDone := make(chan outcome, 1)
	go func() {
		result, err := task.searchWithBatch(context.Background(), query, server.Client(), SearchProviderConfig{}, batch, 1)
		ownerDone <- outcome{result: result, err: err}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		task.mu.Lock()
		_, ownerCreated := task.flights[normalizeSearchQuery(query)]
		task.mu.Unlock()
		if ownerCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("later batch owner did not create its in-flight query")
		}
		time.Sleep(time.Millisecond)
	}

	waiterCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	waiterDone := make(chan outcome, 1)
	go func() {
		result, err := task.searchWithBatch(waiterCtx, query, server.Client(), SearchProviderConfig{}, batch, 0)
		waiterDone <- outcome{result: result, err: err}
	}()

	select {
	case waiter := <-waiterDone:
		if waiter.err != nil {
			t.Fatalf("duplicate waiter failed: %v", waiter.err)
		}
		if waiter.result == "" {
			t.Fatal("duplicate waiter returned an empty result")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate waiter deadlocked behind the later batch owner")
	}

	select {
	case owner := <-ownerDone:
		if owner.err != nil {
			t.Fatalf("batch owner failed: %v", owner.err)
		}
	case <-time.After(time.Second):
		t.Fatal("batch owner did not finish after duplicate waiter released its turn")
	}

	select {
	case <-requests:
	default:
		t.Fatal("batch owner did not invoke the primary provider")
	}
	select {
	case <-requests:
		t.Fatal("duplicate query invoked the primary provider twice")
	default:
	}
}
