package app

import (
	"capelin-go/internal/contracts"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type appFetchRoundTripper func(*http.Request) (*http.Response, error)

func (f appFetchRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestLocalDispatcherKeepsProviderClientSeparateFromFetchClient(t *testing.T) {
	providerUsed := false
	providerClient := &http.Client{Transport: appFetchRoundTripper(func(req *http.Request) (*http.Response, error) {
		providerUsed = true
		return nil, fmt.Errorf("ordinary provider client must not handle fetch_page")
	})}
	fetchClient := &http.Client{Transport: appFetchRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html><body><h1>isolated fetch</h1></body></html>")),
		}, nil
	})}
	a := &app{
		cfg:       config{allowedTools: map[string]bool{toolFetchPage: true}},
		client:    &client{http: providerClient},
		fetchHTTP: fetchClient,
	}

	result, err := a.runTool(context.Background(), contracts.ToolCall{
		Type: "function",
		Function: contracts.FunctionCall{
			Name:      toolFetchPage,
			Arguments: `{"url":"http://public.test/page"}`,
		},
	})
	if err != nil {
		t.Fatalf("runTool: %v", err)
	}
	if !strings.Contains(result, "# isolated fetch") {
		t.Fatalf("fetch result = %q, want isolated fetch response", result)
	}
	if providerUsed {
		t.Fatal("ordinary local provider client handled fetch_page")
	}
}
