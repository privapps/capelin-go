package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	browserUA        = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	toolTimeout      = 30 * time.Second
	maxPageChars     = 14000
	maxSearchResults = 12
	maxListEntries   = 300
	maxFileBytes     = 512 * 1024
	maxExecOutput    = 256 * 1024
)

var ddgSearchURL = "https://html.duckduckgo.com/html"
var bingSearchURL = "https://www.bing.com/search"
var allowPrivateFetch = false

var errListLimitReached = errors.New("list limit reached")

// safeDialer resolves the target hostname and validates every resolved IP against
// isBlockedAddr before opening the TCP connection. This prevents DNS-rebinding
// attacks where a public IP is returned during the pre-flight validateFetchURL
// check but a private IP is returned during the actual HTTP dial.
var safeDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

func safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !allowPrivateFetch {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			if ip, ok := netip.AddrFromSlice(a.IP); ok && isBlockedAddr(ip.Unmap()) {
				return nil, fmt.Errorf("fetch page: refusing private or local IP %q", a.IP)
			}
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("fetch page: no addresses resolved for %q", host)
		}
		// Connect using the first resolved IP to pin the address and prevent rebinding.
		resolvedAddr := net.JoinHostPort(addrs[0].IP.String(), port)
		return safeDialer.DialContext(ctx, network, resolvedAddr)
	}
	return safeDialer.DialContext(ctx, network, addr)
}

var toolHTTPClient = &http.Client{
	Timeout: toolTimeout,
	Transport: &http.Transport{
		DialContext:           safeDial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		req.Header.Set("User-Agent", browserUA)
		return nil
	},
}

type webSearchArgs struct {
	Query string `json:"query"`
}

type fetchPageArgs struct {
	URL string `json:"url"`
}

type listFilesArgs struct {
	Path string `json:"path"`
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type appendFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editFileArgs struct {
	Path   string `json:"path"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

type executeProgramArgs struct {
	Command        string   `json:"command"`
	Args           []string `json:"args"`
	Cwd            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type executeSkillArgs struct {
	Name           string   `json:"name"`
	Command        string   `json:"command"`
	Args           []string `json:"args"`
	Cwd            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type createSubagentArgs struct {
	Name               string   `json:"name"`
	Question           string   `json:"question"`
	AllowedTools       []string `json:"allowed_tools"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
	ExecutionMode      string   `json:"execution_mode"`
	OverflowMode       string   `json:"overflow_mode"`
	WaitTimeoutSeconds int      `json:"wait_timeout_seconds"`
}

type runSubagentArgs struct {
	ID             string `json:"id"`
	Wait           bool   `json:"wait"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ExecutionMode  string `json:"execution_mode"`
}

type awaitSubagentArgs struct {
	ID             string `json:"id"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type listSubagentsArgs struct {
	IncludeDescendants bool `json:"include_descendants"`
}

type readSubagentArgs struct {
	ID            string   `json:"id"`
	IDs           []string `json:"ids"`
	IncludeOutput *bool    `json:"include_output"`
}

type cancelSubagentArgs struct {
	ID string `json:"id"`
}

type searchResult struct {
	Title    string
	URL      string
	Abstract string
}

func specWebSearch() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolWebSearch,
			Description: "Search the web using DuckDuckGo with Bing fallback and return result titles, URLs, and abstracts.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "The search query"},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
	}
}

func specFetchPage() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolFetchPage,
			Description: "Fetch a URL and return content as markdown-like text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "Target URL"},
				},
				"required":             []string{"url"},
				"additionalProperties": false,
			},
		},
	}
}

func specListFiles() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolListFiles,
			Description: "List files and directories under the local workspace.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Relative path inside workspace (optional)"},
				},
				"additionalProperties": false,
			},
		},
	}
}

func specReadFile() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolReadFile,
			Description: "Read a file from local workspace with optional line range.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "Relative file path"},
					"start_line": map[string]any{"type": "integer", "description": "1-based start line"},
					"end_line":   map[string]any{"type": "integer", "description": "1-based end line"},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		},
	}
}

func specWriteFile() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolWriteFile,
			Description: "Write content to a file in local workspace (overwrites existing file).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Relative file path"},
					"content": map[string]any{"type": "string", "description": "Full file content to write"},
				},
				"required":             []string{"path", "content"},
				"additionalProperties": false,
			},
		},
	}
}

func specAppendFile() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolAppendFile,
			Description: "Append content to a file in local workspace.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Relative file path"},
					"content": map[string]any{"type": "string", "description": "Content to append"},
				},
				"required":             []string{"path", "content"},
				"additionalProperties": false,
			},
		},
	}
}

func specEditFile() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolEditFile,
			Description: "Replace an exact string in a file. Fails if old_str is not found or appears more than once.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Relative file path"},
					"old_str": map[string]any{"type": "string", "description": "Exact string to find (must appear exactly once)"},
					"new_str": map[string]any{"type": "string", "description": "Replacement string"},
				},
				"required":             []string{"path", "old_str", "new_str"},
				"additionalProperties": false,
			},
		},
	}
}

func specExecuteProgram() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolExecuteProgram,
			Description: "Execute a local program safely with explicit command and args.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "Program name or path"},
					"args": map[string]any{
						"type":        "array",
						"description": "Program arguments",
						"items":       map[string]any{"type": "string"},
					},
					"cwd":             map[string]any{"type": "string", "description": "Optional working directory relative to workspace root"},
					"timeout_seconds": map[string]any{"type": "integer", "description": "Timeout in seconds (default 30, max 120)"},
				},
				"required":             []string{"command"},
				"additionalProperties": false,
			},
		},
	}
}

func specExecuteSkill() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolExecuteSkill,
			Description: "Execute a command that is declared by a loaded skill.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string", "description": "Skill name"},
					"command": map[string]any{"type": "string", "description": "Command binary from the skill (for example: opencli)"},
					"args": map[string]any{
						"type":        "array",
						"description": "Command arguments",
						"items":       map[string]any{"type": "string"},
					},
					"cwd":             map[string]any{"type": "string", "description": "Optional working directory"},
					"timeout_seconds": map[string]any{"type": "integer", "description": "Timeout in seconds (default 30, max 120)"},
				},
				"required":             []string{"name", "command"},
				"additionalProperties": false,
			},
		},
	}
}

func specListSkills() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolListSkills,
			Description: "List loaded Claude-style skills discovered from skill directories.",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}
}

func specReadSkill() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolReadSkill,
			Description: "Read full SKILL.md content for a loaded skill by name.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Skill name"},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
	}
}

func specCreateSubagent() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolCreateSubagent,
			Description: "Create a worker subagent session with inherited-and-restricted tool policy. Does not start execution.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Optional subagent label"},
					"question": map[string]any{
						"type":        "string",
						"description": "Task prompt for the subagent",
					},
					"allowed_tools": map[string]any{
						"type":        "array",
						"description": "Optional restricted tool allowlist (must be subset of parent allowed tools)",
						"items":       map[string]any{"type": "string"},
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Execution timeout in seconds",
					},
					"execution_mode": map[string]any{
						"type":        "string",
						"description": `Execution mode: "sequential" (serialized, one at a time) or "parallel" (concurrent, respects --subagent-max-parallel limit). Use "parallel" when spawning multiple independent subagents.`,
					},
					"overflow_mode": map[string]any{
						"type":        "string",
						"description": `Behavior when parent is at --subagent-max-children: "wait_for_slot" (default, blocks until active child slot is free) or "fail_fast" (return error immediately).`,
					},
					"wait_timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Timeout for overflow_mode=wait_for_slot. Defaults to --subagent-timeout-seconds (300s unless configured).",
					},
				},
				"required":             []string{"question"},
				"additionalProperties": false,
			},
		},
	}
}

func specRunSubagent() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolRunSubagent,
			Description: "Start a created subagent. For parallel execution of multiple subagents: call run_subagent with wait=false for ALL subagents first (non-blocking fire), then call await_subagent for each to collect results. Use wait=true only when running a single subagent or intentionally serializing.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Subagent id"},
					"wait": map[string]any{
						"type":        "boolean",
						"description": "Block until subagent completes. Set to false when launching multiple subagents in parallel — fire all with wait=false, then await_subagent each.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional wait timeout when wait=true",
					},
					"execution_mode": map[string]any{
						"type":        "string",
						"description": `Scheduling mode override: "sequential" or "parallel"`,
					},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
	}
}

func specAwaitSubagent() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolAwaitSubagent,
			Description: "Wait for a running subagent to finish and return its result envelope.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Subagent id"},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional wait timeout in seconds",
					},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
	}
}

func specListSubagents() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolListSubagents,
			Description: "List subagents visible to the current agent.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include_descendants": map[string]any{
						"type":        "boolean",
						"description": "Include descendant subagents, not only direct children",
					},
				},
				"additionalProperties": false,
			},
		},
	}
}

func specReadSubagent() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolReadSubagent,
			Description: "Read one subagent envelope or aggregate multiple subagent results by ids.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Single subagent id"},
					"ids": map[string]any{
						"type":        "array",
						"description": "Multiple subagent ids for aggregate fan-in read",
						"items":       map[string]any{"type": "string"},
					},
					"include_output": map[string]any{
						"type":        "boolean",
						"description": "When false, omit output payloads",
					},
				},
				"additionalProperties": false,
			},
		},
	}
}

func specCancelSubagent() apiTool {
	return apiTool{
		Type: "function",
		Function: apiToolSpec{
			Name:        toolCancelSubagent,
			Description: "Cancel a pending or running subagent.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Subagent id"},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
	}
}

func runWebSearch(ctx context.Context, query string) (string, error) {
	results, err := runDuckDuckGoSearch(ctx, query)
	if err == nil {
		return formatSearchResults(results), nil
	}

	bingResults, bingErr := runBingSearch(ctx, query)
	if bingErr == nil {
		return formatSearchResults(bingResults), nil
	}

	if errors.Is(err, errNoSearchResults) && errors.Is(bingErr, errNoSearchResults) {
		return "(no results)", nil
	}
	return "", fmt.Errorf("web search failed: duckduckgo: %v; bing: %w", err, bingErr)
}

var errNoSearchResults = errors.New("no search results")

func runDuckDuckGoSearch(ctx context.Context, query string) ([]searchResult, error) {
	form := url.Values{}
	form.Set("q", query)
	form.Set("b", "")
	form.Set("df", "")
	form.Set("kf", "-1")
	form.Set("kh", "1")
	form.Set("kl", "us-en")
	form.Set("kp", "1")
	form.Set("k1", "-1")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ddgSearchURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("web search: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("DNT", "1")

	resp, err := toolHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("web search returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	results, err := parseDDGResults(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, errNoSearchResults
	}
	if len(results) > maxSearchResults {
		results = results[:maxSearchResults]
	}
	return results, nil
}

func runBingSearch(ctx context.Context, query string) ([]searchResult, error) {
	endpoint, err := url.Parse(bingSearchURL)
	if err != nil {
		return nil, fmt.Errorf("bing search: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("setlang", "en-US")
	params.Set("mkt", "en-US")
	params.Set("format", "rss")
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("bing search: %w", err)
	}
	req.Header.Set("User-Agent", browserUA)

	resp, err := toolHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bing search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("bing search returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	results, err := parseBingRSSResults(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, errNoSearchResults
	}
	if len(results) > maxSearchResults {
		results = results[:maxSearchResults]
	}
	return results, nil
}

func formatSearchResults(results []searchResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		title := r.Title
		if title == "" {
			title = "(no title)"
		}
		u := r.URL
		if u == "" {
			u = "(no url)"
		}
		fmt.Fprintf(&b, "%d. **%s**\n   URL: %s\n   %s", i+1, title, u, r.Abstract)
	}
	if b.Len() == 0 {
		return "(no results)"
	}
	return b.String()
}

func parseDDGResults(r io.Reader) ([]searchResult, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parsing search results: %w", err)
	}

	var results []searchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" {
			cls := htmlAttr(n, "class")
			if strings.Contains(cls, "result") && strings.Contains(cls, "web-result") {
				if r := extractDDGResult(n); r != nil {
					results = append(results, *r)
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return results, nil
}

type bingRSSFeed struct {
	Channel struct {
		Items []bingRSSItem `xml:"item"`
	} `xml:"channel"`
}

type bingRSSItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
}

func parseBingRSSResults(r io.Reader) ([]searchResult, error) {
	var feed bingRSSFeed
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("parsing bing rss: %w", err)
	}

	results := make([]searchResult, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		if strings.TrimSpace(item.Title) == "" && strings.TrimSpace(item.Link) == "" {
			continue
		}
		results = append(results, searchResult{
			Title:    strings.TrimSpace(item.Title),
			URL:      strings.TrimSpace(item.Link),
			Abstract: strings.TrimSpace(item.Description),
		})
	}
	return results, nil
}

func extractDDGResult(n *html.Node) *searchResult {
	var r searchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			cls := htmlAttr(n, "class")
			switch {
			case n.Data == "a" && strings.Contains(cls, "result__a"):
				if href := htmlAttr(n, "href"); href != "" {
					r.URL = resolveDDGURL(href)
					r.Title = strings.TrimSpace(htmlText(n))
				}
			case strings.Contains(cls, "result__snippet"):
				r.Abstract = strings.TrimSpace(htmlText(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)

	if r.URL == "" && r.Title == "" {
		return nil
	}
	return &r
}

func resolveDDGURL(href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	if uddg := parsed.Query().Get("uddg"); uddg != "" {
		return uddg
	}
	if strings.HasPrefix(href, "/") {
		return "https://duckduckgo.com" + href
	}
	return href
}

func runFetchPage(ctx context.Context, targetURL string) (string, error) {
	parsed, err := validateFetchURL(ctx, targetURL)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("fetch page: %w", err)
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")

	resp, err := toolHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch page returned HTTP %d for %s", resp.StatusCode, parsed.String())
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var content string

	if strings.Contains(ct, "text/html") || ct == "" || strings.HasSuffix(strings.Split(ct, ";")[0], "html") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		if err != nil {
			return "", fmt.Errorf("read page: %w", err)
		}
		doc, err := html.Parse(bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("parse page HTML: %w", err)
		}
		content = htmlToMarkdown(doc)
	} else {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		if err != nil {
			return "", fmt.Errorf("read page: %w", err)
		}
		content = string(raw)
	}

	if len(content) > maxPageChars {
		content = content[:maxPageChars] + "\n\n[... content truncated ...]"
	}
	if strings.TrimSpace(content) == "" {
		return "(empty page)", nil
	}
	return content, nil
}

func validateFetchURL(ctx context.Context, raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("fetch page: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("fetch page: unsupported URL scheme %q", parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("fetch page: missing host in %q", raw)
	}
	if allowPrivateFetch {
		return parsed, nil
	}
	if isBlockedHostname(host) {
		return nil, fmt.Errorf("fetch page: refusing private or local host %q", host)
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		if isBlockedAddr(ip) {
			return nil, fmt.Errorf("fetch page: refusing private or local IP %q", host)
		}
		return parsed, nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err == nil {
		for _, addr := range addrs {
			if ip, ok := netip.AddrFromSlice(addr.IP); ok && isBlockedAddr(ip) {
				return nil, fmt.Errorf("fetch page: refusing private or local host %q", host)
			}
		}
	}
	return parsed, nil
}

func isBlockedHostname(host string) bool {
	return strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local")
}

func isBlockedAddr(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified()
}

func runListFiles(workspaceRoot string, yolo bool, args listFilesArgs) (string, error) {
	target := args.Path
	if strings.TrimSpace(target) == "" {
		target = "."
	}
	resolved, err := resolvePathForTool(workspaceRoot, target, yolo)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("list files: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("list files: %q is not a directory", target)
	}

	rootDepth := strings.Count(filepath.Clean(resolved), string(os.PathSeparator))
	entries := make([]string, 0, 64)
	err = filepath.WalkDir(resolved, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == resolved {
			return nil
		}
		rel, err := filepath.Rel(workspaceRoot, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && d.IsDir() {
			return filepath.SkipDir
		}

		depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
		if depth > 3 && d.IsDir() {
			return filepath.SkipDir
		}
		marker := ""
		if d.IsDir() {
			marker = "/"
		}
		entries = append(entries, filepath.ToSlash(rel)+marker)
		if len(entries) >= maxListEntries {
			return errListLimitReached
		}
		return nil
	})
	if err != nil && !errors.Is(err, errListLimitReached) {
		return "", fmt.Errorf("list files: %w", err)
	}
	slices.Sort(entries)
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	return strings.Join(entries, "\n"), nil
}

func runReadFile(workspaceRoot string, yolo bool, args readFileArgs) (string, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", errors.New("read_file path is required")
	}
	resolved, err := resolvePathForTool(workspaceRoot, path, yolo)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if len(data) > maxFileBytes {
		return "", fmt.Errorf("read file: file too large (%d bytes)", len(data))
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	start := 1
	end := len(lines)
	if args.StartLine > 0 {
		start = args.StartLine
	}
	if args.EndLine > 0 {
		end = args.EndLine
	}
	if start < 1 || end < start || start > len(lines) {
		return "", fmt.Errorf("read file: invalid line range %d..%d", start, end)
	}
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d. %s\n", i, lines[i-1])
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func runWriteFile(workspaceRoot string, yolo bool, args writeFileArgs) (string, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", errors.New("write_file path is required")
	}
	resolved, err := resolvePathForTool(workspaceRoot, path, yolo)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), filepath.ToSlash(path)), nil
}

func runAppendFile(workspaceRoot string, yolo bool, args appendFileArgs) (string, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", errors.New("append_file path is required")
	}
	resolved, err := resolvePathForTool(workspaceRoot, path, yolo)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("append file: %w", err)
	}
	f, err := os.OpenFile(resolved, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("append file: %w", err)
	}
	defer f.Close()
	written, err := f.WriteString(args.Content)
	if err != nil {
		return "", fmt.Errorf("append file: %w", err)
	}
	return fmt.Sprintf("appended %d bytes to %s", written, filepath.ToSlash(path)), nil
}

func runEditFile(workspaceRoot string, yolo bool, args editFileArgs) (string, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", errors.New("edit_file path is required")
	}
	if args.OldStr == "" {
		return "", errors.New("edit_file old_str is required")
	}
	resolved, err := resolvePathForTool(workspaceRoot, path, yolo)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("edit file: %w", err)
	}
	if len(data) > maxFileBytes {
		return "", fmt.Errorf("edit file: file too large (%d bytes)", len(data))
	}
	content := string(data)
	count := strings.Count(content, args.OldStr)
	if count == 0 {
		return "", fmt.Errorf("edit file: old_str not found in %s", filepath.ToSlash(path))
	}
	if count > 1 {
		return "", fmt.Errorf("edit file: old_str found %d times in %s (must be unique)", count, filepath.ToSlash(path))
	}
	updated := strings.Replace(content, args.OldStr, args.NewStr, 1)
	if err := os.WriteFile(resolved, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("edit file: %w", err)
	}
	return fmt.Sprintf("edited %s", filepath.ToSlash(path)), nil
}

type execResult struct {
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	ExitCode  int      `json:"exit_code"`
	Stdout    string   `json:"stdout"`
	Stderr    string   `json:"stderr"`
	Truncated bool     `json:"truncated"`
	TimedOut  bool     `json:"timed_out"`
	Failed    bool     `json:"failed"`
	Error     string   `json:"error,omitempty"`
}

func runExecuteProgram(ctx context.Context, workspaceRoot string, yolo bool, args executeProgramArgs) (string, error) {
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return "", errors.New("execute_program command is required")
	}
	if !yolo && containsDangerousPattern(command, args.Args) {
		return "", errors.New("execute_program blocked by dangerous-pattern policy")
	}

	cwd := "."
	if strings.TrimSpace(args.Cwd) != "" {
		cwd = args.Cwd
	}
	resolvedCWD, err := resolvePathForTool(workspaceRoot, cwd, yolo)
	if err != nil {
		return "", err
	}

	timeout := toolTimeout
	if args.TimeoutSeconds > 0 {
		if args.TimeoutSeconds > 120 {
			return "", errors.New("execute_program timeout_seconds exceeds 120")
		}
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, command, args.Args...)
	cmd.Dir = resolvedCWD

	stdout := &limitedBuffer{max: maxExecOutput}
	stderr := &limitedBuffer{max: maxExecOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	result := execResult{
		Command:   command,
		Args:      args.Args,
		Cwd:       filepath.ToSlash(cwd),
		ExitCode:  0,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		Failed:    runErr != nil,
	}

	if execCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		result.Error = runErr.Error()
	}

	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func runExecuteSkill(ctx context.Context, workspaceRoot string, yolo bool, skills map[string]skill, args executeSkillArgs) (string, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "", errors.New("execute_skill name is required")
	}
	sk, ok := skills[name]
	if !ok {
		return "", fmt.Errorf("execute_skill: skill %q not found", name)
	}

	command := strings.TrimSpace(args.Command)
	if command == "" {
		return "", errors.New("execute_skill command is required")
	}
	if !slices.Contains(sk.Commands, command) {
		return "", fmt.Errorf("execute_skill: command %q is not declared by skill %q", command, name)
	}

	return runExecuteProgram(ctx, workspaceRoot, yolo, executeProgramArgs{
		Command:        command,
		Args:           args.Args,
		Cwd:            args.Cwd,
		TimeoutSeconds: args.TimeoutSeconds,
	})
}

type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.max <= 0 {
		return len(p), nil
	}
	remaining := l.max - l.buf.Len()
	if remaining <= 0 {
		l.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = l.buf.Write(p[:remaining])
		l.truncated = true
		return len(p), nil
	}
	return l.buf.Write(p)
}

func (l *limitedBuffer) String() string {
	return l.buf.String()
}

func containsDangerousPattern(command string, args []string) bool {
	denyCommands := map[string]struct{}{
		"sh": {}, "bash": {}, "zsh": {}, "fish": {}, "ksh": {}, "dash": {},
		"cmd": {}, "cmd.exe": {}, "powershell": {}, "pwsh": {},
	}
	if _, blocked := denyCommands[strings.ToLower(filepath.Base(command))]; blocked {
		return true
	}

	// exec.Command does not invoke a shell, so shell metacharacters in args are
	// generally safe as literal text. Keep command-token validation strict, and
	// only reject malformed control characters in args.
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, " \t\r\n\x00") {
		return true
	}
	commandPatterns := []string{";", "&&", "||", "|", "`", "$(", "${", "<(", ">("}
	for _, p := range commandPatterns {
		if strings.Contains(command, p) {
			return true
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "\x00") {
			return true
		}
	}
	return false
}

// resolvePathForTool resolves a user-supplied path against workspaceRoot.
// In yolo mode all paths (including absolute ones) are permitted; otherwise
// the path must stay within workspaceRoot.
func resolvePathForTool(workspaceRoot, userPath string, yolo bool) (string, error) {
	if strings.TrimSpace(userPath) == "" {
		return "", errors.New("path is required")
	}
	if yolo {
		clean := filepath.Clean(userPath)
		if !filepath.IsAbs(clean) {
			clean = filepath.Join(workspaceRoot, clean)
		}
		return filepath.Abs(clean)
	}
	return resolveWorkspacePath(workspaceRoot, userPath)
}

// resolveExistingPath resolves symlinks for p, walking up to the nearest existing
// ancestor and reconstructing the remaining path components without symlinks.
func resolveExistingPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil
	}
	resolvedParent, err := resolveExistingPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(p)), nil
}

func resolveWorkspacePath(workspaceRoot, userPath string) (string, error) {
	if strings.TrimSpace(userPath) == "" {
		return "", errors.New("path is required")
	}
	clean := filepath.Clean(userPath)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute paths are not allowed: %q", userPath)
	}
	candidate := filepath.Join(workspaceRoot, clean)

	absBase, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	// Resolve symlinks on both paths to prevent symlink-based workspace escapes.
	resolvedBase, err := resolveExistingPath(absBase)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolvedCandidate, err := resolveExistingPath(absCandidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	rel, err := filepath.Rel(resolvedBase, resolvedCandidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", userPath)
	}
	return absCandidate, nil
}

func htmlToMarkdown(doc *html.Node) string {
	buf := &bytes.Buffer{}

	skipTags := map[string]bool{
		"script": true, "style": true, "nav": true, "footer": true,
		"aside": true, "head": true, "noscript": true, "iframe": true,
		"svg": true, "figure": true,
	}
	blockTags := map[string]bool{
		"p": true, "div": true, "section": true, "article": true,
		"main": true, "blockquote": true, "pre": true, "figure": true,
		"header": true, "table": true, "tr": true, "td": true, "th": true,
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			text := n.Data
			if strings.TrimSpace(text) == "" {
				if !strings.Contains(text, "\n") {
					buf.WriteByte(' ')
				}
				return
			}
			text = strings.Join(strings.Fields(text), " ")
			buf.WriteString(text)
			buf.WriteByte(' ')
			return
		}

		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}

		tag := n.Data
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(tag[1] - '0')
			buf.WriteString("\n\n")
			buf.WriteString(strings.Repeat("#", level))
			buf.WriteByte(' ')
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.WriteString("\n\n")
			return
		case "a":
			href := htmlAttr(n, "href")
			inner := &bytes.Buffer{}
			prev := buf
			buf = inner
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf = prev
			text := strings.TrimSpace(inner.String())
			if href != "" && text != "" && !strings.HasPrefix(href, "#") {
				buf.WriteString("[")
				buf.WriteString(text)
				buf.WriteString("](")
				buf.WriteString(href)
				buf.WriteString(")")
			} else if text != "" {
				buf.WriteString(text)
			}
			buf.WriteByte(' ')
			return
		case "strong", "b":
			buf.WriteString("**")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.Truncate(len(strings.TrimRight(buf.String(), " ")))
			buf.WriteString("** ")
			return
		case "em", "i":
			buf.WriteByte('_')
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.Truncate(len(strings.TrimRight(buf.String(), " ")))
			buf.WriteString("_ ")
			return
		case "code":
			buf.WriteByte('`')
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.Truncate(len(strings.TrimRight(buf.String(), " ")))
			buf.WriteString("` ")
			return
		case "pre":
			buf.WriteString("\n\n```\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.WriteString("\n```\n\n")
			return
		case "li":
			buf.WriteString("\n- ")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		case "ul", "ol":
			buf.WriteString("\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.WriteString("\n")
			return
		case "br":
			buf.WriteString("\n")
			return
		case "hr":
			buf.WriteString("\n---\n")
			return
		case "img":
			alt := htmlAttr(n, "alt")
			if alt != "" {
				buf.WriteString(alt)
				buf.WriteByte(' ')
			}
			return
		}

		if blockTags[tag] {
			buf.WriteString("\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.WriteString("\n")
			return
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}

	walk(doc)

	raw := buf.String()
	lines := strings.Split(raw, "\n")
	var out []string
	blank := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "" {
			blank++
			if blank <= 2 {
				out = append(out, "")
			}
		} else {
			blank = 0
			out = append(out, trimmed)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func htmlAttr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func htmlText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
