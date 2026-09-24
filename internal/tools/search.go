package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"capelin-go/internal/contracts"
)

const (
	SearchProviderParallel = "parallel"
	SearchProviderExa      = "exa"

	parallelSearchEndpoint = "https://search.parallel.ai/mcp"
	exaSearchEndpoint      = "https://mcp.exa.ai/mcp"

	searchRequestTimeout = 30 * time.Second
	maxMCPResponseBytes  = 512 * 1024
	maxMCPSearchResults  = 8
)

// SearchProviderConfig selects the fixed hosted provider used after the
// DuckDuckGo attempt. Endpoints are deliberately not configurable: a model or
// caller must not be able to turn web_search into an arbitrary HTTP client.
type SearchProviderConfig struct {
	Provider       string
	ParallelAPIKey string
	ExaAPIKey      string
}

const maxSearchProviderCallsPerTask = 5

type searchTaskFlight struct {
	done   chan struct{}
	result string
	err    error
}

// SearchTask owns one research task's provider-call budget and normalized-query
// cache. It is deliberately request-scoped; callers must not share it across
// user turns.
type SearchTask struct {
	mu            sync.Mutex
	providerCalls int
	results       map[string]string
	flights       map[string]*searchTaskFlight
	failedFetches map[string]fetchFailure
	fetchFlights  map[string]*searchTaskFlight
}

// SearchBatch gives concurrent searches a deterministic reservation order
// without serializing their primary provider requests. Every search advances
// both phases: primary reservations are ordered first, then fallback
// reservations are ordered after each primary outcome is known.
type SearchBatch struct {
	primaryTurns  []chan struct{}
	fallbackTurns []chan struct{}
}

// NewSearchBatch creates a reservation scheduler for one tool-call batch.
func NewSearchBatch(searchCount int) *SearchBatch {
	if searchCount <= 0 {
		return nil
	}
	batch := &SearchBatch{
		primaryTurns:  make([]chan struct{}, searchCount),
		fallbackTurns: make([]chan struct{}, searchCount),
	}
	for i := range searchCount {
		batch.primaryTurns[i] = make(chan struct{})
		batch.fallbackTurns[i] = make(chan struct{})
	}
	close(batch.primaryTurns[0])
	close(batch.fallbackTurns[0])
	return batch
}

func (batch *SearchBatch) turns(fallback bool) []chan struct{} {
	if fallback {
		return batch.fallbackTurns
	}
	return batch.primaryTurns
}

func (batch *SearchBatch) advance(fallback bool, order int) {
	turns := batch.turns(fallback)
	if order+1 < len(turns) {
		close(turns[order+1])
	}
}

func (batch *SearchBatch) reserve(ctx context.Context, fallback bool, order int, reserve func() (int, bool)) (int, bool) {
	if batch == nil || order < 0 || order >= len(batch.turns(fallback)) {
		return reserve()
	}
	turns := batch.turns(fallback)
	select {
	case <-turns[order]:
	case <-ctx.Done():
		batch.advance(fallback, order)
		return 0, false
	}
	if err := ctx.Err(); err != nil {
		batch.advance(fallback, order)
		return 0, false
	}
	used, ok := reserve()
	batch.advance(fallback, order)
	return used, ok
}

func (batch *SearchBatch) skip(ctx context.Context, fallback bool, order int) {
	if batch == nil || order < 0 || order >= len(batch.turns(fallback)) {
		return
	}
	turns := batch.turns(fallback)
	select {
	case <-turns[order]:
	case <-ctx.Done():
		batch.advance(fallback, order)
		return
	}
	batch.advance(fallback, order)
}

// NewSearchTask starts a task-local web-search budget and empty result cache.
func NewSearchTask() *SearchTask {
	return &SearchTask{
		results:       make(map[string]string),
		flights:       make(map[string]*searchTaskFlight),
		failedFetches: make(map[string]fetchFailure),
		fetchFlights:  make(map[string]*searchTaskFlight),
	}
}

func (task *SearchTask) search(ctx context.Context, query string, client *http.Client, config SearchProviderConfig) (string, error) {
	return task.searchWithBatch(ctx, query, client, config, nil, -1)
}

func (task *SearchTask) searchWithBatch(ctx context.Context, query string, client *http.Client, config SearchProviderConfig, batch *SearchBatch, order int) (string, error) {
	if task == nil {
		return runWebSearchWithConfig(ctx, query, client, config)
	}
	key := normalizeSearchQuery(query)
	task.mu.Lock()
	if result, ok := task.results[key]; ok {
		task.mu.Unlock()
		if batch != nil {
			batch.skip(ctx, false, order)
			batch.skip(ctx, true, order)
		}
		return result, nil
	}
	if flight, ok := task.flights[key]; ok {
		task.mu.Unlock()
		// A duplicate query does not own either reservation phase. Release its
		// batch slots before waiting for the in-flight owner so an earlier
		// duplicate cannot hold the owner's ordered primary turn hostage.
		if batch != nil {
			batch.skip(ctx, false, order)
			batch.skip(ctx, true, order)
		}
		select {
		case <-flight.done:
			return flight.result, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &searchTaskFlight{done: make(chan struct{})}
	task.flights[key] = flight
	task.mu.Unlock()

	reservationPhase := 0
	reserve := func() (int, bool) {
		fallback := reservationPhase > 0
		reservationPhase++
		if batch != nil {
			return batch.reserve(ctx, fallback, order, task.reserveProviderInvocation)
		}
		return task.reserveProviderInvocation()
	}
	result, err := runWebSearchWithBudget(ctx, query, client, config, reserve)
	if batch != nil && reservationPhase < 2 {
		// A primary success, cancellation, or invalid provider configuration
		// skips the fallback phase so the next concurrent call can proceed.
		batch.skip(ctx, true, order)
	}

	task.mu.Lock()
	if err == nil {
		task.results[key] = result
	}
	flight.result, flight.err = result, err
	delete(task.flights, key)
	close(flight.done)
	task.mu.Unlock()
	return result, err
}

func (task *SearchTask) reserveProviderInvocation() (int, bool) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.providerCalls >= maxSearchProviderCallsPerTask {
		return task.providerCalls, false
	}
	task.providerCalls++
	return task.providerCalls, true
}

func normalizeSearchQuery(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

var (
	mcpSearchURL = ""
	searchConfig SearchProviderConfig
)

type searchProviderName string

const (
	searchProviderDuckDuckGo searchProviderName = "DuckDuckGo"
	searchProviderParallel   searchProviderName = "Parallel"
	searchProviderExa        searchProviderName = "Exa"
)

type searchProvider interface {
	Name() searchProviderName
	Search(context.Context, string, *http.Client) ([]searchResult, error)
}

type duckDuckGoProvider struct{}

func (duckDuckGoProvider) Name() searchProviderName { return searchProviderDuckDuckGo }

func (duckDuckGoProvider) Search(ctx context.Context, query string, client *http.Client) ([]searchResult, error) {
	return runDuckDuckGoSearchWithClient(ctx, query, client)
}

type mcpSearchProvider struct {
	name     searchProviderName
	endpoint string
	toolName string
	apiKey   string
}

func (p mcpSearchProvider) Name() searchProviderName { return p.name }

func (p mcpSearchProvider) Search(ctx context.Context, query string, client *http.Client) ([]searchResult, error) {
	arguments := map[string]any{}
	switch p.name {
	case searchProviderParallel:
		arguments["objective"] = query
		arguments["search_queries"] = []string{query}
	case searchProviderExa:
		arguments["query"] = query
		arguments["type"] = "auto"
		arguments["numResults"] = maxMCPSearchResults
		arguments["livecrawl"] = "fallback"
	default:
		return nil, errors.New("unsupported search provider")
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      p.toolName,
			"arguments": arguments,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("provider request could not be encoded")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("provider request could not be created")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", contracts.CapelinUserAgent)
	if strings.TrimSpace(p.apiKey) != "" {
		if p.name == searchProviderExa {
			req.Header.Set("x-api-key", strings.TrimSpace(p.apiKey))
		} else {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(p.apiKey))
		}
	}

	providerClient := clientWithUserAgent(client)
	// Provider endpoints are fixed, but redirects can send the credential and
	// query body to a different origin. Treat redirects as a provider failure.
	providerClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := providerClient.Do(req)
	if err != nil {
		return nil, errors.New("provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, errors.New("provider request failed")
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseBytes+1))
	if err != nil {
		return nil, errors.New("provider response could not be read")
	}
	if len(raw) > maxMCPResponseBytes {
		return nil, errors.New("provider response exceeded the response limit")
	}
	results, err := parseMCPSearchResponse(raw)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, errNoSearchResults
	}
	return results, nil
}

func normalizeSearchProvider(value string) (searchProviderName, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", SearchProviderParallel:
		return searchProviderParallel, nil
	case SearchProviderExa:
		return searchProviderExa, nil
	default:
		return "", fmt.Errorf("unsupported search provider %q", strings.TrimSpace(value))
	}
}

func configuredMCPProvider(config SearchProviderConfig) (searchProvider, error) {
	name, err := normalizeSearchProvider(config.Provider)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(mcpSearchURL)
	if endpoint == "" {
		if name == searchProviderExa {
			endpoint = exaSearchEndpoint
		} else {
			endpoint = parallelSearchEndpoint
		}
	}
	provider := mcpSearchProvider{name: name, endpoint: endpoint}
	switch name {
	case searchProviderExa:
		provider.toolName = "web_search_exa"
		provider.apiKey = config.ExaAPIKey
	case searchProviderParallel:
		provider.toolName = "web_search"
		provider.apiKey = config.ParallelAPIKey
	}
	return provider, nil
}

func runWebSearch(ctx context.Context, query string) (string, error) {
	return runWebSearchWithConfig(ctx, query, toolHTTPClient, searchConfig)
}

func runWebSearchWithClient(ctx context.Context, query string, client *http.Client) (string, error) {
	return runWebSearchWithConfig(ctx, query, client, searchConfig)
}

func runWebSearchWithConfig(ctx context.Context, query string, client *http.Client, config SearchProviderConfig) (string, error) {
	return runWebSearchWithBudget(ctx, query, client, config, nil)
}

func runWebSearchWithBudget(ctx context.Context, query string, client *http.Client, config SearchProviderConfig, reserveProviderCall func() (int, bool)) (string, error) {
	if client == nil {
		client = toolHTTPClient
	}
	searchCtx, cancel := context.WithTimeout(ctx, searchRequestTimeout)
	defer cancel()

	if reserveProviderCall != nil {
		used, ok := reserveProviderCall()
		if !ok {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return formatSearchBudgetExhausted(used), nil
		}
	}
	primary := duckDuckGoProvider{}
	primaryResults, primaryErr := primary.Search(searchCtx, query, client)
	primaryResults = qualityGateSearchResults(query, primaryResults)
	if primaryErr == nil && len(primaryResults) > 0 {
		return formatSearchResponse(primary.Name(), false, "", primaryResults), nil
	}
	primaryReason := searchAttemptReason(primary.Name(), primaryErr)
	if err := searchCtx.Err(); err != nil {
		return "", err
	}

	fallback, err := configuredMCPProvider(config)
	if err != nil {
		return formatNoTrustworthyResults(primaryReason, "configured search provider is invalid"), nil
	}
	if reserveProviderCall != nil {
		used, ok := reserveProviderCall()
		if !ok {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return formatSearchBudgetExhausted(used), nil
		}
	}
	fallbackResults, fallbackErr := fallback.Search(searchCtx, query, client)
	fallbackResults = qualityGateSearchResults(query, fallbackResults)
	if fallbackErr == nil && len(fallbackResults) > 0 {
		return formatSearchResponse(fallback.Name(), true, primaryReason, fallbackResults), nil
	}
	if err := searchCtx.Err(); err != nil {
		return "", err
	}
	return formatNoTrustworthyResults(primaryReason, searchAttemptReason(fallback.Name(), fallbackErr)), nil
}

func formatSearchBudgetExhausted(providerCalls int) string {
	result := struct {
		Status        string `json:"status"`
		ProviderCalls int    `json:"provider_calls"`
		Limit         int    `json:"limit"`
		Guidance      string `json:"guidance"`
	}{
		Status:        "budget_exhausted",
		ProviderCalls: providerCalls,
		Limit:         maxSearchProviderCallsPerTask,
		Guidance:      "Use evidence from earlier searches or fetch a known URL; do not retry web_search in this task.",
	}
	raw, _ := json.Marshal(result)
	return string(raw)
}

func parseMCPSearchResponse(raw []byte) ([]searchResult, error) {
	documents, err := decodeMCPDocuments(raw)
	if err != nil {
		return nil, err
	}
	var results []searchResult
	for _, document := range documents {
		parsed, err := collectMCPResults(document)
		if err != nil {
			return nil, err
		}
		results = append(results, parsed...)
	}
	return results, nil
}

func decodeMCPDocuments(raw []byte) ([]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errNoSearchResults
	}
	if looksLikeSSE(trimmed) {
		return decodeSSEDocuments(trimmed)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var documents []any
	for {
		var value any
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("provider response was malformed")
		}
		documents = append(documents, value)
	}
	if len(documents) == 0 {
		return nil, errNoSearchResults
	}
	return documents, nil
}

func looksLikeSSE(raw []byte) bool {
	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		return strings.HasPrefix(line, "data:") ||
			strings.HasPrefix(line, "event:") ||
			strings.HasPrefix(line, "id:") ||
			strings.HasPrefix(line, "retry:") ||
			strings.HasPrefix(line, ":")
	}
	return false
}

func decodeSSEDocuments(raw []byte) ([]any, error) {
	lines := strings.Split(string(raw), "\n")
	var data []string
	var documents []any
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.TrimSpace(strings.Join(data, "\n"))
		data = nil
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		var value any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return errors.New("provider event was malformed")
		}
		documents = append(documents, value)
		return nil
	}
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.TrimSpace(line) == "":
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, errNoSearchResults
	}
	return documents, nil
}

func collectMCPResults(value any) ([]searchResult, error) {
	switch typed := value.(type) {
	case []any:
		var results []searchResult
		for _, item := range typed {
			parsed, err := collectMCPResults(item)
			if err != nil {
				return nil, err
			}
			results = append(results, parsed...)
		}
		return results, nil
	case string:
		return parseMCPText(typed)
	case map[string]any:
		if rawError, ok := typed["error"]; ok && rawError != nil {
			return nil, errors.New("provider returned an error")
		}
		if isError, ok := typed["isError"].(bool); ok && isError {
			return nil, errors.New("provider returned an error")
		}
		if result, ok := typed["result"]; ok {
			return collectMCPResults(result)
		}
		if text, ok := typed["text"].(string); ok {
			return parseMCPText(text)
		}
		if result := mapSearchRecord(typed); result != nil {
			return []searchResult{*result}, nil
		}
		var results []searchResult
		for _, key := range []string{"content", "structuredContent", "results", "items", "data", "records"} {
			child, ok := typed[key]
			if !ok {
				continue
			}
			parsed, err := collectMCPResults(child)
			if err != nil {
				return nil, err
			}
			results = append(results, parsed...)
		}
		return results, nil
	default:
		return nil, nil
	}
}

func mapSearchRecord(value map[string]any) *searchResult {
	urlValue := firstString(value, "url", "link", "source_url", "sourceUrl")
	if strings.TrimSpace(urlValue) == "" {
		return nil
	}
	title := firstString(value, "title", "name")
	abstract := firstString(value, "abstract", "description", "snippet", "excerpt", "excerpts", "text", "content", "highlight")
	if highlights, ok := value["highlights"]; ok {
		if joined := stringValue(highlights); joined != "" {
			abstract = joined
		}
	}
	return &searchResult{Title: title, URL: urlValue, Abstract: abstract}
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := value[key]; ok {
			if text := stringValue(raw); text != "" {
				return text
			}
		}
	}
	return ""
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if part := stringValue(item); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func parseMCPText(text string) ([]searchResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		var value any
		if err := json.Unmarshal([]byte(text), &value); err == nil {
			return collectMCPResults(value)
		}
	}
	results := parseLabeledSearchText(text)
	return results, nil
}

func parseLabeledSearchText(text string) []searchResult {
	var results []searchResult
	var current searchResult
	mode := ""
	flush := func() {
		if strings.TrimSpace(current.URL) != "" {
			results = append(results, current)
		}
		current = searchResult{}
		mode = ""
	}
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "title:"):
			if current.URL != "" {
				flush()
			}
			current.Title = strings.TrimSpace(line[len("title:"):])
			mode = "title"
		case strings.HasPrefix(lower, "url:"):
			if current.URL != "" {
				flush()
			}
			current.URL = strings.TrimSpace(line[len("url:"):])
			mode = "url"
		case strings.HasPrefix(lower, "highlights:") || strings.HasPrefix(lower, "highlight:"):
			mode = "abstract"
			if value := strings.TrimSpace(strings.SplitN(line, ":", 2)[1]); value != "" {
				current.Abstract = value
			}
		case strings.HasPrefix(lower, "text:") || strings.HasPrefix(lower, "snippet:") || strings.HasPrefix(lower, "description:") || strings.HasPrefix(lower, "excerpt:"):
			mode = "abstract"
			if value := strings.TrimSpace(strings.SplitN(line, ":", 2)[1]); value != "" {
				current.Abstract = strings.TrimSpace(current.Abstract + " " + value)
			}
		case mode == "abstract" && line != "":
			current.Abstract = strings.TrimSpace(current.Abstract + " " + strings.TrimLeft(line, "-• "))
		}
	}
	flush()
	return results
}
