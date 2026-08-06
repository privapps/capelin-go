package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRawProxyDoesNotInheritCapelinUserAgent(t *testing.T) {
	var userAgent string
	var forwardedHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		forwardedHeader = r.Header.Get("X-Raw-Header")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	oldAllowPrivate := allowPrivateFetch
	allowPrivateFetch = true
	defer func() { allowPrivateFetch = oldAllowPrivate }()

	request := httptest.NewRequest(http.MethodGet, "/-/?endpoint="+url.QueryEscape(upstream.URL), nil)
	request.Header.Set("User-Agent", "raw-client")
	request.Header.Set("X-Raw-Header", "preserved")
	response := httptest.NewRecorder()
	proxyHandler(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("proxy status = %d, body = %s", response.Code, response.Body.String())
	}
	if userAgent != "raw-client" {
		t.Fatalf("raw proxy user agent = %q, want raw-client", userAgent)
	}
	if forwardedHeader != "preserved" {
		t.Fatalf("raw proxy header = %q, want preserved", forwardedHeader)
	}
}
