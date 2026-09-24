package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQualityGateSearchResultsNormalizesFiltersAndDeduplicates(t *testing.T) {
	results, report := qualityGateSearchResults("trusted search", []searchResult{
		{Title: "Trusted Search", URL: "HTTPS://Example.com:443/docs#section", Abstract: "Trusted search documentation"},
		{Title: "Duplicate", URL: "https://example.com/docs#other", Abstract: "trusted search"},
		{Title: "", URL: "https://empty.example", Abstract: "trusted search"},
		{Title: "Bad scheme", URL: "javascript:alert(1)", Abstract: "trusted search"},
		{Title: "Trusted porn result", URL: "https://unsafe.example/porn", Abstract: "trusted search"},
		{Title: "Completely unrelated", URL: "https://unrelated.example", Abstract: "nothing relevant"},
	})

	if report.accepted != 1 || len(results) != 1 {
		t.Fatalf("quality gate accepted %d results %#v, want one", report.accepted, results)
	}
	if results[0].URL != "https://example.com/docs" {
		t.Fatalf("normalized URL = %q, want canonical HTTPS URL", results[0].URL)
	}
}

func TestQualityGateSearchResultsRejectsQueriesWithoutMeaningfulTerms(t *testing.T) {
	results, report := qualityGateSearchResults("the and", []searchResult{{
		Title:    "The official result",
		URL:      "https://example.com/result",
		Abstract: "A valid-looking record",
	}})
	if report.accepted != 0 || len(results) != 0 {
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
		results, report := qualityGateSearchResults(test.query, []searchResult{test.result})
		if report.accepted != 1 || len(results) != 1 {
			t.Errorf("query %q rejected legitimate result: %#v", test.query, results)
		}
	}
}

func TestQualityGateSearchResultsRejectsTechnicalPrefixFalsePositives(t *testing.T) {
	results, report := qualityGateSearchResults("C++", []searchResult{{
		Title: "Cooking recipes",
		URL:   "https://example.com/cooking",
	}})
	if report.accepted != 0 || len(results) != 0 {
		t.Fatalf("unrelated C++ result accepted: %#v", results)
	}
}

func TestQualityGateSearchResultsKeepsLegitimateAmbiguousSafetyTerms(t *testing.T) {
	results, report := qualityGateSearchResults("nude mice cancer", []searchResult{{
		Title:    "Nude mice in cancer research",
		URL:      "https://example.com/research",
		Abstract: "A scientific study",
	}})
	if report.accepted != 1 || len(results) != 1 {
		t.Fatalf("legitimate ambiguous result rejected: %#v", results)
	}
}

func TestQualityGateSearchResultsRejectsExplicitAdultDomains(t *testing.T) {
	for _, host := range []string{"pornhub.com", "xvideos.com", "xhamster.com", "xnxx.com"} {
		results, report := qualityGateSearchResults("adult videos", []searchResult{{
			Title: "Adult videos",
			URL:   "https://" + host + "/watch/example",
		}})
		if report.accepted != 0 || len(results) != 0 {
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
	oldClient, oldDDG, oldBing := toolHTTPClient, ddgSearchURL, bingSearchURL
	t.Cleanup(func() {
		toolHTTPClient, ddgSearchURL, bingSearchURL = oldClient, oldDDG, oldBing
	})

	bingCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bing" {
			bingCalled = true
			t.Fatal("Bing was called after a trustworthy DuckDuckGo response")
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
	bingSearchURL = server.URL + "/bing"
	got, err := runWebSearch(context.Background(), "trusted search")
	if err != nil {
		t.Fatal(err)
	}
	if bingCalled {
		t.Fatal("Bing was called unexpectedly")
	}
	for _, want := range []string{"Search provider: DuckDuckGo", "Fallback: no", "Trusted Search", "https://example.com/docs"} {
		if !strings.Contains(got, want) {
			t.Errorf("search output %q does not contain %q", got, want)
		}
	}
}

func TestRunWebSearchFallsBackAfterQualityRejection(t *testing.T) {
	oldClient, oldDDG, oldBing := toolHTTPClient, ddgSearchURL, bingSearchURL
	t.Cleanup(func() {
		toolHTTPClient, ddgSearchURL, bingSearchURL = oldClient, oldDDG, oldBing
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Trusted porn result</a><div class="result__snippet">trusted search</div></div><div class="result web-result"><a class="result__a" href="javascript:bad">Trusted malformed result</a><div class="result__snippet">trusted search</div></div>`))
		case "/bing":
			if r.URL.Query().Get("adlt") != "strict" {
				t.Fatalf("Bing adlt = %q, want strict", r.URL.Query().Get("adlt"))
			}
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(`<rss><channel><item><title>Trusted Search Result</title><link>https://trusted.example/result</link><description>Useful trusted search evidence</description></item></channel></rss>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	toolHTTPClient = &http.Client{}
	ddgSearchURL = server.URL + "/ddg"
	bingSearchURL = server.URL + "/bing"
	got, err := runWebSearch(context.Background(), "trusted search")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: Bing", "Fallback: yes", "Fallback reason: DuckDuckGo returned no trustworthy results", "Trusted Search Result"} {
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
	oldClient, oldDDG, oldBing := toolHTTPClient, ddgSearchURL, bingSearchURL
	t.Cleanup(func() {
		toolHTTPClient, ddgSearchURL, bingSearchURL = oldClient, oldDDG, oldBing
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ddg" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Polluted porn result</a><div class="result__snippet">trusted search</div></div>`))
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<rss><channel><item><title>Unrelated spam result</title><link>https://spam.example/buy-now</link><description>nothing relevant</description></item></channel></rss>`))
	}))
	defer server.Close()

	toolHTTPClient = &http.Client{}
	ddgSearchURL = server.URL + "/ddg"
	bingSearchURL = server.URL + "/bing"
	got, err := runWebSearch(context.Background(), "trusted search")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search provider: none", "Search status: no trustworthy results", "Rejected results are omitted.", "DuckDuckGo returned no trustworthy results", "Bing returned no trustworthy results"} {
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
