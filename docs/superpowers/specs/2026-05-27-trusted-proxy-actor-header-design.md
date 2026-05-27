# kata Trusted-Proxy Actor Header Design

**Status:** Approved design for implementation planning
**Date:** 2026-05-27
**Topic:** Opt-in mode where a trusted front proxy asserts the request actor via a configured header, instead of trusting the client-supplied actor.
**Issue:** https://github.com/kenn-io/kata/issues/58

## 1. Purpose

The daemon credits every mutating action to a caller-supplied `actor` string
(JSON body or query) and only checks that it is non-empty (`validateActor`,
`internal/daemon/server.go`). For a single local user that is fine, but anything
that can reach the API can claim to be anyone.

This adds an **opt-in** mode: when the daemon runs behind a reverse proxy that
has already authenticated the user, kata takes the actor from a configured
request header set by that proxy and treats it as authoritative, ignoring the
client-supplied value. This is the well-known auth-proxy pattern (cf. Grafana's
`[auth.proxy]`).

Off by default. No behavior change unless configured.

## 2. Configuration Surface

Two optional keys extend the existing `[auth]` block in
`<KATA_HOME>/config.toml` (`internal/config/daemon_config.go`):

```toml
[auth]
trusted_actor_header = "X-Kata-Actor"
trusted_proxy_listeners = ["unix:///run/kata/proxy.sock"]
```

- `trusted_actor_header` (string) — the header name the proxy sets. Empty (or
  absent) means the feature is **off**.
- `trusted_proxy_listeners` ([]string) — the literal bind address(es) on which
  the header is honored. A request that did not arrive on one of these listeners
  ignores the header entirely.

Environment overrides match the existing style
(`KATA_AUTH_TOKEN`, `KATA_TRUST_PRIVATE_NETWORK`):

- `KATA_TRUSTED_ACTOR_HEADER` — string; overrides `trusted_actor_header`.
- `KATA_TRUSTED_PROXY_LISTENERS` — comma-separated list; overrides
  `trusted_proxy_listeners`. Entries are trimmed; empty entries are dropped.

The feature is **on** only when `trusted_actor_header` is non-empty after
resolution. `trusted_proxy_listeners` with an empty header is inert (header off
wins). Absent everywhere = off; existing configs parse and behave exactly as
before.

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

### 3.2 Middleware: `withTrustedActor`

A new `net/http` middleware composed just inside `requireBearer` in
`NewServer` (`internal/daemon/server.go`), so it runs only for requests that
auth has admitted or passed through:

```
withCSRFGuards(requireBearer(p)(withTrustedActor(cfg)(mux)))
```

Behavior per request:

- Feature off (empty header name) -> pass through untouched.
- Feature on but local addr not in the allowlist -> pass through untouched
  (header, if any, is ignored).
- Feature on and on a trusted listener:
  - Header present and non-empty -> store the value in the request context as
    the **authoritative** actor.
  - Header absent or empty -> store a **trusted-but-absent** marker in the
    context.

The middleware never rejects on its own; rejection is decided downstream in
`effectiveActor` so that read-only requests (which carry no actor) are never
blocked for lacking the header.

### 3.3 Actor resolution: `effectiveActor`

`validateActor(actor string) error` is replaced at its call sites by
`effectiveActor(ctx context.Context, supplied string) (string, error)`.
`validateActor` is retained as the inner non-empty check.

```
effectiveActor(ctx, supplied):
  authoritative, ok := trustedActorFromContext(ctx)
  if ok:
    return authoritative, nil          // ignores supplied
  if trustedButAbsent(ctx):
    return "", api.NewError(400, "actor_header_required",
        "actor header required on this listener but was missing or empty", "", nil)
  if err := validateActor(supplied); err != nil:
    return "", err                      // existing behavior, unchanged
  return supplied, nil
```

The authoritative value is itself run through `validateActor` inside the
middleware before being stored, so a whitespace-only header is treated as
absent (and therefore rejected for mutations).

### 3.4 Handler integration

The 26 mutation call sites across 13 handler files currently do:

```go
if err := validateActor(in.Body.Actor); err != nil {
    return nil, err
}
```

They become:

```go
actor, err := effectiveActor(ctx, in.Body.Actor)
if err != nil {
    return nil, err
}
in.Body.Actor = actor
```

(The actor field is `in.Body.Actor` for body operations and `in.Actor` for
query operations; a few handlers copy it into a local variable first.)
Reassigning whatever variable the handler passes downstream means the existing
reads — including the value handed to the DB as `Author` and to the throttle
guards — keep working unchanged.

Read-only endpoints that use `actor` as a **filter** (events stream, digest,
audit listings) do not call `validateActor`/`effectiveActor` and are left
exactly as they are.

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
- `requireBearer` admits or passes it through.
- `withTrustedActor` inspects the local addr: trusted listener + header set ->
  context gets the authoritative actor; trusted listener + header missing ->
  context gets the trusted-but-absent marker; otherwise -> context untouched.
- Huma routes to the handler; the handler calls `effectiveActor(ctx, supplied)`.
- Authoritative value wins; trusted-but-absent -> 400; otherwise the supplied
  actor is validated and used.
- The resolved actor flows on as the change `Author`, exactly as today.

## 5. Error Handling

- Trusted listener, mutation, missing/empty header -> `400 actor_header_required`
  with a clear message. (Decision: reject rather than fall back. Falling back
  would let a direct, non-proxied client omit the header and supply any actor,
  defeating the feature.)
- Feature off / non-trusted listener -> identical to current behavior, including
  the existing `400 validation` from `validateActor` on an empty supplied actor.
- Reads carry no actor and never reach `effectiveActor`, so they are never
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
  `checkAuthStartup`); it layers actor attribution on top of it.

## 7. Testing

- **Middleware unit tests** (`withTrustedActor`): drive crafted `*http.Request`s
  with `http.LocalAddrContextKey` set and the header present/absent across the
  trust matrix — trusted+present, trusted+absent, untrusted+present,
  untrusted+absent, feature-off — asserting the resulting context state. No real
  sockets.
- **`effectiveActor` unit tests:** authoritative-wins, trusted-but-absent ->
  400 `actor_header_required`, and the off/non-trusted path delegating to
  `validateActor`.
- **Config tests:** TOML parse of both keys; env overrides incl. comma-split and
  trimming for the listener list; absent = off; existing `[auth]` configs
  unchanged.
- **Listener normalization tests:** `unix://`-prefixed entry matches a unix
  local addr; `host:port` entry matches a TCP local addr; wildcard entry does
  not match.
- **Reads-not-blocked test:** with the feature on and on a trusted listener, a
  header-less GET still returns its normal 2xx — verifying the middleware defers
  rejection to `effectiveActor` and never blocks read paths.
- **Integration test (`httptest`):** trusted listener credits the header value
  and ignores a conflicting body actor; a regression test confirming the
  feature-off path is byte-for-byte current behavior.

## 8. Out of Scope

- The proxy itself and how it authenticates users; any specific identity
  provider.
- Making the `actor` field optional in the request schema.
- Per-operation or per-actor authorization (the header asserts identity, not
  permissions).
