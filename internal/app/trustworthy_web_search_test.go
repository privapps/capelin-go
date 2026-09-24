package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"capelin-go/internal/tools"
	"capelin-go/internal/types"
)

type failingSearchBody struct{}

func (failingSearchBody) Read([]byte) (int, error) { return 0, errors.New("fixture body read failed") }
func (failingSearchBody) Close() error             { return nil }

type failingSearchTransport struct{}

func (failingSearchTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("global search client must not be used")
}

type searchFixtureTransport struct {
	transportError bool
	parserError    bool
}

func (t searchFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/ddg" {
		return http.DefaultTransport.RoundTrip(req)
	}
	if t.transportError {
		return nil, errors.New("fixture transport failed")
	}
	if t.parserError {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       failingSearchBody{},
		}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestRunToolWebSearchRegressionMatrix(t *testing.T) {
	type searchCase struct {
		name              string
		ddgStatus         int
		ddgTransportError bool
		ddgParserError    bool
		bingStatus        int
		ddgBody           string
		bingBody          string
		wantProvider      string
		wantFallbackText  string
		wantSummary       string
		wantPresent       []string
		wantAbsent        []string
	}
	cases := []searchCase{
		{
			name:         "primary success",
			ddgBody:      `<div class="results"><div class="result web-result"><a class="result__a" href="https://primary.example/result">Trusted Search</a><div class="result__snippet">trusted search evidence</div></div></div>`,
			bingBody:     `<rss><channel><item><title>Must not be used</title><link>https://bing.example/result</link></item></channel></rss>`,
			wantProvider: "Search provider: DuckDuckGo",
			wantSummary:  "Trusted Search",
			wantPresent:  []string{"Fallback: no", "Trusted Search", "trusted search evidence"},
			wantAbsent:   []string{"Must not be used", "bing.example"},
		},
		{
			name:              "transport fallback",
			ddgTransportError: true,
			bingBody:          `<rss><channel><item><title>Bing Trusted Search</title><link>https://bing.example/result</link><description>trusted search evidence</description></item></channel></rss>`,
			wantProvider:      "Search provider: Bing",
			wantFallbackText:  "DuckDuckGo request or response failed",
			wantSummary:       "Bing Trusted Search",
			wantPresent:       []string{"Fallback: yes", "Bing Trusted Search"},
		},
		{
			name:             "HTTP fallback",
			ddgStatus:        http.StatusBadGateway,
			bingBody:         `<rss><channel><item><title>Bing Trusted Search</title><link>https://bing.example/http-fallback</link><description>trusted search evidence</description></item></channel></rss>`,
			wantProvider:     "Search provider: Bing",
			wantFallbackText: "DuckDuckGo request or response failed",
			wantSummary:      "Bing Trusted Search",
			wantPresent:      []string{"Fallback: yes", "Bing Trusted Search"},
		},
		{
			name:             "parser fallback",
			ddgParserError:   true,
			bingBody:         `<rss><channel><item><title>Bing Trusted Search</title><link>https://bing.example/parser-fallback</link><description>trusted search evidence</description></item></channel></rss>`,
			wantProvider:     "Search provider: Bing",
			wantFallbackText: "DuckDuckGo request or response failed",
			wantSummary:      "Bing Trusted Search",
			wantPresent:      []string{"Fallback: yes", "Bing Trusted Search"},
		},
		{
			name:             "empty HTTP success fallback",
			ddgBody:          `<html><body>provider returned no result records</body></html>`,
			bingBody:         `<rss><channel><item><title>Bing Trusted Search</title><link>https://bing.example/empty-fallback</link><description>trusted search evidence</description></item></channel></rss>`,
			wantProvider:     "Search provider: Bing",
			wantFallbackText: "DuckDuckGo returned no results",
			wantSummary:      "Bing Trusted Search",
			wantPresent:      []string{"Fallback: yes", "Bing Trusted Search"},
		},
		{
			name:             "empty record fallback",
			ddgBody:          `<div class="result web-result"><span>empty provider record</span></div>`,
			bingBody:         `<rss><channel><item><title>Bing Trusted Search</title><link>https://bing.example/empty-record</link><description>trusted search evidence</description></item></channel></rss>`,
			wantProvider:     "Search provider: Bing",
			wantFallbackText: "DuckDuckGo returned no results",
			wantSummary:      "Bing Trusted Search",
			wantPresent:      []string{"Fallback: yes", "Bing Trusted Search", "trusted search evidence"},
		},
		{
			name:         "mixed results malformed and duplicate records",
			ddgBody:      `<div class="result web-result"><a class="result__a" href="https://mixed.example/result#first">Trusted Search</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="https://mixed.example/result#duplicate">Duplicate Trusted Search</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="javascript:bad">Malformed Trusted Search</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Trusted porn result</a><div class="result__snippet">trusted search evidence</div></div>`,
			bingBody:     `<rss><channel><item><title>Must not be used</title><link>https://bing.example/result</link></item></channel></rss>`,
			wantProvider: "Search provider: DuckDuckGo",
			wantSummary:  "Trusted Search",
			wantPresent:  []string{"Fallback: no", "Trusted Search", "https://mixed.example/result"},
			wantAbsent:   []string{"Duplicate Trusted Search", "Malformed Trusted Search", "javascript:bad", "Trusted porn result", "unsafe.example", "Must not be used"},
		},
		{
			name:             "rejected first duplicate does not get accepted",
			ddgBody:          `<div class="result web-result"><a class="result__a" href="https://duplicate.example/result#unsafe">Trusted porn result</a><div class="result__snippet">trusted search evidence</div></div><div class="result web-result"><a class="result__a" href="https://duplicate.example/result#clean">Clean Duplicate</a><div class="result__snippet">trusted search evidence</div></div>`,
			bingBody:         `<rss><channel><item><title>Bing Recovery</title><link>https://bing.example/recovery</link><description>trusted search evidence</description></item></channel></rss>`,
			wantProvider:     "Search provider: Bing",
			wantFallbackText: "DuckDuckGo returned no trustworthy results",
			wantSummary:      "Bing Recovery",
			wantPresent:      []string{"Fallback: yes", "Bing Recovery"},
			wantAbsent:       []string{"Clean Duplicate", "duplicate.example"},
		},
		{
			name:             "both providers rejected",
			ddgBody:          `<div class="result web-result"><a class="result__a" href="https://unsafe.example/porn">Polluted porn result</a><div class="result__snippet">trusted search evidence</div></div>`,
			bingBody:         `<rss><channel><item><title>Unrelated result</title><link>https://unrelated.example/result</link><description>nothing relevant</description></item></channel></rss>`,
			wantProvider:     "Search provider: none",
			wantFallbackText: "no trustworthy results",
			wantSummary:      "Search provider: none",
			wantPresent:      []string{"Search status: no trustworthy results", "Rejected results are omitted."},
			wantAbsent:       []string{"Polluted porn result", "unsafe.example", "trusted search evidence", "Unrelated result", "unrelated.example", "nothing relevant"},
		},
		{
			name:              "both providers unavailable",
			ddgTransportError: true,
			bingStatus:        http.StatusServiceUnavailable,
			wantProvider:      "Search provider: none",
			wantFallbackText:  "Bing request or response failed",
			wantSummary:       "Search provider: none",
			wantPresent:       []string{"Search status: no trustworthy results", "Rejected results are omitted."},
		},
		{
			name:              "Bing parser failure",
			ddgTransportError: true,
			bingBody:          `<rss><channel><item>`,
			wantProvider:      "Search provider: none",
			wantFallbackText:  "Bing request or response failed",
			wantSummary:       "Search provider: none",
			wantPresent:       []string{"Search status: no trustworthy results", "Rejected results are omitted."},
		},
	}

	oldClient, oldDDG, oldBing := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultBingURL()
	t.Cleanup(func() {
		tools.SetNetworkOverrides(false, oldClient, oldDDG, oldBing)
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bingCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ddg":
					if r.Method != http.MethodPost {
						t.Errorf("DuckDuckGo method = %s, want POST", r.Method)
					}
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Get("kp") != "1" {
						t.Errorf("DuckDuckGo kp = %q, want 1", r.Form.Get("kp"))
					}
					if tc.ddgStatus != 0 {
						http.Error(w, "upstream failure", tc.ddgStatus)
						return
					}
					w.Header().Set("Content-Type", "text/html")
					_, _ = fmt.Fprint(w, tc.ddgBody)
				case "/bing":
					bingCalled = true
					if r.URL.Query().Get("adlt") != "strict" {
						t.Errorf("Bing adlt = %q, want strict", r.URL.Query().Get("adlt"))
					}
					if tc.bingStatus != 0 {
						http.Error(w, "upstream failure", tc.bingStatus)
						return
					}
					w.Header().Set("Content-Type", "application/rss+xml")
					_, _ = fmt.Fprint(w, tc.bingBody)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			searchClient := &http.Client{}
			if tc.ddgTransportError || tc.ddgParserError {
				searchClient = &http.Client{Transport: searchFixtureTransport{
					transportError: tc.ddgTransportError,
					parserError:    tc.ddgParserError,
				}}
			}
			tools.SetNetworkOverrides(true, searchClient, server.URL+"/ddg", server.URL+"/bing")

			var observedToolOutput string
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
				for _, message := range request.Messages {
					if message.Role != "tool" {
						continue
					}
					if err := json.Unmarshal(message.Content, &observedToolOutput); err != nil {
						observedToolOutput = string(message.Content)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if modelCalls == 1 {
					_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"I need to search for trusted information","tool_calls":[{"id":"call-1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"trusted search\"}"}}]},"finish_reason":"tool_calls"}]}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The search is complete.","reasoning_content":"I checked the search result summary."},"finish_reason":"stop"}]}`)
			}))
			defer modelServer.Close()

			a := &app{
				cfg:     config{workspaceRoot: t.TempDir(), toolMaxParallel: 1, toolTimeoutSec: 30, allowedTools: map[string]bool{toolWebSearch: true}},
				client:  &client{endpoint: modelServer.URL + "/chat/completions", http: searchClient},
				toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
				sink:    &spySink{onToolResult: func(_ string, _ bool, detail string) { observedToolOutput = detail }},
			}
			_, _, reasoning, err := a.runTurnLoop(context.Background(), []types.Message{{Role: "system", Content: "test"}}, "search for trusted information", a.rootRuntime(), a.toolset, true)
			if err != nil {
				t.Fatal(err)
			}
			if modelCalls != 2 {
				t.Fatalf("model calls = %d, want tool turn and final turn", modelCalls)
			}
			result := observedToolOutput
			if result == "" {
				t.Fatal("model did not receive a web-search tool result")
			}
			if !strings.Contains(result, tc.wantProvider) {
				t.Errorf("tool output %q does not contain provider %q", result, tc.wantProvider)
			}
			if tc.wantFallbackText != "" && !strings.Contains(result, tc.wantFallbackText) {
				t.Errorf("tool output %q does not contain fallback detail %q", result, tc.wantFallbackText)
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(result, want) {
					t.Errorf("tool output %q does not contain %q", result, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(result, absent) {
					t.Errorf("tool output exposed rejected or unselected content %q: %q", absent, result)
				}
			}
			if !strings.Contains(reasoning, tc.wantSummary) {
				t.Errorf("reasoning summary %q does not contain %q", reasoning, tc.wantSummary)
			}
			if tc.wantSummary != "Search provider: none" && strings.Contains(reasoning, "Search provider:") {
				t.Errorf("provider provenance was treated as a result title in reasoning: %q", reasoning)
			}
			if tc.name == "primary success" && bingCalled {
				t.Error("Bing was called after a trustworthy DuckDuckGo response")
			}
		})
	}
}

func TestRunToolWebSearchUsesApplicationHTTPClient(t *testing.T) {
	oldClient, oldDDG, oldBing := tools.DefaultHTTPClient(), tools.DefaultDuckDuckGoURL(), tools.DefaultBingURL()
	t.Cleanup(func() {
		tools.SetNetworkOverrides(false, oldClient, oldDDG, oldBing)
	})

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bing" {
			t.Fatal("Bing was called after the application client returned DuckDuckGo results")
		}
		if r.Method != http.MethodPost || r.FormValue("kp") != "1" {
			t.Fatalf("unexpected DuckDuckGo request: method=%s kp=%q", r.Method, r.FormValue("kp"))
		}
		_, _ = fmt.Fprint(w, `<div class="result web-result"><a class="result__a" href="https://example.com/docs">Trusted Search</a><div class="result__snippet">trusted search evidence</div></div>`)
	}))
	defer searchServer.Close()

	tools.SetNetworkOverrides(true, &http.Client{Transport: failingSearchTransport{}}, searchServer.URL+"/ddg", searchServer.URL+"/bing")
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
