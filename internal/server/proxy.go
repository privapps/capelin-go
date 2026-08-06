package server

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ProxyConfig supplies the application-owned security and transport
// dependencies used by the raw upstream proxy. Delivery, endpoint resolution,
// request limits, header filtering, and response forwarding remain owned by
// this package.
type ProxyConfig struct {
	Client            *http.Client
	AuthorizeTarget   func(string) error
	LogRejectedTarget func(string)
}

// NewProxyHandler returns the security-aware raw upstream proxy handler. The
// supplied client is used for all upstream requests, but redirects are always
// suppressed because the proxy must never silently leave the authorized
// target.
func NewProxyHandler(config ProxyConfig) http.Handler {
	client := config.Client
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	proxyClient := *client
	proxyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleProxy(w, r, proxyClient.Do, config)
	})
}

func handleProxy(w http.ResponseWriter, r *http.Request, do func(*http.Request) (*http.Response, error), config ProxyConfig) {
	target, err := resolveProxyEndpoint(r.URL.Path, r.URL.Query().Get("endpoint"))
	if err != nil || target == "" {
		WriteError(w, http.StatusBadRequest, "endpoint required: use /-/~<hex-encoded-URL> or /-/?endpoint=...")
		return
	}

	parsed, err := parseProxyTarget(target)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "endpoint must be an absolute http or https URL")
		return
	}
	if config.AuthorizeTarget != nil {
		if err := config.AuthorizeTarget(target); err != nil {
			if config.LogRejectedTarget != nil {
				config.LogRejectedTarget(target)
			}
			WriteError(w, http.StatusForbidden, "target not allowed")
			return
		}
	}
	if r.ContentLength > MaxRequestBodySize {
		WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodySize+1))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > MaxRequestBodySize {
		WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid endpoint: "+err.Error())
		return
	}
	copyProxyHeaders(upstream.Header, r.Header)
	upstream.Host = parsed.Host
	response, err := do(upstream)
	if err != nil {
		WriteError(w, http.StatusBadGateway, "proxy request failed: "+err.Error())
		return
	}
	defer response.Body.Close()
	copyProxyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func resolveProxyEndpoint(path, queryEndpoint string) (string, error) {
	path = strings.TrimPrefix(path, "/-")
	if strings.HasPrefix(path, "/~") {
		decoded, err := decodeHexEndpoint(strings.TrimPrefix(path, "/~"))
		if err != nil {
			return "", err
		}
		return decoded, nil
	}
	if path != "" && path != "/" {
		return "", &RequestError{Code: http.StatusBadRequest, Message: "literal URL paths are not supported"}
	}
	return strings.TrimSpace(queryEndpoint), nil
}

func decodeHexEndpoint(value string) (string, error) {
	if value == "" {
		return "", &RequestError{Code: http.StatusBadRequest, Message: "invalid hex endpoint"}
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return "", &RequestError{Code: http.StatusBadRequest, Message: "invalid hex endpoint"}
	}
	return string(decoded), nil
}

func parseProxyTarget(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, errInvalidProxyTarget
	}
	if !strings.EqualFold(target.Scheme, "http") && !strings.EqualFold(target.Scheme, "https") {
		return nil, errInvalidProxyTarget
	}
	if target.User != nil || target.Fragment != "" || target.Hostname() == "" || strings.HasSuffix(target.Host, ":") {
		return nil, errInvalidProxyTarget
	}
	target.Scheme = strings.ToLower(target.Scheme)
	target.Host = strings.ToLower(target.Host)
	return target, nil
}

var errInvalidProxyTarget = &proxyTargetError{}

type proxyTargetError struct{}

func (*proxyTargetError) Error() string { return "invalid proxy target" }

var hopByHopProxyHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

var sensitiveProxyHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Cookie": true,
	"Set-Cookie": true, "WWW-Authenticate": true, "Forwarded": true,
	"X-Forwarded-For": true, "X-Forwarded-Host": true,
	"X-Forwarded-Proto": true, "X-Real-IP": true,
}

func copyProxyHeaders(dst, src http.Header) {
	connectionHeaders := make(map[string]bool)
	for _, value := range src.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			connectionHeaders[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		lower := strings.ToLower(canonical)
		if hopByHopProxyHeaders[canonical] || sensitiveProxyHeaders[canonical] || connectionHeaders[canonical] || strings.HasPrefix(lower, "access-control-") || strings.HasPrefix(lower, "x-forwarded-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
