package tracker

import (
	"go.kenn.io/kata/internal/httpurl"
	"net"
	"net/url"
)

// ProviderAuthority is public connection metadata, never a credential selector.
// Only providers with operator-selected API/web origins populate these fields.
// Receivers must still route provider I/O to the owner, whose client pins them.
type ProviderAuthority struct {
	APIOrigin string `json:"api_origin,omitempty"`
	WebOrigin string `json:"web_origin,omitempty"`
}

func (a ProviderAuthority) Validate() error {
	for _, origin := range []string{a.APIOrigin, a.WebOrigin} {
		if origin == "" {
			continue
		}
		canonical, err := httpurl.CanonicalHTTPOrigin(origin)
		if err != nil || canonical != origin {
			return ErrInvalid
		}
		parsed, err := url.Parse(origin)
		if err != nil {
			return ErrInvalid
		}
		if parsed.Scheme == "http" {
			ip := net.ParseIP(parsed.Hostname())
			if parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
				return ErrInvalid
			}
		}
	}
	return nil
}
