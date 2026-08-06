package tools

import (
	"capelin-go/internal/contracts"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fetchRoundTripper func(*http.Request) (*http.Response, error)

func (f fetchRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func fetchResponse(status int, headers http.Header, body string) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestFetchPageRejectsPrivateRedirectBeforeDestination(t *testing.T) {
	oldClient, oldAllow := toolHTTPClient, allowPrivateFetch
	t.Cleanup(func() {
		toolHTTPClient = oldClient
		allowPrivateFetch = oldAllow
	})
	allowPrivateFetch = false
	privateHits := 0
	toolHTTPClient = &http.Client{
		Transport: fetchRoundTripper(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Host {
			case "public.test":
				if req.URL.Path == "/start" {
					return fetchResponse(http.StatusFound, http.Header{"Location": []string{"http://127.0.0.1:18080/private"}}, "redirect"), nil
				}
				return fetchResponse(http.StatusOK, http.Header{"Content-Type": []string{"text/html"}}, "<p>recovered public page</p>"), nil
			case "127.0.0.1:18080":
				privateHits++
				return fetchResponse(http.StatusOK, nil, "private body"), nil
			default:
				return nil, fmt.Errorf("unexpected destination %s", req.URL)
			}
		}),
		CheckRedirect: checkFetchRedirect,
	}

	_, err := runFetchPage(context.Background(), "http://public.test/start")
	if err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("fetch error = %v, want private-target rejection", err)
	}
	if privateHits != 0 {
		t.Fatalf("private redirect handler received %d requests", privateHits)
	}
	got, err := runFetchPage(context.Background(), "http://public.test/recovery")
	if err != nil {
		t.Fatalf("recovery fetch: %v", err)
	}
	if !strings.Contains(got, "recovered public page") {
		t.Fatalf("recovery output = %q", got)
	}
}

func TestFetchPageFollowsAllowedRedirectAndExtractsContent(t *testing.T) {
	oldClient, oldAllow := toolHTTPClient, allowPrivateFetch
	t.Cleanup(func() {
		toolHTTPClient = oldClient
		allowPrivateFetch = oldAllow
	})
	allowPrivateFetch = false
	toolHTTPClient = &http.Client{
		Transport: fetchRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "public.test" {
				return nil, fmt.Errorf("unexpected destination %s", req.URL)
			}
			if req.URL.Path == "/start" {
				return fetchResponse(http.StatusFound, http.Header{"Location": []string{"/final"}}, "redirect"), nil
			}
			return fetchResponse(http.StatusOK, http.Header{"Content-Type": []string{"text/html"}}, "<html><body><h1>Public page</h1></body></html>"), nil
		}),
		CheckRedirect: checkFetchRedirect,
	}

	got, err := runFetchPage(context.Background(), "http://public.test/start")
	if err != nil {
		t.Fatalf("runFetchPage: %v", err)
	}
	if !strings.Contains(got, "# Public page") {
		t.Fatalf("fetch output = %q, want extracted heading", got)
	}
}

func TestDispatcherUsesFetchClientInsteadOfProviderClient(t *testing.T) {
	providerUsed := false
	fetchClient := &http.Client{
		Transport: fetchRoundTripper(func(req *http.Request) (*http.Response, error) {
			return fetchResponse(http.StatusOK, http.Header{"Content-Type": []string{"text/html"}}, "<p>fetch client</p>"), nil
		}),
		CheckRedirect: checkFetchRedirect,
	}
	providerClient := &http.Client{
		Transport: fetchRoundTripper(func(req *http.Request) (*http.Response, error) {
			providerUsed = true
			return nil, fmt.Errorf("ordinary provider client must not handle fetch_page")
		}),
	}
	dispatcher := Dispatcher{
		HTTPClient:      providerClient,
		FetchHTTPClient: fetchClient,
		Hooks:           Hooks{IsEnabled: func(any, string) bool { return true }},
	}

	call := contracts.ToolCall{
		Type: "function",
		Function: contracts.FunctionCall{
			Name:      FetchPage,
			Arguments: `{"url":"http://public.test/page"}`,
		},
	}
	got, err := dispatcher.Run(context.Background(), struct{}{}, call)
	if err != nil {
		t.Fatalf("dispatcher.Run: %v", err)
	}
	if !strings.Contains(got, "fetch client") {
		t.Fatalf("fetch output = %q, want fetch client response", got)
	}
	if providerUsed {
		t.Fatal("ordinary provider client handled fetch_page")
	}
}
