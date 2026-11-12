package mcpevents

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/httpurl"
)

// TrustedCallbackPolicy is immutable after compilation and safe to share.
// Its zero value keeps callbacks public-only.
type TrustedCallbackPolicy struct{ origins map[string][]netip.Addr }

// CompileTrustedCallbacks validates and copies operator grants. Errors never
// include origins or addresses, which may disclose private network details.
func CompileTrustedCallbacks(entries []config.MCPEventsTrustedCallback) (*TrustedCallbackPolicy, error) {
	policy := &TrustedCallbackPolicy{origins: make(map[string][]netip.Addr, len(entries))}
	for i, entry := range entries {
		u, err := parseWebhookCallback(entry.Origin)
		if err != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(entry.Origin, "?#") {
			return nil, fmt.Errorf("invalid mcp_events.trusted_callbacks[%d].origin", i)
		}
		origin := webhookOrigin(u)
		if _, exists := policy.origins[origin]; exists {
			return nil, fmt.Errorf("invalid mcp_events.trusted_callbacks[%d].origin", i)
		}
		if len(entry.Addresses) == 0 {
			return nil, fmt.Errorf("invalid mcp_events.trusted_callbacks[%d].addresses", i)
		}
		seen := make(map[netip.Addr]bool, len(entry.Addresses))
		literal, literalErr := netip.ParseAddr(u.Hostname())
		for j, raw := range entry.Addresses {
			ip, err := netip.ParseAddr(raw)
			if err != nil || ip.Zone() != "" {
				return nil, fmt.Errorf("invalid mcp_events.trusted_callbacks[%d].addresses[%d]", i, j)
			}
			ip = ip.Unmap()
			if ip == netip.MustParseAddr("100.100.100.200") || ip == netip.MustParseAddr("fd00:ec2::254") || ip == netip.MustParseAddr("fd20:ce::254") ||
				(!ip.IsPrivate() && !netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) || seen[ip] {
				return nil, fmt.Errorf("invalid mcp_events.trusted_callbacks[%d].addresses[%d]", i, j)
			}
			if literalErr == nil && literal.Unmap() != ip {
				return nil, fmt.Errorf("invalid mcp_events.trusted_callbacks[%d].addresses[%d]", i, j)
			}
			seen[ip] = true
			policy.origins[origin] = append(policy.origins[origin], ip)
		}
	}
	return policy, nil
}

func webhookOrigin(u *url.URL) string {
	// Normalize literal spelling as well as host case and the HTTPS default port.
	v := *u
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		host := ip.Unmap().String()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if u.Port() != "" {
			host += ":" + u.Port()
		}
		v.Host = host
	}
	origin, _ := httpurl.CanonicalHTTPOrigin(v.String()) // syntax already validated
	return origin
}
