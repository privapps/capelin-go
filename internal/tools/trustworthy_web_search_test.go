package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQualityGateSearchResultsNormalizesFiltersAndDeduplicates(t *testing.T) {
	results := qualityGateSearchResults("trusted search", []searchResult{
		{Title: "Trusted Search", URL: "HTTPS://Example.com:443/docs#section", Abstract: "Trusted search documentation"},
		{Title: "Duplicate", URL: "https://example.com/docs#other", Abstract: "trusted search"},
		{Title: "", URL: "https://empty.example", Abstract: "trusted search"},
		{Title: "Bad scheme", URL: "javascript:alert(1)", Abstract: "trusted search"},
		{Title: "Trusted porn result", URL: "https://unsafe.example/porn", Abstract: "trusted search"},
		{Title: "Completely unrelated", URL: "https://unrelated.example", Abstract: "nothing relevant"},
	})

	if len(results) != 1 {
		t.Fatalf("quality gate accepted %d results %#v, want one", len(results), results)
	}
	if results[0].URL != "https://example.com/docs" {
		t.Fatalf("normalized URL = %q, want canonical HTTPS URL", results[0].URL)
	}
}

func TestQualityGateSearchResultsRejectsQueriesWithoutMeaningfulTerms(t *testing.T) {
	results := qualityGateSearchResults("the and", []searchResult{{
		Title:    "The official result",
		URL:      "https://example.com/result",
		Abstract: "A valid-looking record",
	}})
	if len(results) != 0 {
		t.Fatalf("stopword-only query accepted %#v, want no trustworthy results", results)
	}
}

func TestQualityGateSearchResultsAcceptsTechnicalAndInflectedMatches(t *testing.T) {
	for _, test := range []struct {
		query  string
		result searchResult
	}{
		{query: "C++", result: searchResult{Title: "C++ programming guide", URL: "https://example.com/cpp"}},
		{query: "run benchmarks", result: searchResult{Title: "Running Benchmarks", URL: "https://example.com/benchmarks", Abstract: "How to run useful benchmarks"}},
	} {
		results := qualityGateSearchResults(test.query, []searchResult{test.result})
		if len(results) != 1 {
			t.Errorf("query %q rejected legitimate result: %#v", test.query, results)
		}
	}
}

func TestQualityGateSearchResultsRejectsTechnicalPrefixFalsePositives(t *testing.T) {
	results := qualityGateSearchResults("C++", []searchResult{{
		Title: "Cooking recipes",
		URL:   "https://example.com/cooking",
	}})
	if len(results) != 0 {
		t.Fatalf("unrelated C++ result accepted: %#v", results)
	}
}

func TestQualityGateSearchResultsKeepsLegitimateAmbiguousSafetyTerms(t *testing.T) {
	results := qualityGateSearchResults("nude mice cancer", []searchResult{{
		Title:    "Nude mice in cancer research",
		URL:      "https://example.com/research",
		Abstract: "A scientific study",
	}})
	if len(results) != 1 {
		t.Fatalf("legitimate ambiguous result rejected: %#v", results)
	}
}

func TestQualityGateSearchResultsRejectsExplicitAdultDomains(t *testing.T) {
	for _, host := range []string{"pornhub.com", "xvideos.com", "xhamster.com", "xnxx.com"} {
		results := qualityGateSearchResults("adult videos", []searchResult{{
			Title: "Adult videos",
			URL:   "https://" + host + "/watch/example",
		}})
		if len(results) != 0 {
			t.Errorf("explicit adult domain %q accepted: %#v", host, results)
		}
	}
}

func TestParseDDGResultsSkipsResultWrappers(t *testing.T) {
	results, err := parseDDGResults(strings.NewReader(`<div class="results"><div class="result web-result"><a class="result__a" href="https://example.com/result">Trusted Search</a><div class="result__snippet">trusted search evidence</div></div></div>`))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "Trusted Search" {
		t.Fatalf("parsed DDG results = %#v, want one child result", results)
	}
}

func TestRunWebSearchUsesDuckDuckGoAndStrictSafeSearch(t *testing.T) {
	oldClient, oldDDG, oldMCP := toolHTTPClient, ddgSearchURL, mcpSearchURL
	t.Cleanup(func() {
		toolHTTPClient, ddgSearchURL, mcpSearchURL = oldClient, oldDDG, oldMCP
	})

	mcpCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			mcpCalled = true
			t.Fatal("MCP fallback was called after a trustworthy DuckDuckGo response")
		}
		if r.Method != http.MethodPost {
			t.Fatalf("DuckDuckGo method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("kp") != "1" {
			t.Fatalf("DuckDuckGo safe-search parameter kp = %q, want 1", r.Form.Get("kp"))
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://Example.com:443/docs#fragment">Trusted Search</a><div class="result__snippet">Trusted search documentation</div></div>`))
	}))
	defer server.Close()

	toolHTTPClient = &http.Client{}
	ddgSearchURL = server.URL + "/ddg"
	mcpSearchURL = server.URL + "/mcp"
	got, err := runWebSearch(context.Background(), "trusted search")
	if err != nil {
		t.Fatal(err)
	}
	if mcpCalled {
		t.Fatal("MCP fallback was called unexpectedly")
	}
	for _, want := range []string{"Search provider: DuckDuckGo", "Fallback: no", "Trusted Search", "https://example.com/docs"} {
		if !strings.Contains(got, want) {
			t.Errorf("search output %q does not contain %q", got, want)
		}
	}
}

func TestRunWebSearchFallsBackAfterQualityRejection(t *testing.T) {
	oldClient, oldDDG, oldMCP := toolHTTPClient, ddgSearchURL, mcpSearchURL
	t.Cleanup(func() {
		toolHTTPClient, ddgSearchURL, mcpSearchURL = oldClient, oldDDG, oldMCP
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Trusted porn result</a><div class="result__snippet">trusted search</div></div><div class="result web-result"><a class="result__a" href="javascript:bad">Trusted malformed result</a><div class="result__snippet">trusted search</div></div>`))
		case "/mcp":
			if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json, text/event-stream" {
				t.Fatalf("unexpected MCP request: method=%s accept=%q", r.Method, r.Header.Get("Accept"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Trusted Search Result\",\"url\":\"https://trusted.example/result\",\"snippet\":\"Useful trusted search evidence\"}]}"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	toolHTTPClient = &http.Client{}
	ddgSearchURL = server.URL + "/ddg"
	mcpSearchURL = server.URL + "/mcp"
	got, err := runWebSearch(context.Background(), "trusted search")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: Parallel", "Fallback: yes", "Fallback reason: DuckDuckGo returned no trustworthy results", "Trusted Search Result"} {
		if !strings.Contains(got, want) {
			t.Errorf("search output %q does not contain %q", got, want)
		}
	}
	for _, rejected := range []string{"Trusted porn result", "unsafe.example", "Trusted malformed result", "javascript:bad"} {
		if strings.Contains(got, rejected) {
			t.Errorf("search output exposed rejected content %q: %q", rejected, got)
		}
	}
}

func TestRunWebSearchReturnsExplicitNoTrustworthyResults(t *testing.T) {
	oldClient, oldDDG, oldMCP := toolHTTPClient, ddgSearchURL, mcpSearchURL
	t.Cleanup(func() {
		toolHTTPClient, ddgSearchURL, mcpSearchURL = oldClient, oldDDG, oldMCP
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ddg" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Polluted porn result</a><div class="result__snippet">trusted search</div></div>`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Unrelated spam result\",\"url\":\"https://spam.example/buy-now\",\"snippet\":\"nothing relevant\"}]}"}]}}`))
	}))
	defer server.Close()

	toolHTTPClient = &http.Client{}
	ddgSearchURL = server.URL + "/ddg"
	mcpSearchURL = server.URL + "/mcp"
	got, err := runWebSearch(context.Background(), "trusted search")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: none", "Search status: no trustworthy results", "Rejected results are omitted.", "DuckDuckGo returned no trustworthy results", "Parallel returned no trustworthy results"} {
		if !strings.Contains(got, want) {
			t.Errorf("no-results output %q does not contain %q", got, want)
		}
	}
	for _, rejected := range []string{"Polluted porn result", "unsafe.example", "trusted search", "Unrelated spam result", "spam.example", "nothing relevant"} {
		if strings.Contains(got, rejected) {
			t.Errorf("no-results output exposed rejected content %q: %q", rejected, got)
		}
	}
}

func TestNormalizeSearchURLRejectsUnsupportedURLs(t *testing.T) {
	for _, raw := range []string{"", "javascript:alert(1)", "file:///tmp/result", "https://", "https://user:pass@example.com"} {
		if _, err := normalizeSearchURL(raw); err == nil {
			t.Errorf("normalizeSearchURL(%q) succeeded, want rejection", raw)
		}
	}
	if got, err := normalizeSearchURL("HTTP://Example.com:080/path/./child/../#fragment"); err != nil || got != "http://example.com/path/" {
		t.Errorf("normalizeSearchURL canonical result = %q, err = %v", got, err)
	}
}

func TestQualityGateLongQueryRequiresEnoughAnchorEvidence(t *testing.T) {
	query := "2026 Asian Games men's football final venue schedule medal results October broadcast timezone"
	results := qualityGateSearchResults(query, []searchResult{{
		Title:    "Asian Games schedule",
		URL:      "https://example.com/medal-schedule",
		Abstract: "Official medal results for the October 2026 final broadcast.",
	}})
	if len(results) != 0 {
		t.Fatalf("result matching only broad and optional query terms was accepted: %#v", results)
	}
}
