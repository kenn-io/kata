package daemon

import (
	"context"
	"net"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// PrincipalKind identifies how a request was authenticated.
type PrincipalKind string

const (
	// PrincipalDBToken is a DB-backed user token with an actor.
	PrincipalDBToken PrincipalKind = "db_token"
	// PrincipalBootstrap is the identity-mode bootstrap/admin token.
	PrincipalBootstrap PrincipalKind = "bootstrap"
	// PrincipalStaticToken is the legacy configured bearer token outside
	// identity mode. It is not an attributed actor, but token-admin routes
	// audit it as bootstrap/admin rather than as the target actor.
	PrincipalStaticToken PrincipalKind = "static_token"
	// PrincipalTrustedProxy is set by the trusted-proxy middleware when an
	// accepted request on a trusted listener carries the configured actor
	// header. The Principal.Actor field holds the verified header value.
	PrincipalTrustedProxy PrincipalKind = "trusted_proxy"
	// PrincipalTrustedProxyAbsent is set by the trusted-proxy middleware
	// when a request on a trusted listener is missing the configured actor
	// header (or its value is empty). Writes against this principal are
	// rejected; reads pass through.
	PrincipalTrustedProxyAbsent PrincipalKind = "trusted_proxy_absent"
	// PrincipalHost is supplied in process by a mounted service's host access
	// adapter. It never comes from a network header.
	PrincipalHost PrincipalKind = "host"
	// PrincipalWebLocal is issued through the owner-local launch proof or the
	// direct keyless-loopback browser boundary. It permits ordinary attributed
	// writes without granting bearer-token administration authority.
	PrincipalWebLocal PrincipalKind = "web_local"
)

// Principal is the request-local identity derived by auth middleware.
type Principal struct {
	Kind    PrincipalKind
	Actor   string
	Subject string
	TokenID int64
	Name    *string
}

type principalContextKey struct{}
type insecureReadonlyContextKey struct{}

// WithPrincipal attaches an authenticated request principal to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext returns the authenticated request principal, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

func validHostPrincipal(principal Principal) bool {
	return principal.Kind == PrincipalHost &&
		strings.TrimSpace(principal.Subject) != "" &&
		strings.TrimSpace(principal.Actor) != ""
}

func withInsecureReadonlyRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, insecureReadonlyContextKey{}, true)
}

func insecureReadonlyRequest(ctx context.Context) bool {
	v, _ := ctx.Value(insecureReadonlyContextKey{}).(bool)
	return v
}

func actorFor(ctx context.Context, requestActor string) string {
	if p, ok := PrincipalFromContext(ctx); ok && p.Actor != "" {
		return p.Actor
	}
	return requestActor
}

func attributedActor(ctx context.Context, requestActor string) (string, error) {
	if err := ensureAttributedWriteAllowed(ctx); err != nil {
		return "", err
	}
	actor := actorFor(ctx, requestActor)
	if err := validateActor(actor); err != nil {
		return "", err
	}
	return actor, nil
}

func ensureAttributedWriteAllowed(ctx context.Context) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	switch p.Kind {
	case PrincipalBootstrap:
		return api.NewError(403, "bootstrap_token_write_forbidden",
			"bootstrap token cannot perform attributed writes; use a user token", "", nil)
	case PrincipalTrustedProxyAbsent:
		return api.NewError(400, "actor_header_required",
			"actor header required on this listener but was missing or empty", "", nil)
	default:
		return nil
	}
}

func ensureTokenAdminAllowed(ctx context.Context) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.Kind == PrincipalBootstrap || p.Kind == PrincipalStaticToken {
		return nil
	}
	return api.NewError(403, "token_admin_forbidden",
		"token administration requires the bootstrap token or a local no-auth session", "", nil)
}

func tokenAdminAuditActor(ctx context.Context, fallback string) string {
	if p, ok := PrincipalFromContext(ctx); ok &&
		(p.Kind == PrincipalBootstrap || p.Kind == PrincipalStaticToken) {
		return db.BootstrapActor
	}
	return fallback
}

func tuiBypassAllowed(ctx context.Context, source, reason string) bool {
	if source != "tui" || reason != "done" {
		return false
	}
	if p, ok := PrincipalFromContext(ctx); ok {
		switch p.Kind {
		case PrincipalDBToken, PrincipalTrustedProxy, PrincipalTrustedProxyAbsent, PrincipalWebLocal:
			return false
		}
	}
	return ownerLocalTransport(ctx)
}

// ownerLocalTransport recognizes the daemon transports that stay inside the
// owner-local host boundary. net/http supplies LocalAddrContextKey from the
// accepted listener, so a request body or forwarding header cannot claim this
// state. Loopback TCP remains local for Windows, where the default daemon
// transport is not a Unix socket. Missing and unknown address types fail
// closed.
func ownerLocalTransport(ctx context.Context) bool {
	addr, ok := ctx.Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return false
	}
	switch local := addr.(type) {
	case *net.UnixAddr:
		return true
	case *net.TCPAddr:
		return local.IP.IsLoopback()
	default:
		return false
	}
}

func principalFromAPIToken(tok db.APIToken) Principal {
	return Principal{
		Kind:    PrincipalDBToken,
		Actor:   tok.Actor,
		TokenID: tok.ID,
		Name:    tok.Name,
	}
}
