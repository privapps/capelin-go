package app

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

var activeServerPolicy *serverSecurityPolicy

// Kept for compatibility with the legacy application tests and network seam.
var allowPrivateFetch = false

var proxyHTTPClient = NewSecureHTTPClient(&secureDialPolicy{
	AllowPrivate: func(string) bool { return allowPrivateFetch },
}, nil, 5)

func init() {
	proxyHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
}

var hopByHopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

func copyProxyHeaders(dst, src http.Header) {
	connectionHeaders := map[string]bool{}
	for _, value := range src.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			connectionHeaders[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		lower := strings.ToLower(canonical)
		if hopByHopHeaders[canonical] || connectionHeaders[canonical] || strings.HasPrefix(lower, "access-control-") || strings.HasPrefix(lower, "x-forwarded-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

var sensitiveProxyHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Cookie": true,
	"X-Forwarded-For": true, "X-Forwarded-Host": true, "X-Forwarded-Proto": true,
	"X-Real-IP": true, "Forwarded": true,
}

func proxyTarget(r *http.Request) (string, error) {
	return extractServerTarget(r.URL.Path, "/-", r.URL.Query())
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	target, err := proxyTarget(r)
	if err != nil || target == "" {
		writeError(w, http.StatusBadRequest, "endpoint required: use /-/~<hex-encoded-URL> or /-/?endpoint=...")
		return
	}
	parsed, err := parseAbsoluteTarget(target)
	if activeServerPolicy != nil {
		if authorized, authErr := activeServerPolicy.authorizeTarget(target); authErr != nil {
			activeServerPolicy.logRejectedTarget(target)
			writeError(w, http.StatusForbidden, "target not allowed")
			return
		} else {
			parsed = authorized
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "endpoint must be an absolute http or https URL")
		return
	}
	if r.ContentLength > 10*1024*1024 {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024+1))
	if readErr != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > 10*1024*1024 {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, parsed.URL.String(), io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid endpoint: "+err.Error())
		return
	}
	copyProxyHeaders(upstream.Header, r.Header)
	for key := range sensitiveProxyHeaders {
		upstream.Header.Del(key)
	}
	upstream.Host = parsed.URL.Host
	resp, err := proxyHTTPClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "proxy request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	copyProxyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
