package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type secureDialPolicy struct {
	Resolver     ipResolver
	Dialer       *net.Dialer
	AllowPrivate func(host string) bool
}

func (p *secureDialPolicy) resolver() ipResolver {
	if p != nil && p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p *secureDialPolicy) dialer() *net.Dialer {
	if p != nil && p.Dialer != nil {
		return p.Dialer
	}
	return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
}

func isProhibitedServerAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return true
	}
	if addr == netip.MustParseAddr("169.254.169.254") || addr == netip.MustParseAddr("100.100.100.200") {
		return true
	}
	return false
}

func (p *secureDialPolicy) resolveAndDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid destination %q: %w", addr, err)
	}
	host = strings.Trim(host, "[]")
	var ips []net.IPAddr
	if parsed, parseErr := netip.ParseAddr(host); parseErr == nil {
		ips = []net.IPAddr{{IP: net.ParseIP(parsed.String())}}
	} else {
		ips, err = p.resolver().LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %q: no addresses", host)
	}
	private, public := false, false
	for _, candidate := range ips {
		ip, ok := netip.AddrFromSlice(candidate.IP)
		if !ok || !ip.IsValid() {
			return nil, fmt.Errorf("resolve %q: invalid address", host)
		}
		if isProhibitedServerAddr(ip) {
			private = true
		} else {
			public = true
		}
	}
	if private && public {
		return nil, fmt.Errorf("destination %q resolves to mixed public and private or local addresses", host)
	}
	if private && (p.AllowPrivate == nil || !p.AllowPrivate(host)) {
		return nil, fmt.Errorf("destination %q resolves to a private or local address", host)
	}
	return p.dialer().DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// SecureTransport performs request authorization around one configured,
// long-lived HTTP transport. The underlying transport is initialized once so
// its idle connection pool survives across RoundTrip calls.
type SecureTransport struct {
	Base      http.RoundTripper
	Policy    *secureDialPolicy
	Authorize func(*url.URL) error

	once       sync.Once
	configured http.RoundTripper
}

func (t *SecureTransport) configure() {
	base, ok := t.Base.(*http.Transport)
	if !ok || base == nil {
		if t.Base != nil {
			t.configured = t.Base
			return
		}
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	// Proxy and dial policy are client-level configuration. Set them before the
	// first request instead of mutating or cloning the transport per RoundTrip.
	base.Proxy = nil
	if t.Policy != nil {
		base.DialContext = t.Policy.resolveAndDial
	}
	t.configured = base
}

func (t *SecureTransport) transport() http.RoundTripper {
	t.once.Do(t.configure)
	return t.configured
}

func (t *SecureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("missing request URL")
	}
	if t.Authorize != nil {
		if err := t.Authorize(req.URL); err != nil {
			return nil, err
		}
	}
	base := t.transport()
	if base == nil {
		return nil, fmt.Errorf("secure transport is not configured")
	}
	return base.RoundTrip(req)
}

func NewSecureHTTPClient(policy *secureDialPolicy, authorize func(*url.URL) error, maxRedirects int) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	secureTransport := &SecureTransport{Base: base, Policy: policy, Authorize: authorize}
	// Initialize before publishing the client so all requests share the same
	// policy-configured transport from the first RoundTrip onward.
	secureTransport.transport()
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: secureTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if maxRedirects > 0 && len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			if req == nil || req.URL == nil {
				return fmt.Errorf("redirect has no URL")
			}
			if authorize != nil {
				return authorize(req.URL)
			}
			return nil
		},
	}
}
