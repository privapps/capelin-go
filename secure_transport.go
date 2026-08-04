package main

// The server outbound transport lives in this file so that all of the server
// request paths (proxy, synchronous LLM, and asynchronous LLM) can share the
// same DNS and address policy.  In particular, validation is performed at the
// point of dialing as well as before a request is made; this closes the DNS
// rebinding window between URL validation and connection establishment.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ipResolver is deliberately small to make DNS behaviour deterministic in
// tests (and to permit callers to provide a resolver backed by their own DNS
// policy).
type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// secureDialPolicy controls whether addresses classified as local/private may
// be used. AllowPrivate must be checked against the exact origin by the caller;
// a wildcard policy must never return true here.
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

// isProhibitedServerAddr identifies destinations which are not public unicast
// addresses. Unmapping IPv4-mapped IPv6 values is important because URL hosts
// and DNS APIs may represent the same address in either form.
func isProhibitedServerAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return true
	}
	// Documentation and benchmarking ranges are not routable destinations.
	if addr.Is4() {
		a := addr.As4()
		if a[0] == 192 && a[1] == 0 && a[2] == 2 || a[0] == 198 && a[1] == 51 && a[2] == 100 || a[0] == 203 && a[1] == 0 && a[2] == 113 {
			return true
		}
	}
	// IPv4 metadata endpoint (169.254.169.254) is already link-local, but keep
	// this explicit for readability and for future classification changes.
	if addr == netip.MustParseAddr("169.254.169.254") || addr == netip.MustParseAddr("100.100.100.200") {
		return true
	}
	return false
}

func hostWithoutPort(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(addr, "[]")
}

// resolveAndDial resolves all answers, rejects a mixed public/prohibited set,
// and then dials one of the validated addresses. The original hostname is
// retained by net/http for Host and TLS SNI because only DialContext's socket
// address is replaced.
func (p *secureDialPolicy) resolveAndDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid destination %q: %w", addr, err)
	}
	host = strings.Trim(host, "[]")
	var ips []net.IPAddr
	if parsed, err := netip.ParseAddr(host); err == nil {
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
	private := false
	public := false
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
	// Once every answer has passed policy, pin the connection to the first
	// answer. No second hostname lookup is performed by the dialer.
	return p.dialer().DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// SecureTransport is an http.RoundTripper with DNS pinning. Authorize is
// called for every request, including redirects via SecureCheckRedirect.
type SecureTransport struct {
	Base      http.RoundTripper
	Policy    *secureDialPolicy
	Authorize func(*url.URL) error
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
	base, ok := t.Base.(*http.Transport)
	if !ok || base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	// Never honor process-wide proxy environment variables for secure outbound
	// requests. A proxy would perform its own DNS resolution and bypass the
	// address validation and DNS pinning enforced by Policy's DialContext.
	base.Proxy = nil
	if t.Policy == nil {
		return base.RoundTrip(req)
	}
	tr := base.Clone()
	tr.DialContext = t.Policy.resolveAndDial
	return tr.RoundTrip(req)
}

// NewSecureHTTPClient builds the shared client used by server outbound paths.
// The transport is constructed once, retaining connection pooling while its
// DialContext performs a fresh validated resolution for each new connection.
func NewSecureHTTPClient(policy *secureDialPolicy, authorize func(*url.URL) error, maxRedirects int) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	// ProxyFromEnvironment would route around resolveAndDial (and let the proxy
	// resolve the destination), defeating private-address and DNS-rebinding
	// protections.
	base.Proxy = nil
	if policy != nil {
		base.DialContext = policy.resolveAndDial
	}
	tr := &SecureTransport{Base: base, Policy: nil, Authorize: authorize}
	return &http.Client{Timeout: requestTimeout, Transport: tr, CheckRedirect: SecureCheckRedirect(authorize, maxRedirects)}
}

// SecureCheckRedirect reauthorizes every redirect before net/http starts the
// next request. DNS/address checks remain in SecureTransport's DialContext.
func SecureCheckRedirect(authorize func(*url.URL) error, max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if max > 0 && len(via) >= max {
			return fmt.Errorf("too many redirects")
		}
		if req == nil || req.URL == nil {
			return fmt.Errorf("redirect has no URL")
		}
		if authorize != nil {
			if err := authorize(req.URL); err != nil {
				return err
			}
		}
		return nil
	}
}

// cloneSecureTLSConfig is useful to callers constructing transports while
// preserving hostname-based certificate verification (never set InsecureSkipVerify
// merely because the socket is pinned to an IP).
func cloneSecureTLSConfig(base *tls.Config) *tls.Config {
	if base == nil {
		return nil
	}
	return base.Clone()
}
