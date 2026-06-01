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
// token auth on non-loopback private-network TCP; InsecureReadonly is the dev
// escape hatch that allows GETs on non-loopback TCP without a token. These
// fields are also surfaced through ServerConfig; this struct exists so the
// middleware itself does not depend on ServerConfig.
type authPolicy struct {
	Token                string
	TrustPrivateNetwork  bool
	InsecureReadonly     bool
	RequireTokenIdentity bool
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
			if r.URL.Path == pathPing || r.URL.Path == pathHealth {
				next.ServeHTTP(w, r)
				return
			}
			if isFederationTransportRoute(r.Method, r.URL.Path) ||
				isClaimActionRoute(r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if p.RequireTokenIdentity {
				requireIdentityBearer(w, r, next, p, tokenStore)
				return
			}
			if p.Token == "" {
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
				next.ServeHTTP(w, r)
				return
			}
			got := r.Header.Get(authHeader)
			if !strings.HasPrefix(got, authBearerPrefix) {
				api.WriteEnvelope(w, http.StatusUnauthorized, "auth_required",
					"Authorization: Bearer <token> required")
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
			"Authorization: Bearer <token> required")
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

func isClaimActionRoute(method, path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/v1/projects/")
	if !ok {
		return false
	}
	projectID, rest, ok := strings.Cut(rest, "/issues/")
	if !ok || projectID == "" {
		return false
	}
	for _, r := range projectID {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, suffix, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	if method == http.MethodGet {
		return suffix == "lease"
	}
	if method != http.MethodPost {
		return false
	}
	switch suffix {
	case "lease/actions/acquire", "lease/actions/renew", "lease/actions/release", "lease/actions/force_release":
		return true
	default:
		return false
	}
}

func isFederationTransportRoute(method, path string) bool {
	var suffix string
	switch method {
	case http.MethodGet:
		if strings.HasSuffix(path, "/federation/events") {
			suffix = "/federation/events"
		} else {
			suffix = "/federation/metadata"
		}
	case http.MethodPost:
		suffix = "/federation/events:ingest"
	default:
		return false
	}

	rest, ok := strings.CutPrefix(path, "/api/v1/projects/")
	if !ok {
		return false
	}
	projectID, ok := strings.CutSuffix(rest, suffix)
	if !ok {
		return false
	}
	if projectID == "" {
		return false
	}
	for _, r := range projectID {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkAuthStartup refuses startup when the listen address would expose
// the daemon to plaintext-on-the-wire access. listen uses the same
// convention as runDaemonWithListen: "" means platform-default local
// transport; "host:port" means TCP. The matrix on non-loopback TCP is:
//
//	Token != "" && TrustPrivateNetwork -> permit (operator accepts private-network confidentiality)
//	Token != "" && !TrustPrivateNetwork -> REFUSE (token would travel in cleartext)
//	Token == "" &&  InsecureReadonly   -> permit (dev-only GET access)
//	Token == "" && !InsecureReadonly   -> REFUSE (would expose mutations to the LAN)
//
// The daemon does not terminate TLS, so a bearer token on plaintext non-
// loopback HTTP is a passive-capture risk. Operators wanting cross-host
// access must either tunnel via SSH (loopback on both ends) or front the
// daemon with a TLS-terminating reverse proxy and bind the daemon to a
// Unix socket or 127.0.0.1.
func checkAuthStartup(listen string, p authPolicy) error {
	if p.RequireTokenIdentity && p.Token == "" {
		return fmt.Errorf("require_token_identity requires a bootstrap token")
	}
	if p.RequireTokenIdentity && p.InsecureReadonly {
		return fmt.Errorf("require_token_identity cannot be combined with --insecure-readonly")
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
		Token:                auth.Token,
		TrustPrivateNetwork:  auth.TrustPrivateNetwork,
		InsecureReadonly:     insecureReadonly,
		RequireTokenIdentity: auth.RequireTokenIdentity,
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
