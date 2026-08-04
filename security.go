package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// serverSecurityPolicy contains the independent browser-origin and outbound-target allowlists.
type serverSecurityPolicy struct {
	AllowedOrigins      map[string]bool
	AllowAllOrigins     bool
	AllowedTargets      map[string]bool // canonical origin entries
	AllowAllTargets     bool
	AllowPrivateTargets bool
}

type parsedTarget struct {
	URL      *url.URL
	Origin   string
	Hostname string
	Private  bool
}

func (p serverSecurityPolicy) authorizeOrigin(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", true
	}
	origin, err := parseHTTPOrigin(raw)
	if err != nil || (!p.AllowAllOrigins && !p.AllowedOrigins[origin]) {
		return "", false
	}
	// Return the exact supplied origin for CORS reflection; canonical form is
	// used only for validation and allowlist comparison.
	return strings.TrimSpace(raw), true
}

func canonicalOrigin(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + host
}

func parseHTTPOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("origin is empty")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("origin must be an absolute URL")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("origin scheme must be http or https")
	}
	if u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("origin must not contain credentials, path, query, or fragment")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("origin hostname is missing")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("origin port is empty")
	}
	return canonicalOrigin(u), nil
}

func parseAllowlist(raw string, origin bool) (map[string]bool, bool, error) {
	out := map[string]bool{}
	all := false
	if strings.TrimSpace(raw) == "" {
		return out, false, nil
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, false, fmt.Errorf("empty allowlist entry")
		}
		if item == "*" {
			all = true
			continue
		}
		var v string
		var err error
		if origin {
			v, err = parseHTTPOrigin(item)
		} else {
			p, e := parseAbsoluteTarget(item)
			err = e
			if e == nil {
				if p.URL.Path != "" && p.URL.Path != "/" || p.URL.RawQuery != "" || p.URL.Fragment != "" {
					err = fmt.Errorf("target allowlist entries must be origins without path, query, or fragment")
				}
				v = p.Origin
			}
		}
		if err != nil {
			// Do not include the raw entry in startup diagnostics: URL credentials
			// (and other secrets embedded in malformed values) must not reach logs.
			return nil, false, fmt.Errorf("invalid allowlist entry")
		}
		out[v] = true
	}
	return out, all, nil
}

func (p serverSecurityPolicy) authorizeURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("target not allowed")
	}
	_, err := p.authorizeTarget(u.String())
	return err
}

func (p serverSecurityPolicy) secureHTTPClient() *http.Client {
	private := func(host string) bool {
		if !p.AllowPrivateTargets {
			return false
		}
		for origin := range p.AllowedTargets {
			u, err := url.Parse(origin)
			if err == nil && strings.EqualFold(u.Hostname(), host) {
				return true
			}
		}
		return false
	}
	return NewSecureHTTPClient(&secureDialPolicy{AllowPrivate: private}, p.authorizeURL, 5)
}

func parseStrictBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "false", "0", "no", "off":
		return false, nil
	case "true", "1", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("expected true or false, got %q", raw)
	}
}

func parseAbsoluteTarget(raw string) (parsedTarget, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return parsedTarget{}, fmt.Errorf("target must be an absolute http or https URL")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return parsedTarget{}, fmt.Errorf("target scheme must be http or https")
	}
	if u.User != nil {
		return parsedTarget{}, fmt.Errorf("target credentials are not allowed")
	}
	if u.Fragment != "" {
		return parsedTarget{}, fmt.Errorf("target fragments are not allowed")
	}
	if u.Hostname() == "" {
		return parsedTarget{}, fmt.Errorf("target hostname is missing")
	}
	if strings.HasSuffix(u.Host, ":") {
		return parsedTarget{}, fmt.Errorf("target port is empty")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return parsedTarget{URL: u, Origin: canonicalOrigin(u), Hostname: strings.ToLower(u.Hostname()), Private: isPrivateHostname(u.Hostname())}, nil
}

func isPrivateHostname(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

func (p serverSecurityPolicy) authorizeTarget(raw string) (parsedTarget, error) {
	t, err := parseAbsoluteTarget(raw)
	if err != nil {
		return parsedTarget{}, err
	}
	if !p.AllowAllTargets && !p.AllowedTargets[t.Origin] {
		return parsedTarget{}, fmt.Errorf("target not allowed")
	}
	if t.Private && (!p.AllowPrivateTargets || !p.AllowedTargets[t.Origin]) {
		return parsedTarget{}, fmt.Errorf("target not allowed")
	}
	return t, nil
}

func (p serverSecurityPolicy) rejectedTargetReason(raw string) string {
	t, err := parseAbsoluteTarget(raw)
	if err != nil {
		return "invalid"
	}
	if !p.AllowAllTargets && !p.AllowedTargets[t.Origin] {
		return "not_allowlisted"
	}
	if t.Private && (!p.AllowPrivateTargets || !p.AllowedTargets[t.Origin]) {
		return "private_target"
	}
	return "invalid"
}

func (p serverSecurityPolicy) logRejectedTarget(raw string) {
	hostname := "(unknown)"
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Hostname() != "" {
		hostname = strings.ToLower(u.Hostname())
	}
	log.Printf("[capelin-go] rejected target hostname %s reason %s", hostname, p.rejectedTargetReason(raw))
}

func (p serverSecurityPolicy) logRejectedOrigin(raw string) {
	// Never log credentials, query strings, or other attacker-supplied origin data.
	if origin, err := parseHTTPOrigin(raw); err == nil {
		log.Printf("[capelin-go] rejected origin %s", origin)
		return
	}
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Hostname() != "" {
		log.Printf("[capelin-go] rejected origin hostname %s", strings.ToLower(u.Hostname()))
		return
	}
	log.Printf("[capelin-go] rejected origin (invalid)")
}

// extractServerTarget accepts query endpoint values and ~<hex URL> paths only.
func extractServerTarget(path, endpointPrefix string, query url.Values) (string, error) {
	path = strings.TrimPrefix(path, endpointPrefix)
	if strings.HasPrefix(path, "/~") {
		decoded, err := hex.DecodeString(strings.TrimPrefix(path, "/~"))
		if err != nil {
			return "", fmt.Errorf("invalid hex endpoint")
		}
		return string(decoded), nil
	}
	if path != "" && path != "/" {
		return "", fmt.Errorf("literal URL paths are not supported")
	}
	return strings.TrimSpace(query.Get("endpoint")), nil
}
