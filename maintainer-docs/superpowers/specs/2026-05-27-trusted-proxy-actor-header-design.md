# kata Trusted-Proxy Actor Header Design

**Status:** Approved design for implementation planning (post-PR-#65)
**Date:** 2026-05-27 (last revised 2026-05-28)
**Topic:** Opt-in mode where a trusted front proxy asserts the request actor via a configured header, complementing PR #65's token-identity mode.
**Issue:** https://github.com/kenn-io/kata/issues/58
**Depends on:** PR #65 (`fix/simple-token-auth`) — token-identity infrastructure, `Principal` type, `attributedActor` chokepoint.

## 1. Purpose

The daemon credits every mutating action to an actor. PR #65 introduces the
notion of an **attributed principal** — a server-derived identity that wins over
any client-supplied actor. In PR #65 the only attributed principal source is a
DB-registered API token (`PrincipalDBToken`).

This spec adds a second attributed-principal source for the deployment pattern
PR #65 does not cover: a reverse proxy fronts the daemon, authenticates the
user (SSO, OAuth, mutual-TLS, whatever), and asserts the actor via a configured
HTTP header over a daemon path only the proxy can reach.

Token identity and trusted-proxy identity are two **server-derived actor
modes**. Both ignore client-supplied actor fields where they apply. Token
identity applies on every request the daemon accepts (PR #65). Trusted-proxy
identity applies only to requests that arrive on a configured trusted listener;
requests on any other listener follow PR #65's existing rules unchanged. Within
the scope where a mode applies, there is no fallback to a body or query actor.
An operator picks one mode (or both, on different listeners) based on their
deployment:

| Mode | Auth boundary | Actor source | Best for |
|------|---------------|--------------|----------|
| Token identity (PR #65) | The daemon | DB-stored `api_tokens` row | Direct CLI clients with personal tokens |
| Trusted-proxy identity (this spec) | A reverse proxy | A configured header set by the proxy | SSO/OAuth deployments behind a proxy |

Off by default. No behavior change unless configured. Coexists with token
identity at the chokepoint (`attributedActor`).

## 2. Configuration Surface

A new TOML sub-table `[auth.proxy]` carries this mode's keys, mirroring the
shape Grafana uses for `[auth.proxy]`:

```toml
[auth.proxy]
trusted_actor_header = "X-Kata-Actor"
trusted_proxy_listeners = ["unix:///run/kata/proxy.sock"]
```

- `trusted_actor_header` (string) — the header name the proxy sets. Empty (or
  absent) means trusted-proxy mode is **off**.
- `trusted_proxy_listeners` ([]string) — the literal bind address(es) on which
  the header is honored. A request that did not arrive on one of these listeners
  ignores the header entirely.

Environment overrides match the existing style (`KATA_AUTH_TOKEN`,
`KATA_TRUST_PRIVATE_NETWORK`, `KATA_REQUIRE_TOKEN_IDENTITY`):

- `KATA_TRUSTED_ACTOR_HEADER` — string; overrides `trusted_actor_header`.
- `KATA_TRUSTED_PROXY_LISTENERS` — comma-separated list; overrides
  `trusted_proxy_listeners`. Entries are trimmed; empty entries are dropped.

Both env vars treat empty/unset as "no override" (matches `KATA_AUTH_TOKEN`):
an operator cannot disable a TOML-enabled mode by setting the env var to an
empty string. Disabling the mode requires editing config.toml. This is
intentional — silently turning off a security-affecting config from an empty
env var is more dangerous than the friction of a config edit.

Resolution rules after env + TOML merge:

- `trusted_actor_header` empty, `trusted_proxy_listeners` any → mode **off**.
  The header (if any) is ignored everywhere; PR #65 behavior is unchanged.
  Listeners-without-header is accepted as dead config (no security impact:
  with no header name, no overwrite ever happens).
- `trusted_actor_header` non-empty, `trusted_proxy_listeners` empty →
  **rejected at config load** with an error naming the missing key. A silent
  no-op here is dangerous: an operator who typos the listener address (or
  forgets it) would believe proxy attribution is on while body-supplied actors
  continue to flow through. Implementers: `ReadDaemonConfig` must return an
  error in this case after the env+TOML merge so an env-supplied listener can
  complete a TOML-supplied header (and vice versa).
- `trusted_actor_header` non-empty, `trusted_proxy_listeners` non-empty → mode
  **on** for the listed listeners; other listeners pass through untouched.

Absent everywhere = off; existing configs parse and behave exactly as before.

### 2.1 Listener address forms

Allowlist entries must be **literal** bind addresses, matching the daemon's
`listen` value:

- Unix socket: `unix:///run/kata/proxy.sock` (the `unix://` prefix is stripped
  before matching).
- TCP: `host:port`, e.g. `100.64.0.5:7777`.

Wildcard binds (`0.0.0.0:7777`, `:7777`, `::`) are **not** valid allowlist
entries: an accepted connection reports the specific interface IP it arrived on,
never the wildcard, so a wildcard entry would never match. This mirrors the
existing posture in `requireNonPublic` (`internal/daemon/endpoint.go`), which
already rejects unspecified binds. A trusted proxy listener should be a Unix
socket or a specific private IP that only the proxy can reach.

## 3. Architecture

### 3.1 Per-request listener trust

The HTTP server roots every request in a base context that carries
`http.LocalAddrContextKey` — the `net.Addr` of the local end of the accepted
connection (set automatically by `net/http`). A middleware reads that address,
normalizes it, and tests membership in the normalized allowlist:

- Unix local addr (`*net.UnixAddr`): compare its path against allowlist entries
  with any `unix://` prefix stripped.
- TCP local addr (`*net.TCPAddr`): compare its `host:port` string against
  allowlist entries.

Resolving trust per request (rather than once at startup from
`cfg.Endpoint.Address()`) is deliberate: `cfg.Endpoint` is nil when the server
is mounted via `httptest`, so a startup-resolved boolean would be untestable.
The per-request check works in both the real daemon and `httptest`, and makes
the listener-trust matrix unit-testable without binding real sockets.

### 3.2 Middleware: `withTrustedProxyActor`

A new `net/http` middleware composed just inside `requireBearer` (or PR #65's
`requireIdentityBearer` when identity mode is on) in `NewServer`, so it runs
only on requests that auth has admitted or passed through. The middleware does
NOT replace PR #65's principal middleware — it runs after it. On trusted
listeners with a valid header it overwrites the principal with a
`PrincipalTrustedProxy`; on trusted listeners with a missing/empty header it
sets a `PrincipalTrustedProxyAbsent` sentinel.

Composition order (outer → inner):

```
withCSRFGuards
  requireBearer / requireIdentityBearer  (sets Principal{StaticToken|Bootstrap|DBToken})
    withTrustedProxyActor                (overwrites Principal on trusted listeners)
      mux
```

Behavior per request:

- Mode off (empty header name) -> pass through untouched. Principal from PR #65
  (if any) stands.
- Mode on but local addr not in the allowlist -> pass through untouched
  (header, if any, is ignored).
- Mode on and on a trusted listener:
  - Header present and non-empty -> overwrite principal with
    `Principal{Kind: PrincipalTrustedProxy, Actor: <trimmed header value>}`.
  - Header absent or empty -> overwrite principal with
    `Principal{Kind: PrincipalTrustedProxyAbsent}`.

The middleware never rejects on its own; rejection is decided downstream in
`attributedActor` / `actorFor` so read-only requests (which do not call those
functions) are never blocked.

If the proxy ever sends multiple values for the configured header, the
middleware silently uses the first value (Go's `r.Header.Get` semantics) — no
log, no rejection. This is intentional: detecting and reacting to malformed
proxy behavior is the proxy operator's job, not the daemon's, and adding a log
line per request would just be noise on every well-formed deployment. Operator
docs must require a single value at the proxy boundary so this case never
arises in practice.

### 3.3 Integration with PR #65's `attributedActor` chokepoint

PR #65 lands one chokepoint for all attributed writes:

```go
// from PR #65
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
```

This spec extends two functions in that package, **adding a third principal
kind** rather than introducing a parallel resolver:

- `actorFor(ctx, requestActor)` learns to return the proxy-asserted actor when
  the context carries `Principal{Kind: PrincipalTrustedProxy, Actor: a}` —
  returning `a`, ignoring `requestActor`.
- `ensureAttributedWriteAllowed(ctx)` learns to reject when the context carries
  `Principal{Kind: PrincipalTrustedProxyAbsent}`, returning
  `400 actor_header_required`.

All 26 mutation handlers PR #65 swapped to `attributedActor` automatically pick
up the new attribution source — no further handler edits.

### 3.4 Principal precedence

`withTrustedProxyActor` overwrites whichever principal PR #65 already attached
to the request when the request lands on a trusted listener and the mode is on.
The full matrix:

| Listener trusted? | Header set? | Mode | Principal on ctx after middleware | Mutation outcome via `attributedActor` |
|-------------------|-------------|------|------------------------------------|----------------------------------------|
| n/a | n/a | off | Whatever PR #65 set (`StaticToken` / `Bootstrap` / `DBToken` / none) | PR #65 rules |
| no | any | on | Whatever PR #65 set | PR #65 rules |
| yes | non-empty | on | `PrincipalTrustedProxy{Actor: <header>}` (overwrites PR #65) | `actorFor` returns the header value; body actor ignored |
| yes | empty/absent | on | `PrincipalTrustedProxyAbsent` (overwrites PR #65) | `ensureAttributedWriteAllowed` rejects with `400 actor_header_required` |

Two consequences of the overwrite-on-trusted-listener rule:

- A valid DB-token identity from PR #65 is silently discarded when the same
  request lands on a trusted listener with this mode on. Operators running
  both modes on the same listener are choosing this behavior; the deployment
  guidance is to terminate each mode on its own listener (a Unix socket for
  the proxy; a TCP port for direct token-holding clients).
- Token-admin endpoints (`POST /api/v1/tokens`, `GET /api/v1/tokens`,
  `POST /api/v1/tokens/{id}/actions/revoke`) reject **both** trusted-proxy
  principal kinds — `PrincipalTrustedProxy` and `PrincipalTrustedProxyAbsent` —
  via PR #65's `ensureTokenAdminAllowed`, which is allow-list and admits only
  `PrincipalBootstrap`, `PrincipalStaticToken`, or no-principal. A
  trusted-proxy front-end cannot mint or revoke tokens whether or not it
  supplies the header; that capability stays bound to the bootstrap actor.
  Both branches are pinned by tests
  (`TestTrustedProxyHeader_TokenAdminForbidden` for the header-present case,
  `TestTrustedProxyHeader_TokenAdminForbiddenWithoutHeader` for absent).

### 3.5 Schema stays required

The `actor` body/query field keeps its `required:"true"` Huma tag. On a trusted
listener the client still sends some actor value, which is simply ignored in
favor of the header (acceptance criteria: "a conflicting body/query actor is
ignored"). This keeps the change off every actor struct tag and means a client
that omits actor entirely still gets the existing required-field validation
error — no behavior change. The proxy deployment forwards the kata client
request, which always includes an actor, so this is invisible in practice.

## 4. Data Flow

A mutating request flows through these stages in order:

- Client (or proxy-forwarded client) sends a mutating request.
- `requireBearer` / `requireIdentityBearer` admits the request; in identity
  mode PR #65 sets a `Principal{Kind: ...}` from the bearer.
- `withTrustedProxyActor` inspects the local addr: trusted listener + header
  set -> overwrites principal with `PrincipalTrustedProxy{Actor: header}`;
  trusted listener + header missing -> overwrites principal with
  `PrincipalTrustedProxyAbsent`; otherwise -> principal untouched.
- Huma routes to the handler; the handler calls
  `attributedActor(ctx, in.Body.Actor)`.
- `ensureAttributedWriteAllowed` rejects `PrincipalTrustedProxyAbsent` with
  `400 actor_header_required`.
- `actorFor` returns the proxy-asserted actor for `PrincipalTrustedProxy`,
  ignoring `requestActor`. For any other principal kind, PR #65's existing
  rules apply.
- The resolved actor flows on as the change `Author`, exactly as today.

## 5. Error Handling

- Trusted listener, mutation, missing/empty header -> `400 actor_header_required`
  with a clear message. (Decision: reject rather than fall back. Falling back
  would let a direct, non-proxied client omit the header and supply any actor,
  defeating the feature.)
- Mode off / non-trusted listener -> identical to current behavior (whatever
  PR #65 establishes for the principal in scope).
- Reads carry no actor and never reach `attributedActor`, so they are never
  blocked by this feature.

## 6. Security Notes

- Trust is bound to specific listeners so a client on any other path cannot
  spoof the actor by setting the header. With a Unix socket or a private IP that
  only the proxy can reach, the header is meaningful exactly because nothing else
  can set it on that path.
- Stripping any client-supplied copy of the header before it reaches kata is the
  proxy's responsibility and is deployment-side (out of scope). This will be
  noted in the operator docs.
- The feature does not change the connection-auth posture (`requireBearer`,
  `checkAuthStartup`) and does not interact with token admin (token mint/revoke
  still bootstrap-or-loopback only per PR #65). It layers actor attribution on
  top.
- Token-identity mode and trusted-proxy mode are intentionally independent.
  Operators choosing trusted-proxy mode typically run the daemon on a Unix
  socket or loopback that only the proxy can reach; the proxy owns user auth
  and the daemon owns audit attribution. Token-identity mode is for direct
  clients each holding a DB-registered token.

## 7. Testing

- **Middleware unit tests** (`withTrustedProxyActor`): drive crafted
  `*http.Request`s with `http.LocalAddrContextKey` set and the header
  present/absent across the trust matrix — trusted+present, trusted+absent,
  untrusted+present, untrusted+absent, mode-off — asserting the resulting
  principal value on the context. No real sockets.
- **`actorFor` / `ensureAttributedWriteAllowed` unit tests for the new
  principal kinds:** authoritative-wins (`PrincipalTrustedProxy`),
  trusted-but-absent -> `400 actor_header_required`
  (`PrincipalTrustedProxyAbsent`), and the existing principal kinds delegate
  unchanged.
- **Config tests:** TOML parse of both keys under `[auth.proxy]`; env overrides
  incl. comma-split and trimming for the listener list; absent = off; existing
  `[auth]` configs unchanged.
- **Listener normalization tests:** `unix://`-prefixed entry matches a unix
  local addr; `host:port` entry matches a TCP local addr; wildcard entry does
  not match.
- **Reads-not-blocked test:** with the mode on and on a trusted listener, a
  header-less GET still returns its normal 2xx — verifying the middleware
  defers rejection to `attributedActor` and never blocks read paths.
- **Integration test (`httptest`):** trusted listener credits the header value
  and ignores a conflicting body actor; a regression test confirming the
  mode-off path is byte-for-byte current behavior.

## 8. Out of Scope

- The proxy itself and how it authenticates users; any specific identity
  provider.
- Making the `actor` field optional in the request schema.
- Per-operation or per-actor authorization (the header asserts identity, not
  permissions).
- Cross-mode interactions beyond "trusted-proxy principal wins over whatever
  PR #65 established for this request." If an operator runs both modes at
  once on the same listener, the trusted-proxy principal overwrites the token
  principal — documented but not encouraged.
