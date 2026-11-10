package mcpevents

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"

	"go.kenn.io/kata/internal/httpurl"
)

// TrustedCallback grants one exact HTTPS origin a static set of private peers.
// Only daemon operator configuration should supply these grants.
type TrustedCallback struct {
	Origin    string
	Addresses []string
}

// TrustedCallbackPolicy is immutable after compilation and safe to share.
// Its zero value keeps callbacks public-only.
type TrustedCallbackPolicy struct{ origins map[string][]netip.Addr }

// CompileTrustedCallbacks validates and copies operator grants. Errors never
// include origins or addresses, which may disclose private network details.
func CompileTrustedCallbacks(entries []TrustedCallback) (*TrustedCallbackPolicy, error) {
	policy := &TrustedCallbackPolicy{origins: make(map[string][]netip.Addr, len(entries))}
	invalid := errors.New("invalid trusted callback policy")
	for _, entry := range entries {
		u, err := parseWebhookCallback(entry.Origin)
		if err != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(entry.Origin, "?#") || len(entry.Addresses) == 0 {
			return nil, invalid
		}
		origin := webhookOrigin(u)
		if _, exists := policy.origins[origin]; exists {
			return nil, invalid
		}
		seen := make(map[netip.Addr]bool, len(entry.Addresses))
		literal, literalErr := netip.ParseAddr(u.Hostname())
		for _, raw := range entry.Addresses {
			ip, err := netip.ParseAddr(raw)
			if err != nil || ip.Zone() != "" {
				return nil, invalid
			}
			ip = ip.Unmap()
			if !(ip.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) || seen[ip] {
				return nil, invalid
			}
			if literalErr == nil && literal.Unmap() != ip {
				return nil, invalid
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
