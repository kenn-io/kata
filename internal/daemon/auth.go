package daemon

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const (
	authBearerPrefix     = "Bearer "
	authHeader           = "Authorization"
	pathPing             = "/api/v1/ping"
	pathHealth           = "/api/v1/health"
	pathEventsStreamPath = "/api/v1/events/stream"
)

// authPolicy is the resolved auth posture at daemon start. Token == "" disables
// bearer auth; TrustPrivateNetwork is the explicit operator opt-in that allows
// token auth on non-loopback private-network TCP; AllowUnauthenticatedPrivateNetworkWrites
// moves the local no-token trust model onto a literal private IP bind; InsecureReadonly
// is the dev escape hatch that allows GETs on non-loopback TCP without a token.
// These fields are also surfaced through ServerConfig; this struct exists so
// the middleware itself does not depend on ServerConfig.
type authPolicy struct {
	Token                                    string
	TrustPrivateNetwork                      bool
	AllowUnauthenticatedPrivateNetworkWrites bool
	InsecureReadonly                         bool
	RequireTokenIdentity                     bool

	// SelfAuthenticatedRoutes is the set of routes whose handler owns
	// authentication; the middleware must not pre-empt them with a daemon-token
	// check. NewServer generates it from the registered routes. The zero value
	// matches nothing, so a caller that does not set it gets bearer enforcement
	// on every route.
	SelfAuthenticatedRoutes selfAuthenticatedRouteMatcher
}

// requireBearer returns an HTTP middleware that enforces bearer-token auth
// per the spec matrix:
//
//	Token == "" && !InsecureReadonly  -> no-op (local-socket / loopback deployments)
//	Token == "" &&  InsecureReadonly  -> GETs pass; mutations + SSE return 401
//	Token != ""                       -> all non-health paths require Bearer == Token
//
// /api/v1/ping and /api/v1/health bypass unconditionally so health-check probes
// do not need credentials.
func requireBearer(p authPolicy, tokenStores ...db.Storage) func(http.Handler) http.Handler {
	var tokenStore db.Storage
	if len(tokenStores) > 0 {
		tokenStore = tokenStores[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Non-API paths can only reach the data-free embedded web handler.
			// Keep the static shell public; listener Host policy still wraps it.
			if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api" &&
				r.URL.Path != "/openapi.yaml" && r.URL.Path != "/openapi.json" {
				next.ServeHTTP(w, r)
				return
			}
			if webSessionAuthenticated(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == pathPing || r.URL.Path == pathHealth || isWebSessionBootstrapRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			if p.SelfAuthenticatedRoutes.matches(r) {
				next.ServeHTTP(w, r)
				return
			}
			if p.RequireTokenIdentity {
				requireIdentityBearer(w, r, next, p, tokenStore)
				return
			}
			if p.Token == "" {
				if p.AllowUnauthenticatedPrivateNetworkWrites && isTokenAdminPath(r.URL.Path) {
					api.WriteEnvelope(w, http.StatusUnauthorized, "auth_required",
						"token administration requires authentication; daemon allows unauthenticated private-network writes")
					return
				}
				if !p.InsecureReadonly {
					next.ServeHTTP(w, r)
					return
				}
				if r.Method != http.MethodGet ||
					strings.HasPrefix(r.URL.Path, pathEventsStreamPath) ||
					isTokenAdminPath(r.URL.Path) {
					api.WriteEnvelope(w, http.StatusUnauthorized, "auth_required",
						"mutations and event stream require authentication; daemon is in --insecure-readonly mode")
					return
				}
				next.ServeHTTP(w, r.WithContext(withInsecureReadonlyRequest(r.Context())))
				return
			}
			got := r.Header.Get(authHeader)
			if !strings.HasPrefix(got, authBearerPrefix) {
				api.WriteEnvelope(w, http.StatusUnauthorized, "auth_required",
					"Authorization bearer required")
				return
			}
			presented := strings.TrimPrefix(got, authBearerPrefix)
			if subtle.ConstantTimeCompare([]byte(presented), []byte(p.Token)) != 1 {
				api.WriteEnvelope(w, http.StatusForbidden, "auth_invalid", "token mismatch")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{
				Kind: PrincipalStaticToken,
			})))
		})
	}
}

func requireIdentityBearer(w http.ResponseWriter, r *http.Request, next http.Handler, p authPolicy, tokenStore db.Storage) {
	got := r.Header.Get(authHeader)
	if !strings.HasPrefix(got, authBearerPrefix) {
		api.WriteEnvelope(w, http.StatusUnauthorized, "auth_required",
			"Authorization bearer required")
		return
	}
	presented := strings.TrimPrefix(got, authBearerPrefix)
	if p.Token != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(p.Token)) == 1 {
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{
			Kind: PrincipalBootstrap,
		})))
		return
	}
	if tokenStore == nil {
		api.WriteEnvelope(w, http.StatusInternalServerError, "internal",
			"token identity requires a database")
		return
	}
	tok, err := tokenStore.ResolveAPIToken(r.Context(), presented)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			api.WriteEnvelope(w, http.StatusInternalServerError, "internal",
				"token identity lookup failed")
			return
		}
		api.WriteEnvelope(w, http.StatusForbidden, "token_invalid", "token invalid")
		return
	}
	next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principalFromAPIToken(tok))))
}

func isTokenAdminPath(path string) bool {
	return path == "/api/v1/tokens" || strings.HasPrefix(path, "/api/v1/tokens/")
}

// checkAuthStartup refuses startup when the listen address would expose
// the daemon to plaintext-on-the-wire access. listen uses the same
// convention as runDaemonWithListen: "" means platform-default local
// transport; "host:port" means TCP. The matrix on non-loopback TCP is:
//
//	Token != "" && TrustPrivateNetwork                 -> permit (operator accepts private-network confidentiality)
//	Token != "" && !TrustPrivateNetwork                -> REFUSE (token would travel in cleartext)
//	Token == "" && AllowUnauthenticatedPrivateNetworkWrites -> permit only on literal private IP binds
//	Token == "" &&  InsecureReadonly                   -> permit (dev-only GET access)
//	Token == "" && !InsecureReadonly                   -> REFUSE (would expose mutations to the LAN)
//
// The daemon does not terminate TLS, so a bearer token on plaintext non-
// loopback HTTP is a passive-capture risk. Operators wanting cross-host
// access must either tunnel via SSH (loopback on both ends) or front the
// daemon with a TLS-terminating reverse proxy and bind the daemon to a
// Unix socket or 127.0.0.1.
func checkAuthStartup(listen string, p authPolicy) error {
	if p.AllowUnauthenticatedPrivateNetworkWrites && p.Token != "" {
		return fmt.Errorf("allow_unauthenticated_private_network_writes requires no auth token")
	}
	if p.AllowUnauthenticatedPrivateNetworkWrites && p.RequireTokenIdentity {
		return fmt.Errorf("allow_unauthenticated_private_network_writes cannot be combined with require_token_identity")
	}
	if p.AllowUnauthenticatedPrivateNetworkWrites && p.InsecureReadonly {
		return fmt.Errorf("allow_unauthenticated_private_network_writes cannot be combined with --insecure-readonly")
	}
	if p.RequireTokenIdentity && p.Token == "" {
		return fmt.Errorf("require_token_identity requires a bootstrap token")
	}
	if p.RequireTokenIdentity && p.InsecureReadonly {
		return fmt.Errorf("require_token_identity cannot be combined with --insecure-readonly")
	}
	if p.AllowUnauthenticatedPrivateNetworkWrites {
		if isLiteralPrivateNetworkTCP(listen) {
			return nil
		}
		return fmt.Errorf("listen %q with unauthenticated writes requires a literal private IP bind "+
			"(RFC1918, CGNAT, link-local, or ULA); hostnames, public IPs, and wildcard binds are not supported", listen)
	}
	if !isNonLoopbackTCP(listen) {
		return nil
	}
	if p.Token != "" {
		if p.TrustPrivateNetwork {
			return nil
		}
		return fmt.Errorf("non-loopback TCP listen %q with a bearer token is not "+
			"supported — the daemon does not terminate TLS, so the token would "+
			"travel over plaintext HTTP; bind to a Unix socket or 127.0.0.1 and "+
			"tunnel via SSH or a TLS-terminating reverse proxy", listen)
	}
	if p.InsecureReadonly {
		return nil
	}
	return fmt.Errorf("non-loopback TCP listen %q is not supported — "+
		"bind to a Unix socket or 127.0.0.1, or pass --insecure-readonly "+
		"for dev-only GET access (no mutations)", listen)
}

// CheckAuthStartup is the exported form used by the CLI entry point.
func CheckAuthStartup(listen string, auth config.AuthConfig, insecureReadonly bool) error {
	return checkAuthStartup(listen, authPolicy{
		Token:                                    auth.Token,
		TrustPrivateNetwork:                      auth.TrustPrivateNetwork,
		AllowUnauthenticatedPrivateNetworkWrites: auth.AllowUnauthenticatedPrivateNetworkWrites,
		InsecureReadonly:                         insecureReadonly,
		RequireTokenIdentity:                     auth.RequireTokenIdentity,
	})
}

// TrustPrivateNetworkWarning returns the startup warning shown when the daemon
// is configured to send bearer tokens over trusted private-network HTTP.
func TrustPrivateNetworkWarning(listen string, auth config.AuthConfig) (string, bool) {
	if !isNonLoopbackTCP(listen) || auth.Token == "" || !auth.TrustPrivateNetwork {
		return "", false
	}
	return "kata daemon: WARNING: listening on non-loopback TCP with bearer auth; " +
		"operator has asserted private-network confidentiality.", true
}

// UnauthenticatedPrivateNetworkWritesWarning returns the startup warning shown
// when the daemon is configured to accept writes from a private-network TCP bind
// without bearer authentication.
func UnauthenticatedPrivateNetworkWritesWarning(listen string, auth config.AuthConfig) (string, bool) {
	if auth.Token != "" || !auth.AllowUnauthenticatedPrivateNetworkWrites || !isLiteralPrivateNetworkTCP(listen) {
		return "", false
	}
	return "kata daemon: WARNING: listening on private-network TCP with unauthenticated writes; " +
		"any device that can reach this address can mutate data, open the event stream, " +
		"and assert client-supplied actors. Token administration remains blocked.", true
}

// isNonLoopbackTCP reports whether listen designates a TCP bind that's
// reachable from anywhere but loopback. Empty listen (platform-default local
// transport) returns false. Hosts that resolve to loopback IPs return false.
// Wildcard binds (empty host, 0.0.0.0, ::) and non-loopback IPs / unknown
// hostnames return true so the auth-startup check defaults to "needs a token"
// for anything that could plausibly be reached from another machine on the
// same network.
func isNonLoopbackTCP(listen string) bool {
	if listen == "" {
		return false
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	// Empty host means ":port" — net.Listen binds every interface. 0.0.0.0
	// and :: are the IPv4 / IPv6 wildcards. All three are non-loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	// Non-IP, non-localhost hostname — we can't safely resolve here without
	// DNS, so treat as non-loopback. Operators can use 127.0.0.1 / ::1
	// explicitly if they want the loopback-only path.
	return true
}

func isLiteralPrivateNetworkTCP(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnatBlock.Contains(ip)
}

// cgnatBlock is RFC6598 100.64.0.0/10. Go's net.IP.IsPrivate() intentionally
// excludes it, but private overlay networks commonly use this range.
var cgnatBlock = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}
