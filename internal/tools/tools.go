package tools

import (
	"bytes"
	"capelin-go/internal/contracts"
	"capelin-go/internal/policy"
	"capelin-go/internal/skills"
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
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
)

// Tool names are owned by the tool capability so policy and composition use a
// single catalog vocabulary.
const (
	WebSearch      = "web_search"
	FetchPage      = "fetch_page"
	ListFiles      = "list_files"
	ReadFile       = "read_file"
	WriteFile      = "write_file"
	EditFile       = "edit_file"
	AppendFile     = "append_file"
	ExecuteProgram = "execute_program"
	ExecuteSkill   = "execute_skill"
	ListSkills     = "list_skills"
	ReadSkill      = "read_skill"
	CreateSubagent = "create_subagent"
	RunSubagent    = "run_subagent"
	AwaitSubagent  = "await_subagent"
	ListSubagents  = "list_subagents"
	ReadSubagent   = "read_subagent"
	CancelSubagent = "cancel_subagent"
	UpdateTodos    = "update_todos"
	CompleteGoal   = "complete_goal"
)

const (
	toolTimeout      = 60 * time.Second
	toolTimeoutMax   = 600 // 10 minutes – absolute cap for any tool timeout
	maxPageChars     = 14000
	maxSearchResults = 12
	maxListEntries   = 300
	maxFileBytes     = 512 * 1024
	maxExecOutput    = 256 * 1024
	ToolTimeoutMax   = toolTimeoutMax
)

var ddgSearchURL = "https://html.duckduckgo.com/html"
var bingSearchURL = "https://www.bing.com/search"
var allowPrivateFetch = false

var errListLimitReached = errors.New("list limit reached")

type privateFetchOverrideKey struct{}

func withPrivateFetchOverride(ctx context.Context, allow bool) context.Context {
	return context.WithValue(ctx, privateFetchOverrideKey{}, allow)
}

func privateFetchAllowed(ctx context.Context) bool {
	if allow, ok := ctx.Value(privateFetchOverrideKey{}).(bool); ok {
		return allow
	}
	return allowPrivateFetch
}

// safeDialer resolves the target hostname and validates every resolved IP against
// isBlockedAddr before opening the TCP connection. This prevents DNS-rebinding
// attacks where a public IP is returned during the pre-flight validateFetchURL
// check but a private IP is returned during the actual HTTP dial.
var safeDialer = &net.Dialer{
	Timeout:   toolTimeout,
	KeepAlive: toolTimeout,
}

func safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !privateFetchAllowed(ctx) {
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
	Timeout: time.Duration(toolTimeoutMax) * time.Second,
	Transport: &http.Transport{
		DialContext:           safeDial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	CheckRedirect: checkFetchRedirect,
}

func checkFetchRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("too many redirects")
	}
	if req == nil || req.URL == nil {
		return fmt.Errorf("fetch page: redirect has no URL")
	}
	if _, err := validateFetchURLWithPrivate(req.Context(), req.URL.String(), privateFetchAllowed(req.Context())); err != nil {
		return err
	}
	req.Header.Set("User-Agent", contracts.CapelinUserAgent)
	return nil
}

type userAgentTransport struct {
	base http.RoundTripper
}

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	request.Header.Set("User-Agent", contracts.CapelinUserAgent)
	return t.base.RoundTrip(request)
}

func clientWithUserAgent(client *http.Client) *http.Client {
	copy := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = userAgentTransport{base: base}
	return &copy
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

type searchResult struct {
	Title    string
	URL      string
	Abstract string
}

type searchProvider string

const (
	searchProviderDuckDuckGo searchProvider = "DuckDuckGo"
	searchProviderBing       searchProvider = "Bing"
)

func specWebSearch() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        WebSearch,
			Description: "Search the web with quality-gated DuckDuckGo results and Bing fallback; return the selected provider, result titles, URLs, and abstracts.",
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

func specFetchPage() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        FetchPage,
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

func specListFiles() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ListFiles,
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

func specReadFile() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ReadFile,
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

func specWriteFile() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        WriteFile,
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

func specAppendFile() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        AppendFile,
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

func specEditFile() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        EditFile,
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

func specExecuteProgram() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ExecuteProgram,
			Description: "Execute a local program directly with an explicit executable and argument vector; never invoke or parse a shell.",
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

func specExecuteSkill() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ExecuteSkill,
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

func specListSkills() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ListSkills,
			Description: "List loaded Claude-style skills discovered from skill directories.",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}
}

func specReadSkill() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ReadSkill,
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

func specCreateSubagent() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        CreateSubagent,
			Description: "Create a worker subagent session with inherited-and-restricted tool policy. Does not start execution. Admission is non-blocking: large fan-outs are kept in bounded pending/queued batches, and a full allowance returns a recoverable capacity error for retry after terminal children release capacity.",
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
						"description": `Behavior when parent is at --subagent-max-children: "wait_for_slot" (default, admit bounded pending/queued work without blocking the parent tool batch) or "fail_fast" (return error immediately).`,
					},
					"wait_timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Legacy wait-for-slot timeout validation. Admission never waits synchronously; retry a capacity error after a terminal child releases capacity. Defaults to --subagent-timeout-seconds (600s unless configured).",
					},
				},
				"required":             []string{"question"},
				"additionalProperties": false,
			},
		},
	}
}

func specRunSubagent() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        RunSubagent,
			Description: "Start a created subagent. Creation and execution are separate phases. For parallel execution of multiple subagents: call run_subagent with wait=false for ALL admitted handles first (queued work is scheduled within bounded limits), then call await_subagent for each to collect results. Retry an actionable capacity error after terminal children release capacity.",
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

func specAwaitSubagent() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        AwaitSubagent,
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

func specListSubagents() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ListSubagents,
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

func specReadSubagent() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        ReadSubagent,
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

func specCancelSubagent() contracts.Tool {
	return contracts.Tool{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        CancelSubagent,
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
	return runWebSearchWithClient(ctx, query, toolHTTPClient)
}

func runWebSearchWithClient(ctx context.Context, query string, client *http.Client) (string, error) {
	if client == nil {
		client = toolHTTPClient
	}
	primaryResults, primaryErr := runDuckDuckGoSearchWithClient(ctx, query, client)
	primaryResults = qualityGateSearchResults(query, primaryResults)
	if primaryErr == nil && len(primaryResults) > 0 {
		return formatSearchResponse(searchProviderDuckDuckGo, false, "", primaryResults), nil
	}

	fallbackReason := searchAttemptReason(searchProviderDuckDuckGo, primaryErr)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fallbackResults, fallbackErr := runBingSearchWithClient(ctx, query, client)
	fallbackResults = qualityGateSearchResults(query, fallbackResults)
	if fallbackErr == nil && len(fallbackResults) > 0 {
		return formatSearchResponse(searchProviderBing, true, fallbackReason, fallbackResults), nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	return formatNoTrustworthyResults(fallbackReason, searchAttemptReason(searchProviderBing, fallbackErr)), nil
}

var errNoSearchResults = errors.New("no search results")

func searchAttemptReason(provider searchProvider, providerErr error) string {
	if providerErr != nil {
		if errors.Is(providerErr, errNoSearchResults) {
			return fmt.Sprintf("%s returned no results", provider)
		}
		// Do not include provider response bodies or parser details in the
		// tool result: an error body can contain the same polluted content the
		// quality gate is intended to keep away from the agent.
		return fmt.Sprintf("%s request or response failed", provider)
	}
	return fmt.Sprintf("%s returned no trustworthy results", provider)
}

func formatSearchResponse(provider searchProvider, fallback bool, fallbackReason string, results []searchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Search provider: %s\n", provider)
	if fallback {
		b.WriteString("Fallback: yes\n")
		fmt.Fprintf(&b, "Fallback reason: %s\n", fallbackReason)
	} else {
		b.WriteString("Fallback: no\n")
	}
	b.WriteString("\n")
	b.WriteString(formatSearchResults(results))
	return b.String()
}

func formatNoTrustworthyResults(primaryReason, fallbackReason string) string {
	return fmt.Sprintf("Search provider: none\nFallback: yes\nFallback reason: %s; %s\nSearch status: no trustworthy results\n\nNo trustworthy search results were found. Rejected results are omitted.", primaryReason, fallbackReason)
}

// qualityGateSearchResults normalizes and validates provider records before
// they become search evidence. The first result for a canonical URL wins so a
// provider cannot fill the response with duplicate records.
func qualityGateSearchResults(query string, results []searchResult) []searchResult {
	if len(results) == 0 {
		return nil
	}

	accepted := make([]searchResult, 0, min(len(results), maxSearchResults))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		normalizedURL, err := normalizeSearchURL(result.URL)
		if err != nil {
			continue
		}
		result.Title = strings.TrimSpace(result.Title)
		result.URL = normalizedURL
		result.Abstract = strings.TrimSpace(result.Abstract)

		if _, ok := seen[normalizedURL]; ok {
			continue
		}
		// The first valid canonical URL claims the record even when its
		// metadata is later rejected, so a provider cannot bypass the gate by
		// repeating the URL with sanitized metadata.
		seen[normalizedURL] = struct{}{}
		if result.Title == "" {
			continue
		}
		if !isRelevantSearchResult(query, result) || isUnsafeSearchResult(result) {
			continue
		}
		accepted = append(accepted, result)
	}

	if len(accepted) > maxSearchResults {
		accepted = accepted[:maxSearchResults]
	}
	return accepted
}

func normalizeSearchURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("empty or invalid search URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported search URL scheme %q", parsed.Scheme)
	}
	if parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil {
		return "", errors.New("search URL has no usable public URL form")
	}
	port := parsed.Port()
	portNumber := 0
	if port != "" {
		portNumber, err = strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", errors.New("search URL has an invalid port")
		}
	}

	host := strings.ToLower(parsed.Hostname())
	normalizedPort := strconv.Itoa(portNumber)
	if port == "" || (parsed.Scheme == "http" && portNumber == 80) || (parsed.Scheme == "https" && portNumber == 443) {
		parsed.Host = host
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]:" + normalizedPort
	} else {
		parsed.Host = host + ":" + normalizedPort
	}
	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	} else {
		trailingSlash := strings.HasSuffix(parsed.Path, "/")
		parsed.Path = path.Clean(parsed.Path)
		if !strings.HasPrefix(parsed.Path, "/") {
			parsed.Path = "/" + parsed.Path
		}
		if trailingSlash && parsed.Path != "/" {
			parsed.Path += "/"
		}
		parsed.RawPath = ""
	}
	return parsed.String(), nil
}

func isRelevantSearchResult(query string, result searchResult) bool {
	queryTerms := meaningfulSearchTerms(query)
	if len(queryTerms) == 0 {
		// A query made only of stop words provides no reliable relevance
		// signal. Treating every provider record as relevant would let an
		// otherwise successful response bypass the quality gate.
		return false
	}
	resultTerms := make(map[string]struct{})
	for _, term := range searchTokens(result.Title + " " + result.Abstract) {
		resultTerms[term] = struct{}{}
	}
	for _, term := range technicalSearchTokens(result.Title + " " + result.Abstract) {
		resultTerms[term] = struct{}{}
	}
	matches := 0
	for _, term := range queryTerms {
		for resultTerm := range resultTerms {
			if searchTermsMatch(term, resultTerm) {
				matches++
				break
			}
		}
	}
	// Require nearly all content-bearing query terms. This keeps a result
	// containing one incidental word from passing a multi-term relevance check,
	// while allowing a long query to omit one term when a provider shortens a
	// title or abstract.
	requiredMatches := len(queryTerms)
	if requiredMatches > 3 {
		requiredMatches--
	}
	return matches >= requiredMatches
}

func searchTermsMatch(queryTerm, resultTerm string) bool {
	if queryTerm == resultTerm {
		return true
	}
	if strings.ContainsAny(queryTerm, "+#") || strings.ContainsAny(resultTerm, "+#") {
		return false
	}
	queryStem := strings.TrimSuffix(queryTerm, "s")
	resultStem := strings.TrimSuffix(resultTerm, "s")
	if queryStem == resultStem {
		return true
	}
	if len(queryStem) >= 3 && strings.HasPrefix(resultStem, queryStem) {
		return true
	}
	return len(resultStem) >= 3 && strings.HasPrefix(queryStem, resultStem)
}

func meaningfulSearchTerms(text string) []string {
	terms := searchTokens(text)
	technicalTerms := technicalSearchTokens(text)
	if len(technicalTerms) > 0 {
		terms = technicalTerms
		for _, term := range searchTokens(text) {
			if len([]rune(term)) >= 2 && !searchStopWords[term] {
				terms = append(terms, term)
			}
		}
	}
	meaningful := make([]string, 0, len(terms))
	for _, term := range terms {
		if len([]rune(term)) < 2 || searchStopWords[term] {
			continue
		}
		meaningful = append(meaningful, term)
	}
	return meaningful
}

func technicalSearchTokens(text string) []string {
	runes := []rune(strings.ToLower(text))
	var tokens []string
	for i := 0; i < len(runes); {
		if !unicode.IsLetter(runes[i]) && !unicode.IsNumber(runes[i]) {
			i++
			continue
		}
		start := i
		for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsNumber(runes[i])) {
			i++
		}
		if i < len(runes) && runes[i] == '#' {
			i++
		} else if i+1 < len(runes) && runes[i] == '+' && runes[i+1] == '+' {
			i += 2
		}
		candidate := string(runes[start:i])
		if strings.ContainsAny(candidate, "+#") {
			tokens = append(tokens, candidate)
		}
	}
	return tokens
}

var searchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "how": true, "in": true,
	"is": true, "it": true, "of": true, "on": true, "or": true, "the": true,
	"to": true, "what": true, "when": true, "where": true, "which": true,
	"who": true, "why": true, "with": true,
}

func searchTokens(text string) []string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

func isUnsafeSearchResult(result searchResult) bool {
	text := strings.ToLower(result.Title + " " + result.URL + " " + result.Abstract)
	if parsed, err := url.Parse(result.URL); err == nil {
		for _, marker := range []string{
			"porn", "hentai", "sexcam", "camgirl", "onlyfans", "xvideos",
			"xhamster", "xnxx", "redtube", "youporn", "spankbang", "brazzers",
		} {
			if strings.Contains(strings.ToLower(parsed.Hostname()), marker) {
				return true
			}
		}
	}
	terms := make(map[string]struct{})
	for _, term := range searchTokens(text) {
		terms[term] = struct{}{}
	}
	for _, term := range []string{
		"porn", "porno", "pornography", "hentai", "sexcam", "camgirl",
		"camgirls", "blowjob", "blowjobs", "milf", "onlyfans",
	} {
		if _, ok := terms[term]; ok {
			return true
		}
	}
	for _, phrase := range []string{
		"adult entertainment", "adult videos", "free porn", "make money fast",
		"crypto giveaway",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func runDuckDuckGoSearch(ctx context.Context, query string) ([]searchResult, error) {
	return runDuckDuckGoSearchWithClient(ctx, query, toolHTTPClient)
}

func runDuckDuckGoSearchWithClient(ctx context.Context, query string, client *http.Client) ([]searchResult, error) {
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
	req.Header.Set("User-Agent", contracts.CapelinUserAgent)
	req.Header.Set("DNT", "1")

	resp, err := clientWithUserAgent(client).Do(req)
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
	return results, nil
}

func runBingSearch(ctx context.Context, query string) ([]searchResult, error) {
	return runBingSearchWithClient(ctx, query, toolHTTPClient)
}

func runBingSearchWithClient(ctx context.Context, query string, client *http.Client) ([]searchResult, error) {
	endpoint, err := url.Parse(bingSearchURL)
	if err != nil {
		return nil, fmt.Errorf("bing search: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("setlang", "en-US")
	params.Set("mkt", "en-US")
	params.Set("format", "rss")
	params.Set("adlt", "strict")
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("bing search: %w", err)
	}
	req.Header.Set("User-Agent", contracts.CapelinUserAgent)

	resp, err := clientWithUserAgent(client).Do(req)
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
			if hasHTMLClass(cls, "result") || hasHTMLClass(cls, "web-result") {
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

func hasHTMLClass(classAttribute, wanted string) bool {
	for _, className := range strings.Fields(classAttribute) {
		if className == wanted {
			return true
		}
	}
	return false
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
	return runFetchPageWithClient(ctx, targetURL, toolHTTPClient)
}

// runFetchPageWithClient allows server-mode callers to supply the policy-aware
// transport. The standalone wrapper above retains the existing safe tool
// client and test API.
func runFetchPageWithClient(ctx context.Context, targetURL string, client *http.Client) (string, error) {
	parsed, err := validateFetchURLWithPrivate(ctx, targetURL, privateFetchAllowed(ctx))
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("fetch page: %w", err)
	}
	req.Header.Set("User-Agent", contracts.CapelinUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")

	if client == nil {
		client = toolHTTPClient
	}
	resp, err := clientWithUserAgent(client).Do(req)
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
	return validateFetchURLWithPrivate(ctx, raw, allowPrivateFetch)
}

func validateFetchURLWithPrivate(ctx context.Context, raw string, allowPrivate bool) (*url.URL, error) {
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
	if allowPrivate {
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

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".capelin-write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func runWriteFile(workspaceRoot string, yolo bool, args writeFileArgs) (string, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", errors.New("write_file path is required")
	}
	if len(args.Content) > maxFileBytes {
		return "", fmt.Errorf("write file: content too large (%d bytes, max %d)", len(args.Content), maxFileBytes)
	}
	resolved, err := resolvePathForTool(workspaceRoot, path, yolo)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	// Atomic write: write to temp file then rename.
	if err := atomicWrite(resolved, []byte(args.Content)); err != nil {
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
	if len(updated) > maxFileBytes {
		return "", fmt.Errorf("edit file: result too large (%d bytes)", len(updated))
	}
	// Atomic write: write to temp file then rename.
	if err := atomicWrite(resolved, []byte(updated)); err != nil {
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

func prepareExecuteProgram(workspaceRoot string, yolo bool, args executeProgramArgs) (string, string, error) {
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return "", "", errors.New("execute_program command is required")
	}
	if !yolo && containsDangerousPattern(command, args.Args) {
		return "", "", fmt.Errorf(
			"execute_program blocked by dangerous-pattern policy for command %q: direct program execution requires the executable alone in command and each argument separately in args; it never invokes or parses a shell (for example, use command %q with args [\"arg1\", \"arg2\"])",
			command,
			"program",
		)
	}

	cwd := "."
	if strings.TrimSpace(args.Cwd) != "" {
		cwd = args.Cwd
	}
	resolvedCWD, err := resolvePathForTool(workspaceRoot, cwd, yolo)
	if err != nil {
		return "", "", err
	}
	return command, resolvedCWD, nil
}

// runDetachedProgram validates and starts a direct child process, then hands
// responsibility for waiting to a goroutine. The child receives no inherited
// application output streams, and no context-bound timeout is applied after
// Start succeeds. A parent cancellation observed before Start still rejects
// the launch; cancellation after Start deliberately does not kill the child.
func runDetachedProgram(ctx context.Context, workspaceRoot string, yolo bool, args executeProgramArgs) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("execute_program: detached launch cancelled: %w", err)
	}
	command, resolvedCWD, err := prepareExecuteProgram(workspaceRoot, yolo, args)
	if err != nil {
		return err
	}

	// CommandContext with an uncancellable context gives the platform-specific
	// process-group setup a valid Cancel hook while still leaving the child
	// independent of the caller after Start succeeds.
	cmd := exec.CommandContext(context.Background(), command, args.Args...)
	cmd.Dir = resolvedCWD
	setupProcessGroup(cmd)
	childOutput, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("execute_program: opening detached output sink: %w", err)
	}
	cmd.Stdout = childOutput
	cmd.Stderr = childOutput
	if err := cmd.Start(); err != nil {
		_ = childOutput.Close()
		return fmt.Errorf("execute_program: failed to start %q: %w", command, err)
	}
	// The child inherited its own descriptors. Closing the parent's copy avoids
	// retaining a file handle while the asynchronously reaped child runs.
	_ = childOutput.Close()
	go func() {
		// Waiting in this goroutine allows the OS to release the process
		// resources without making the caller wait for the child lifetime.
		_ = cmd.Wait()
	}()
	return nil
}

func runExecuteProgram(ctx context.Context, workspaceRoot string, yolo bool, args executeProgramArgs) (string, error) {
	return runExecuteProgramWithTimeoutLimit(ctx, workspaceRoot, yolo, args, 120)
}

// runIdleHookProgram keeps the idle-hook wait adapter on the tool package's
// process and result envelope while allowing its own configured 600-second
// lifecycle ceiling. The ordinary execute_program contract remains capped at
// 120 seconds.
func runIdleHookProgram(ctx context.Context, workspaceRoot string, yolo bool, args executeProgramArgs) (string, error) {
	return runExecuteProgramWithTimeoutLimit(ctx, workspaceRoot, yolo, args, ToolTimeoutMax)
}

func runExecuteProgramWithTimeoutLimit(ctx context.Context, workspaceRoot string, yolo bool, args executeProgramArgs, maxTimeoutSeconds int) (string, error) {
	command, resolvedCWD, err := prepareExecuteProgram(workspaceRoot, yolo, args)
	if err != nil {
		return "", err
	}
	cwd := "."
	if strings.TrimSpace(args.Cwd) != "" {
		cwd = args.Cwd
	}

	timeout := toolTimeout
	if args.TimeoutSeconds > 0 {
		if args.TimeoutSeconds > maxTimeoutSeconds {
			return "", fmt.Errorf("execute_program timeout_seconds exceeds %d", maxTimeoutSeconds)
		}
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, command, args.Args...)
	cmd.Dir = resolvedCWD
	setupProcessGroup(cmd)

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

func runExecuteSkill(ctx context.Context, workspaceRoot string, yolo bool, skillsMap map[string]skills.Skill, args executeSkillArgs) (string, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "", errors.New("execute_skill name is required")
	}
	sk, ok := skillsMap[name]
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
	return policy.ContainsDangerousPattern(command, args)
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

// resolveWorkspacePath is retained as an app-local compatibility wrapper for
// callers and tests; the confinement policy is owned by internal/policy.
func resolveWorkspacePath(workspaceRoot, userPath string) (string, error) {
	return policy.ResolveWorkspacePath(workspaceRoot, userPath)
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
