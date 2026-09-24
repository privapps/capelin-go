package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const (
	fetchFailureNotFound  = "not_found"
	fetchFailureForbidden = "forbidden"
	fetchFailureRateLimit = "rate_limited"
	fetchFailureServer    = "server_error"
	fetchFailureTimeout   = "timeout"
	fetchFailureCancelled = "cancelled"
	fetchFailureRedirect  = "redirect"
	fetchFailureNetwork   = "network_error"
	fetchFailureHTTP      = "http_error"
)

type fetchFailure struct {
	category      string
	httpStatus    int
	policyBlocked bool
}

// fetchArgumentError is intentionally bounded. URL validation errors must be
// distinguishable from safe-network policy failures so the application can
// offer corrected-argument recovery without echoing a URL that may contain
// credentials or other sensitive query data.
type fetchArgumentError struct{}

func (*fetchArgumentError) Error() string {
	return "fetch page: invalid or unsupported URL argument"
}

func (failure *fetchFailure) Error() string {
	if failure == nil {
		return "source fetch failed"
	}
	if failure.policyBlocked {
		return "fetch page: private or local or otherwise unsafe destination was blocked by policy"
	}
	return "source fetch failed: " + failure.category
}

func (failure *fetchFailure) output(previouslyFailed bool) string {
	if failure == nil {
		return ""
	}
	status := "fetch_failed"
	if failure.policyBlocked {
		status = "fetch_blocked"
	}
	result := struct {
		Status           string `json:"status"`
		Category         string `json:"category"`
		HTTPStatus       int    `json:"http_status,omitempty"`
		PreviouslyFailed bool   `json:"previously_failed,omitempty"`
		Guidance         string `json:"guidance"`
	}{
		Status:           status,
		Category:         failure.category,
		HTTPStatus:       failure.httpStatus,
		PreviouslyFailed: previouslyFailed,
		Guidance:         fetchFailureGuidance(failure.category, failure.httpStatus, previouslyFailed),
	}
	raw, _ := json.Marshal(result)
	return string(raw)
}

func fetchFailureGuidance(category string, status int, previouslyFailed bool) string {
	if previouslyFailed {
		return "This URL already failed in this task; choose another URL from recent web_search results when available. Valid public URLs supplied directly by the user are also allowed."
	}
	switch category {
	case "policy_blocked":
		return "This URL or redirect was rejected by the safe-network policy; do not retry it. Choose another public URL from recent web_search results when available."
	case fetchFailureNotFound:
		if status == http.StatusGone {
			return "The source is gone (410); choose another URL from recent web_search results when available."
		}
		return "The source was not found (404); choose another URL from recent web_search results when available."
	case fetchFailureForbidden:
		return "The source denied access (403); this is a source restriction, not a tool permission failure. Choose another search result."
	case fetchFailureRateLimit:
		return "The source is rate limiting requests (429); wait or use another search result instead of retrying this URL."
	case fetchFailureServer:
		return "The source is temporarily unavailable; use another search result or retry later."
	case fetchFailureTimeout:
		return "The source fetch timed out; use another search result or retry later, not the same URL immediately."
	case fetchFailureCancelled:
		return "The source fetch was cancelled; do not retry it automatically."
	case fetchFailureRedirect:
		return "The source redirect could not be followed safely; choose another public URL from search results."
	case fetchFailureNetwork:
		return "The source could not be reached; use another search result or retry later."
	default:
		return "The source could not be fetched; use another search result or retry later."
	}
}

func fetchFailureForStatus(status int) *fetchFailure {
	failure := &fetchFailure{httpStatus: status}
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		failure.category = fetchFailureNotFound
	case status == http.StatusForbidden:
		failure.category = fetchFailureForbidden
	case status == http.StatusTooManyRequests:
		failure.category = fetchFailureRateLimit
	case status >= http.StatusInternalServerError && status < 600:
		failure.category = fetchFailureServer
	case status >= 300 && status < 400:
		failure.category = fetchFailureRedirect
	default:
		failure.category = fetchFailureHTTP
	}
	return failure
}

func fetchFailureForTransport(ctx context.Context, err error) error {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return &fetchFailure{category: fetchFailureCancelled}
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &fetchFailure{category: fetchFailureTimeout}
	}
	var requestError *url.Error
	if errors.As(err, &requestError) {
		cause := strings.ToLower(requestError.Err.Error())
		if strings.Contains(cause, "too many redirects") {
			return &fetchFailure{category: fetchFailureRedirect}
		}
		if strings.Contains(cause, "redirect") || strings.Contains(cause, "private or local") {
			return &fetchFailure{category: "policy_blocked", policyBlocked: true}
		}
		var networkError net.Error
		if errors.As(requestError.Err, &networkError) {
			if networkError.Timeout() {
				return &fetchFailure{category: fetchFailureTimeout}
			}
			return &fetchFailure{category: fetchFailureNetwork}
		}
		// Certificate and other origin-side failures are source-network
		// failures, not safe-network policy rejections.
		return &fetchFailure{category: fetchFailureNetwork}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return &fetchFailure{category: fetchFailureTimeout}
		}
		return &fetchFailure{category: fetchFailureNetwork}
	}
	return &fetchFailure{category: fetchFailureNetwork}
}

func (task *SearchTask) fetch(ctx context.Context, targetURL string, client *http.Client) (string, error) {
	if task == nil {
		return runFetchPageWithClient(ctx, targetURL, client)
	}
	key := normalizeFailedFetchURL(targetURL)

	task.mu.Lock()
	if failure, ok := task.failedFetches[key]; ok {
		task.mu.Unlock()
		return failure.output(true), nil
	}
	if flight, ok := task.fetchFlights[key]; ok {
		task.mu.Unlock()
		select {
		case <-flight.done:
			return flight.result, flight.err
		case <-ctx.Done():
			failure := fetchFailureForTransport(ctx, ctx.Err())
			var classified *fetchFailure
			if errors.As(failure, &classified) {
				return classified.output(false), nil
			}
			return "", failure
		}
	}
	flight := &searchTaskFlight{done: make(chan struct{})}
	task.fetchFlights[key] = flight
	task.mu.Unlock()

	result, fetchErr := runFetchPageWithClient(ctx, targetURL, client)
	var failure *fetchFailure
	if errors.As(fetchErr, &failure) {
		task.mu.Lock()
		task.failedFetches[key] = *failure
		task.mu.Unlock()
		result, fetchErr = failure.output(false), nil
	}
	task.mu.Lock()
	flight.result, flight.err = result, fetchErr
	delete(task.fetchFlights, key)
	close(flight.done)
	task.mu.Unlock()
	return result, fetchErr
}

func normalizeFailedFetchURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	invalidKey := func() string {
		digest := sha256.Sum256([]byte(trimmed))
		return "invalid:" + hex.EncodeToString(digest[:])
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return invalidKey()
	}
	user := parsed.User
	parsed.User = nil
	canonical, err := normalizeSearchURL(parsed.String())
	if err != nil {
		return invalidKey()
	}
	canonicalURL, err := url.Parse(canonical)
	if err != nil {
		return invalidKey()
	}
	canonicalURL.User = user
	return canonicalURL.String()
}
