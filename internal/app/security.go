package app

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type serverSecurityPolicy struct {
	AllowedOrigins      map[string]bool
	AllowAllOrigins     bool
	AllowedTargets      map[string]bool
	AllowAllTargets     bool
	AllowPrivateTargets bool
}

type parsedTarget struct {
	URL      *url.URL
	Origin   string
	Hostname string
	Private  bool
}

func loadServerSecurityPolicy(rawOrigins, rawTargets, rawPrivate string) (serverSecurityPolicy, error) {
	origins, allOrigins, err := parseAllowlist(rawOrigins, true)
	if err != nil {
		return serverSecurityPolicy{}, err
	}
	targets, allTargets, err := parseAllowlist(rawTargets, false)
	if err != nil {
		return serverSecurityPolicy{}, err
	}
	allowPrivate, err := parseStrictBool(rawPrivate)
	if err != nil {
		return serverSecurityPolicy{}, fmt.Errorf("SERVER_ALLOW_PRIVATE_TARGETS: %w", err)
	}
	return serverSecurityPolicy{
		AllowedOrigins: origins, AllowAllOrigins: allOrigins,
		AllowedTargets: targets, AllowAllTargets: allTargets,
		AllowPrivateTargets: allowPrivate,
	}, nil
}

func (p serverSecurityPolicy) authorizeOrigin(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", true
	}
	origin, err := parseHTTPOrigin(raw)
	if err != nil || (!p.AllowAllOrigins && !p.AllowedOrigins[origin]) {
		return "", false
	}
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
	if u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("origin hostname or port is missing")
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
		if origin {
			value, err := parseHTTPOrigin(item)
			if err != nil {
				return nil, false, fmt.Errorf("invalid allowlist entry")
			}
			out[value] = true
			continue
		}
		target, err := parseAbsoluteTarget(item)
		if err != nil || target.URL.Path != "" && target.URL.Path != "/" || target.URL.RawQuery != "" || target.URL.Fragment != "" {
			return nil, false, fmt.Errorf("invalid allowlist entry")
		}
		out[target.Origin] = true
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
	if u.User != nil || u.Fragment != "" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return parsedTarget{}, fmt.Errorf("invalid target")
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
	target, err := parseAbsoluteTarget(raw)
	if err != nil {
		return parsedTarget{}, err
	}
	if !p.AllowAllTargets && !p.AllowedTargets[target.Origin] {
		return parsedTarget{}, fmt.Errorf("target not allowed")
	}
	if target.Private && (!p.AllowPrivateTargets || !p.AllowedTargets[target.Origin]) {
		return parsedTarget{}, fmt.Errorf("target not allowed")
	}
	return target, nil
}

func (p serverSecurityPolicy) rejectedTargetReason(raw string) string {
	target, err := parseAbsoluteTarget(raw)
	if err != nil {
		return "invalid"
	}
	if !p.AllowAllTargets && !p.AllowedTargets[target.Origin] {
		return "not_allowlisted"
	}
	if target.Private && (!p.AllowPrivateTargets || !p.AllowedTargets[target.Origin]) {
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
	if origin, err := parseHTTPOrigin(raw); err == nil {
		log.Printf("[capelin-go] rejected origin %s", origin)
		return
	}
	log.Printf("[capelin-go] rejected origin (invalid)")
}

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
