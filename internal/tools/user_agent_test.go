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
	oldClient, oldDDG, oldBing := toolHTTPClient, ddgSearchURL, bingSearchURL
	defer func() {
		toolHTTPClient, ddgSearchURL, bingSearchURL = oldClient, oldDDG, oldBing
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
		case "/bing":
			w.Header().Set("Location", "/bing-results")
			w.WriteHeader(http.StatusFound)
		case "/bing-results":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(`<rss><channel><item><title>Example</title><link>https://example.com</link><description>A result</description></item></channel></rss>`))
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
	SetNetworkOverrides(true, client, server.URL+"/ddg", server.URL+"/bing")
	if _, err := runDuckDuckGoSearch(context.Background(), "query"); err != nil {
		t.Fatal(err)
	}
	if _, err := runBingSearch(context.Background(), "query"); err != nil {
		t.Fatal(err)
	}
	if _, err := runFetchPageWithClient(withPrivateFetchOverride(context.Background(), true), server.URL+"/fetch", client); err != nil {
		t.Fatal(err)
	}

	wantPaths := []string{"/ddg", "/ddg-results", "/bing", "/bing-results", "/fetch", "/fetch-results"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("request paths = %#v, want %#v", paths, wantPaths)
	}
	for i, userAgent := range userAgents {
		if userAgent != contracts.CapelinUserAgent {
			t.Errorf("request %d user agent = %q, want %q", i, userAgent, contracts.CapelinUserAgent)
		}
	}
}
