package daemon

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// listenerTrusted reports whether localAddr matches one of the configured
// trusted-proxy-listener entries. Entries are normalized by trimming
// whitespace and any "unix://" prefix; the local addr's String() is
// compared verbatim (for Unix sockets that's the path; for TCP it's
// host:port with brackets for IPv6).
//
// A wildcard bind ("0.0.0.0:7777", "[::]:7777") reports a specific
// interface IP per accepted connection, never the wildcard, so a
// wildcard entry never matches. Operators should list literal bind
// addresses (a Unix socket or a specific private IP).
func listenerTrusted(localAddr net.Addr, allowlist []string) bool {
	if localAddr == nil || len(allowlist) == 0 {
		return false
	}
	local := localAddr.String()
	for _, entry := range allowlist {
		if normalizeListenerEntry(entry) == local {
			return true
		}
	}
	return false
}

func normalizeListenerEntry(s string) string {
	s = strings.TrimSpace(s)
	return strings.TrimPrefix(s, "unix://")
}

// withTrustedProxyActor inspects each request's local address and, on a
// trusted listener, overwrites the request principal with a
// PrincipalTrustedProxy carrying the header value. A missing or empty
// header on a trusted listener becomes a PrincipalTrustedProxyAbsent
// sentinel. The middleware never rejects on its own; rejection is left to
// ensureAttributedWriteAllowed so read-only paths (which never call
// attributedActor) are not blocked.
func withTrustedProxyActor(cfg ServerConfig) func(http.Handler) http.Handler {
	headerName := strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader)
	allowlist := cfg.Auth.Proxy.TrustedProxyListeners
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if headerName == "" {
				next.ServeHTTP(w, r)
				return
			}
			localAddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
			if !listenerTrusted(localAddr, allowlist) {
				next.ServeHTTP(w, r)
				return
			}
			warnIfOverwritingUpstreamPrincipal(r.Context(), logger, localAddr, headerName)
			raw := strings.TrimSpace(r.Header.Get(headerName))
			var ctx context.Context
			if raw != "" {
				ctx = WithPrincipal(r.Context(), Principal{
					Kind:  PrincipalTrustedProxy,
					Actor: raw,
				})
			} else {
				ctx = WithPrincipal(r.Context(), Principal{
					Kind: PrincipalTrustedProxyAbsent,
				})
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// warnIfOverwritingUpstreamPrincipal logs a warning when bearer auth has
// already attached a principal and trusted-proxy mode is about to overwrite
// it. An operator who configured both modes on the same listener gets
// per-request visibility that the proxy header is silently winning, so they
// can either accept the topology or split the modes onto separate listeners.
func warnIfOverwritingUpstreamPrincipal(ctx context.Context, logger *slog.Logger, localAddr net.Addr, headerName string) {
	existing, ok := PrincipalFromContext(ctx)
	if !ok {
		return
	}
	switch existing.Kind {
	case PrincipalDBToken, PrincipalStaticToken, PrincipalBootstrap:
	default:
		return
	}
	listener := ""
	if localAddr != nil {
		listener = localAddr.String()
	}
	logger.Warn("trusted-proxy header overwriting upstream principal",
		"listener", listener,
		"upstream_kind", string(existing.Kind),
		"upstream_actor", existing.Actor,
		"header", headerName)
}
