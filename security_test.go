package main

import (
	"bytes"
	"encoding/hex"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type trackingBody struct {
	*bytes.Reader
	read bool
}

func (b *trackingBody) Read(p []byte) (int, error) { b.read = true; return b.Reader.Read(p) }
func (b *trackingBody) Close() error               { return nil }

func TestServerHandlersRejectTargetsBeforeBodyRead(t *testing.T) {
	old := activeServerPolicy
	defer func() { activeServerPolicy = old }()
	p := serverSecurityPolicy{AllowedTargets: map[string]bool{"https://allowed.example": true}}
	activeServerPolicy = &p
	for _, tc := range []struct {
		name    string
		handler func(*httptest.ResponseRecorder, *http.Request)
		path    string
	}{
		{"proxy", func(w *httptest.ResponseRecorder, r *http.Request) { proxyHandler(w, r) }, "/-/"},
		{"sync", func(w *httptest.ResponseRecorder, r *http.Request) { (&app{}).handleChatCompletion(w, r, nil) }, "/"},
		{"async", func(w *httptest.ResponseRecorder, r *http.Request) { (&app{}).handleAsyncChatCompletion(w, r, nil) }, "/async/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackingBody{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1024))}
			r := httptest.NewRequest("POST", tc.path+"?endpoint=https://denied.example/v1", body)
			r.Body = body
			w := httptest.NewRecorder()
			tc.handler(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", w.Code)
			}
			if body.read {
				t.Fatal("handler read body before rejecting target")
			}
		})
	}
}

func TestSecurityOriginsAndTargets(t *testing.T) {
	got, err := parseHTTPOrigin("HTTPS://Example.COM:443/")
	if err != nil || got != "https://example.com" {
		t.Fatalf("origin=%q err=%v", got, err)
	}
	if _, err := parseHTTPOrigin("https://example.com/path"); err == nil {
		t.Fatal("path should be rejected")
	}
	target, err := parseAbsoluteTarget("HTTPS://Example.COM:443/a?token=secret")
	if err != nil || target.Origin != "https://example.com" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	for _, raw := range []string{"ftp://example.com", "https://user:pass@example.com", "https://example.com/#x"} {
		if _, err := parseAbsoluteTarget(raw); err == nil {
			t.Errorf("expected rejection: %s", raw)
		}
	}
	for _, raw := range []string{"https://example.com:", "https://[::1]:"} {
		if _, err := parseHTTPOrigin(raw); err == nil {
			t.Errorf("empty origin port should be rejected: %s", raw)
		}
		if _, err := parseAbsoluteTarget(raw); err == nil {
			t.Errorf("empty target port should be rejected: %s", raw)
		}
	}
}

func TestRejectedTargetLogIsSafeAndCategorized(t *testing.T) {
	p := serverSecurityPolicy{AllowedTargets: map[string]bool{"https://allowed.example": true}}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })

	p.logRejectedTarget("https://denied.example/path?token=secret")
	p.logRejectedTarget("https://user:pass@bad.example:")
	output := logs.String()
	if !strings.Contains(output, "hostname denied.example reason not_allowlisted") ||
		!strings.Contains(output, "hostname bad.example reason invalid") {
		t.Fatalf("unexpected rejection logs: %q", output)
	}
	for _, secret := range []string{"/path", "token=secret", "user", "pass"} {
		if strings.Contains(output, secret) {
			t.Fatalf("rejection log contains sensitive/raw data %q: %q", secret, output)
		}
	}
}

func TestExtractServerTarget(t *testing.T) {
	raw := "https://example.com/v1?token=secret"
	hexPath := "/~" + hex.EncodeToString([]byte(raw))
	got, err := extractServerTarget(hexPath, "", url.Values{})
	if err != nil || got != raw {
		t.Fatalf("hex extraction=%q err=%v", got, err)
	}
	got, err = extractServerTarget("/", "", url.Values{"endpoint": []string{raw}})
	if err != nil || got != raw {
		t.Fatalf("query extraction=%q err=%v", got, err)
	}
	if _, err := extractServerTarget("/https%3A%2F%2Fexample.com", "", url.Values{}); err == nil || !strings.Contains(err.Error(), "literal") {
		t.Fatal("literal path should be rejected")
	}
	if _, err := extractServerTarget("/~zz", "", url.Values{}); err == nil {
		t.Fatal("malformed hex should be rejected")
	}
}

func TestTargetAuthorization(t *testing.T) {
	targets, _, err := parseAllowlist("HTTPS://EXAMPLE.COM:443", false)
	if err != nil {
		t.Fatal(err)
	}
	p := serverSecurityPolicy{AllowedTargets: targets}
	if _, err := p.authorizeTarget("https://example.com/path"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.authorizeTarget("http://example.com/path"); err == nil {
		t.Fatal("scheme mismatch should reject")
	}
	if _, err := p.authorizeTarget("https://example.com:8443/path"); err == nil {
		t.Fatal("port mismatch should reject")
	}
}

func TestWildcardTargetRetainsExactPrivateGate(t *testing.T) {
	targets, all, err := parseAllowlist("*,http://127.0.0.1:11434", false)
	if err != nil || !all || !targets["http://127.0.0.1:11434"] {
		t.Fatalf("wildcard/exact targets=%v all=%v err=%v", targets, all, err)
	}
	p := serverSecurityPolicy{AllowedTargets: targets, AllowAllTargets: all, AllowPrivateTargets: true}
	if _, err := p.authorizeTarget("http://127.0.0.1:11434/v1"); err != nil {
		t.Fatalf("exact private target should pass policy gate: %v", err)
	}
}
