package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

type staticResolver map[string][]net.IPAddr

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	v, ok := r[host]
	if !ok {
		return nil, errors.New("not found")
	}
	return v, nil
}

func TestIsProhibitedServerAddr(t *testing.T) {
	for _, tc := range []struct {
		name, ip string
		bad      bool
	}{
		{"public4", "8.8.8.8", false},
		{"public6", "2001:4860:4860::8888", false},
		{"loopback", "127.0.0.1", true},
		{"private", "10.0.0.1", true},
		{"linklocal", "169.254.1.1", true},
		{"metadata", "169.254.169.254", true},
		{"multicast", "224.0.0.1", true},
		{"unspecified", "::", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := isProhibitedServerAddr(mustAddr(t, tc.ip))
			if got != tc.bad {
				t.Fatalf("isProhibitedServerAddr(%s) = %v, want %v", tc.ip, got, tc.bad)
			}
		})
	}
}

func mustAddr(t *testing.T, s string) (a netip.Addr) {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSecureDialPolicyRejectsMixedAnswers(t *testing.T) {
	p := &secureDialPolicy{Resolver: staticResolver{"mixed.test": {
		{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")},
	}}}
	_, err := p.resolveAndDial(context.Background(), "tcp", "mixed.test:443")
	if err == nil {
		t.Fatal("mixed public/private answers were accepted")
	}
}

func TestSecureDialPolicyPrivateRequiresGate(t *testing.T) {
	p := &secureDialPolicy{Resolver: staticResolver{"local.test": {{IP: net.ParseIP("127.0.0.1")}}}}
	if _, err := p.resolveAndDial(context.Background(), "tcp", "local.test:1"); err == nil {
		t.Fatal("private destination accepted without gate")
	}
	p.AllowPrivate = func(host string) bool { return host == "local.test" }
	// Port 1 is expected to refuse the connection; importantly, the policy
	// check must pass and reach the dialer rather than report a private error.
	if _, err := p.resolveAndDial(context.Background(), "tcp", "local.test:1"); err == nil || strings.Contains(err.Error(), "private or local") {
		t.Fatalf("private gate was not honored: %v", err)
	}
}

func TestSecureRedirectReauthorizes(t *testing.T) {
	called := ""
	check := SecureCheckRedirect(func(u *url.URL) error { called = u.Host; return nil }, 3)
	req, _ := http.NewRequest("GET", "https://next.example/path", nil)
	if err := check(req, nil); err != nil {
		t.Fatal(err)
	}
	if called != "next.example" {
		t.Fatalf("authorize host = %q", called)
	}
}
