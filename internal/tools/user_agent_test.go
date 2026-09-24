package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"capelin-go/internal/contracts"
)

func TestCapelinUserAgentOnSearchAndFetchRedirects(t *testing.T) {
	oldClient, oldDDG, oldMCP := toolHTTPClient, ddgSearchURL, mcpSearchURL
	defer func() {
		toolHTTPClient, ddgSearchURL, mcpSearchURL = oldClient, oldDDG, oldMCP
	}()

	var paths []string
	var userAgents []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		userAgents = append(userAgents, r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/ddg":
			w.Header().Set("Location", "/ddg-results")
			w.WriteHeader(http.StatusFound)
		case "/ddg-results":
			_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://example.com">Example</a><div class="result__snippet">A result</div></div>`))
		case "/mcp":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Example\",\"url\":\"https://example.com\",\"snippet\":\"A result\"}]}"}]}}`))
		case "/fetch":
			w.Header().Set("Location", "/fetch-results")
			w.WriteHeader(http.StatusFound)
		case "/fetch-results":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><p>Fetched page</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{}
	SetNetworkOverrides(true, client, server.URL+"/ddg", server.URL+"/mcp")
	if _, err := runDuckDuckGoSearch(context.Background(), "query"); err != nil {
		t.Fatal(err)
	}
	provider, err := configuredMCPProvider(SearchProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Search(context.Background(), "query", client); err != nil {
		t.Fatal(err)
	}
	if _, err := runFetchPageWithClient(withPrivateFetchOverride(context.Background(), true), server.URL+"/fetch", client); err != nil {
		t.Fatal(err)
	}

	wantPaths := []string{"/ddg", "/ddg-results", "/mcp", "/fetch", "/fetch-results"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("request paths = %#v, want %#v", paths, wantPaths)
	}
	for i, userAgent := range userAgents {
		if userAgent != contracts.CapelinUserAgent {
			t.Errorf("request %d user agent = %q, want %q", i, userAgent, contracts.CapelinUserAgent)
		}
	}
}
