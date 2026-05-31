# Trusted-Proxy Actor Header Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Hard dependency:** PR #65 (`fix/simple-token-auth`) must be merged before this plan can be executed. The plan plugs into PR #65's `Principal` type, `WithPrincipal` context helper, `actorFor`, `ensureAttributedWriteAllowed`, and `attributedActor` chokepoint. Tasks reference these symbols directly.

**Goal:** Add a trusted-proxy actor header mode that lets a reverse proxy assert the request actor via a configured header. Coexists with PR #65's token-identity mode at the `attributedActor` chokepoint; both are server-derived attribution modes.

**Architecture:** New `[auth.proxy]` config sub-table plus env overrides. A new `withTrustedProxyActor` middleware (composed inside PR #65's bearer/identity middleware) inspects the request's local addr; on a trusted listener it overwrites the principal with `PrincipalTrustedProxy{Actor: header}` or a `PrincipalTrustedProxyAbsent` sentinel. PR #65's `actorFor` and `ensureAttributedWriteAllowed` are extended to recognize the two new principal kinds — `PrincipalTrustedProxy` returns the proxy-asserted actor (ignoring the body actor); `PrincipalTrustedProxyAbsent` rejects with `400 actor_header_required`. Handler code does not change.

**Tech Stack:** Go, `net/http`, `github.com/danielgtaylor/huma/v2`, `github.com/BurntSushi/toml`, `github.com/stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-05-27-trusted-proxy-actor-header-design.md`.

---

## File Structure

**Create:**
- `internal/daemon/trusted_actor.go` — listener matcher + `withTrustedProxyActor` middleware.
- `internal/daemon/trusted_actor_test.go` — unit tests for the matcher and the middleware matrix.
- `internal/daemon/trusted_actor_e2e_test.go` — integration tests via `httptest`.

**Modify:**
- `internal/config/daemon_config.go` — add a `ProxyConfig` struct under `AuthConfig`, parsing the `[auth.proxy]` sub-table; extend `applyDaemonConfigEnv` with the two new env overrides.
- `internal/config/daemon_config_test.go` — add tests for TOML parse + env overrides for the new keys.
- `internal/daemon/auth.go` (or wherever PR #65 puts `Principal`/`actorFor`/`ensureAttributedWriteAllowed`):
  - Add two `PrincipalKind` constants: `PrincipalTrustedProxy`, `PrincipalTrustedProxyAbsent`.
  - Extend `actorFor` to return the proxy-asserted actor for `PrincipalTrustedProxy`.
  - Extend `ensureAttributedWriteAllowed` to reject `PrincipalTrustedProxyAbsent` with `400 actor_header_required`.
- `internal/daemon/auth_test.go` (or the file PR #65 tests `actorFor` in) — add tests for the two new principal-kind branches.
- `internal/daemon/server.go` — compose `withTrustedProxyActor` into the middleware chain in `NewServer`, inside `requireBearer`/`requireIdentityBearer`.

**Leave alone:**
- The 26 mutation handlers — PR #65 already swapped them to `attributedActor`. This plan changes nothing in any handler file.
- Read-only actor-filter handlers (`handlers_events.go`, `handlers_digest.go`, `handlers_audit.go`) — never call `attributedActor`.
- Every `actor` struct tag in `internal/api/types.go` — schema stays `required:"true"`.

---

## Task 1: Add `[auth.proxy]` config keys

**Files:**
- Modify: `internal/config/daemon_config.go` (extend `AuthConfig` with a `Proxy ProxyConfig` field; define `ProxyConfig`)
- Test: `internal/config/daemon_config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/daemon_config_test.go`:

```go
func TestReadDaemonConfig_AuthProxy(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")

	dir := t.TempDir()
	t.Setenv("KATA_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	body := `
[auth]
token = "tok"

[auth.proxy]
trusted_actor_header = "X-Kata-Actor"
trusted_proxy_listeners = ["unix:///run/kata/proxy.sock", "100.64.0.5:7777"]
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, "X-Kata-Actor", cfg.Auth.Proxy.TrustedActorHeader)
	require.Equal(t,
		[]string{"unix:///run/kata/proxy.sock", "100.64.0.5:7777"},
		cfg.Auth.Proxy.TrustedProxyListeners)
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/config/ -run TestReadDaemonConfig_AuthProxy -v`
Expected: FAIL — `cfg.Auth.Proxy` undefined or unknown-key TOML error.

- [ ] **Step 3: Add the struct + sub-table parsing**

In `internal/config/daemon_config.go`, extend `AuthConfig` and define `ProxyConfig`:

```go
type AuthConfig struct {
	Token               string      `toml:"token"`
	TrustPrivateNetwork bool        `toml:"trust_private_network"`
	// ... whatever PR #65 added to AuthConfig (RequireTokenIdentity etc.) stays.
	Proxy               ProxyConfig `toml:"proxy"`
}

// ProxyConfig is the [auth.proxy] sub-table. Both keys empty/absent means
// trusted-proxy actor mode is off; this is the default.
type ProxyConfig struct {
	TrustedActorHeader    string   `toml:"trusted_actor_header"`
	TrustedProxyListeners []string `toml:"trusted_proxy_listeners"`
}
```

Extend the trim block in `ReadDaemonConfig`:

```go
cfg.Listen = strings.TrimSpace(cfg.Listen)
cfg.Auth.Token = strings.TrimSpace(cfg.Auth.Token)
cfg.Auth.Proxy.TrustedActorHeader = strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader)
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/config/ -run TestReadDaemonConfig_AuthProxy -v`
Expected: PASS.

- [ ] **Step 5: Run the full config package suite**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/daemon_config.go internal/config/daemon_config_test.go
git commit -m "feat: add [auth.proxy] config sub-table for trusted-proxy actor header"
```

---

## Task 2: Apply env overrides for the two new keys

**Files:**
- Modify: `internal/config/daemon_config.go` (extend `applyDaemonConfigEnv`)
- Test: `internal/config/daemon_config_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/daemon_config_test.go`:

```go
func TestApplyDaemonConfigEnv_AuthProxyHeader(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "X-Env-Actor")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_actor_header = \"X-Toml-Actor\"\n"), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, "X-Env-Actor", cfg.Auth.Proxy.TrustedActorHeader,
		"KATA_TRUSTED_ACTOR_HEADER must override config.toml")
}

func TestApplyDaemonConfigEnv_AuthProxyListeners(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS",
		"unix:///s1 , 100.64.0.5:7777 ,, ")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_proxy_listeners = [\"unix:///toml-only\"]\n"), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t,
		[]string{"unix:///s1", "100.64.0.5:7777"},
		cfg.Auth.Proxy.TrustedProxyListeners,
		"KATA_TRUSTED_PROXY_LISTENERS must split on commas, trim, drop empties, override config.toml")
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./internal/config/ -run TestApplyDaemonConfigEnv_AuthProxy -v`
Expected: FAIL — env variables not applied yet.

- [ ] **Step 3: Extend `applyDaemonConfigEnv`**

In `internal/config/daemon_config.go`, add inside `applyDaemonConfigEnv` (after the existing token / trust_private_network branches PR #65 left in place):

```go
if v := strings.TrimSpace(os.Getenv("KATA_TRUSTED_ACTOR_HEADER")); v != "" {
	cfg.Auth.Proxy.TrustedActorHeader = v
}
if raw := os.Getenv("KATA_TRUSTED_PROXY_LISTENERS"); raw != "" {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	cfg.Auth.Proxy.TrustedProxyListeners = out
}
```

`os.Getenv` + non-empty matches the existing `KATA_AUTH_TOKEN` pattern (empty string = no override) and keeps Task 1's TOML test compatible.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./internal/config/ -run TestApplyDaemonConfigEnv_AuthProxy -v`
Expected: PASS.

- [ ] **Step 5: Run the full config package suite**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/daemon_config.go internal/config/daemon_config_test.go
git commit -m "feat: add env overrides for [auth.proxy] keys"
```

---

## Task 3: Listener-trust matcher

**Files:**
- Create: `internal/daemon/trusted_actor.go`
- Create: `internal/daemon/trusted_actor_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/trusted_actor_test.go`:

```go
package daemon

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListenerTrusted(t *testing.T) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", "100.64.0.5:7777")
	if err != nil {
		t.Fatal(err)
	}
	unixAddr := &net.UnixAddr{Name: "/run/kata/proxy.sock", Net: "unix"}
	otherTCP, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:9999")

	cases := []struct {
		name      string
		local     net.Addr
		allowlist []string
		want      bool
	}{
		{"empty allowlist", tcpAddr, nil, false},
		{"nil addr", nil, []string{"100.64.0.5:7777"}, false},
		{"tcp match", tcpAddr, []string{"100.64.0.5:7777"}, true},
		{"tcp no match", otherTCP, []string{"100.64.0.5:7777"}, false},
		{"unix match with prefix", unixAddr, []string{"unix:///run/kata/proxy.sock"}, true},
		{"unix match plain path", unixAddr, []string{"/run/kata/proxy.sock"}, true},
		{"unix no match", unixAddr, []string{"/different/path"}, false},
		{"whitespace in entry trimmed", tcpAddr, []string{"  100.64.0.5:7777 "}, true},
		{"wildcard 0.0.0.0 never matches", tcpAddr, []string{"0.0.0.0:7777"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listenerTrusted(tc.local, tc.allowlist)
			assert.Equal(t, tc.want, got)
		})
	}
}
```

- [ ] **Step 2: Run the test and verify it fails (compile error)**

Run: `go test ./internal/daemon/ -run TestListenerTrusted -v`
Expected: FAIL — `undefined: listenerTrusted`.

- [ ] **Step 3: Implement the matcher**

Create `internal/daemon/trusted_actor.go`:

```go
package daemon

import (
	"net"
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
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestListenerTrusted -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/trusted_actor.go internal/daemon/trusted_actor_test.go
git commit -m "feat: add listener-trust matcher for trusted-proxy actor mode"
```

---

## Task 4: Add `PrincipalTrustedProxy` and `PrincipalTrustedProxyAbsent` kinds

**Files:**
- Modify: wherever PR #65 defines `PrincipalKind` and the existing constants (likely `internal/daemon/auth.go`).
- Test: wherever PR #65 tests `PrincipalKind`/`Principal`.

- [ ] **Step 1: Write the failing tests**

Append to PR #65's principal test file (or create `internal/daemon/principal_proxy_test.go` if there's no obvious home):

```go
func TestPrincipalKind_TrustedProxyConstants(t *testing.T) {
	// The two new kinds must be distinct values that round-trip through
	// the existing String() / printable form PR #65 provides on
	// PrincipalKind (so logs and audit reads don't collapse them).
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalTrustedProxyAbsent)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalDBToken)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalStaticToken)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalBootstrap)
	// If PR #65 added a Stringer on PrincipalKind, lock the names:
	// assert.Equal(t, "trusted_proxy", PrincipalTrustedProxy.String())
	// assert.Equal(t, "trusted_proxy_absent", PrincipalTrustedProxyAbsent.String())
}
```

(Adjust to whatever surface PR #65 exposes on `PrincipalKind` — the test should at minimum lock the new constants as distinct values.)

- [ ] **Step 2: Run the test and verify it fails (compile error: undefined constants)**

Run: `go test ./internal/daemon/ -run TestPrincipalKind_TrustedProxyConstants -v`
Expected: FAIL.

- [ ] **Step 3: Add the new principal kinds**

In the file where PR #65 defines `PrincipalKind`, extend the constant block:

```go
const (
	// ... PR #65's existing kinds: PrincipalStaticToken, PrincipalBootstrap, PrincipalDBToken
	PrincipalTrustedProxy        // set by the trusted-proxy middleware when the header is present.
	PrincipalTrustedProxyAbsent  // set by the trusted-proxy middleware when the header is missing/empty on a trusted listener.
)
```

If PR #65 added a `Stringer` on `PrincipalKind`, extend its switch with the two new cases.

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestPrincipalKind_TrustedProxyConstants -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/auth.go internal/daemon/principal_proxy_test.go
git commit -m "feat: add trusted-proxy principal kinds"
```

(File names depend on where PR #65 put things.)

---

## Task 5: `withTrustedProxyActor` middleware + matrix test

**Files:**
- Modify: `internal/daemon/trusted_actor.go`
- Modify: `internal/daemon/trusted_actor_test.go`

- [ ] **Step 1: Write the failing matrix test**

Append to `internal/daemon/trusted_actor_test.go`. Extend the import block with:
- `"context"`, `"net/http"`, `"net/http/httptest"`
- `"github.com/stretchr/testify/require"`
- `"go.kenn.io/kata/internal/config"`

```go
func TestWithTrustedProxyActor_Matrix(t *testing.T) {
	tcpAddr, _ := net.ResolveTCPAddr("tcp", "100.64.0.5:7777")
	otherTCP, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:9999")

	type want struct {
		kind  PrincipalKind // zero value = principal unchanged
		actor string
	}
	cases := []struct {
		name     string
		header   string
		local    net.Addr
		auth     config.AuthConfig
		// optional "incoming" principal set by PR #65's bearer middleware;
		// the trusted-proxy middleware should only overwrite it on a trusted listener.
		incoming Principal
		want     want
	}{
		{
			name:   "mode off, no header set",
			header: "", local: tcpAddr,
			auth:     config.AuthConfig{},
			incoming: Principal{Kind: PrincipalDBToken, Actor: "alice"},
			want:     want{kind: PrincipalDBToken, actor: "alice"}, // unchanged
		},
		{
			name:   "mode off, header sent but ignored",
			header: "alice", local: tcpAddr,
			auth:     config.AuthConfig{Proxy: config.ProxyConfig{TrustedProxyListeners: []string{"100.64.0.5:7777"}}},
			incoming: Principal{},
			want:     want{},
		},
		{
			name:   "mode on, untrusted listener",
			header: "alice", local: otherTCP,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			incoming: Principal{Kind: PrincipalDBToken, Actor: "alice"},
			want:     want{kind: PrincipalDBToken, actor: "alice"}, // unchanged
		},
		{
			name:   "mode on, trusted listener, header set -> overwrite",
			header: "alice", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			incoming: Principal{Kind: PrincipalDBToken, Actor: "token-bob"},
			want:     want{kind: PrincipalTrustedProxy, actor: "alice"},
		},
		{
			name:   "mode on, trusted listener, header missing -> absent",
			header: "", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			want: want{kind: PrincipalTrustedProxyAbsent},
		},
		{
			name:   "mode on, trusted listener, whitespace-only header -> absent",
			header: "   ", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			want: want{kind: PrincipalTrustedProxyAbsent},
		},
		{
			name:   "mode on, trusted listener, header trimmed before storing",
			header: "  bob  ", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			want: want{kind: PrincipalTrustedProxy, actor: "bob"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Principal
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if p, ok := PrincipalFromContext(r.Context()); ok {
					got = p
				}
				w.WriteHeader(http.StatusOK)
			})
			h := withTrustedProxyActor(ServerConfig{Auth: tc.auth})(next)

			req := httptest.NewRequest("POST", "/x", nil)
			if tc.header != "" {
				req.Header.Set("X-Kata-Actor", tc.header)
			}
			ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, tc.local)
			// Simulate the upstream bearer middleware having already set a principal:
			if tc.incoming.Kind != 0 {
				ctx = WithPrincipal(ctx, tc.incoming)
			}
			req = req.WithContext(ctx)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)
			assert.Equal(t, tc.want.kind, got.Kind)
			assert.Equal(t, tc.want.actor, got.Actor)
		})
	}
}
```

(`PrincipalFromContext` and `WithPrincipal` are PR #65's helpers — confirm exact names after #65 merges.)

- [ ] **Step 2: Run the test and verify it fails (compile error)**

Run: `go test ./internal/daemon/ -run TestWithTrustedProxyActor_Matrix -v`
Expected: FAIL — `undefined: withTrustedProxyActor`.

- [ ] **Step 3: Implement the middleware**

Append to `internal/daemon/trusted_actor.go`. Extend the import block with `"context"`, `"net/http"`:

```go
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
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestWithTrustedProxyActor_Matrix -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/trusted_actor.go internal/daemon/trusted_actor_test.go
git commit -m "feat: add withTrustedProxyActor middleware"
```

---

## Task 6: Extend `actorFor` + `ensureAttributedWriteAllowed` for the new kinds

**Files:**
- Modify: wherever PR #65 defines `actorFor` and `ensureAttributedWriteAllowed` (likely `internal/daemon/auth.go` or `internal/daemon/server.go`).
- Test: wherever PR #65 tests them.

- [ ] **Step 1: Write the failing tests**

Append:

```go
func TestActorFor_TrustedProxy(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind:  PrincipalTrustedProxy,
		Actor: "proxy-user",
	})
	got := actorFor(ctx, "client-claim")
	assert.Equal(t, "proxy-user", got, "trusted-proxy principal wins over supplied actor")
}

func TestEnsureAttributedWriteAllowed_TrustedProxyAbsent(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind: PrincipalTrustedProxyAbsent,
	})
	err := ensureAttributedWriteAllowed(ctx)
	require.Error(t, err)

	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 400, apiErr.Status)
	assert.Equal(t, "actor_header_required", apiErr.Code)
}

func TestEnsureAttributedWriteAllowed_TrustedProxyAllowed(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind:  PrincipalTrustedProxy,
		Actor: "proxy-user",
	})
	require.NoError(t, ensureAttributedWriteAllowed(ctx),
		"PrincipalTrustedProxy must be allowed to do attributed writes")
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./internal/daemon/ -run TestActorFor_TrustedProxy -run TestEnsureAttributedWriteAllowed_TrustedProxy -v`
Expected: FAIL — the existing `actorFor` / `ensureAttributedWriteAllowed` don't recognize the new kinds.

- [ ] **Step 3: Extend the resolvers**

Add the two new branches to PR #65's existing functions:

```go
func actorFor(ctx context.Context, requestActor string) string {
	if p, ok := PrincipalFromContext(ctx); ok {
		switch p.Kind {
		// ... PR #65's existing cases (PrincipalDBToken returns p.Actor, etc.) ...
		case PrincipalTrustedProxy:
			return p.Actor
		}
	}
	return requestActor
}

func ensureAttributedWriteAllowed(ctx context.Context) error {
	if p, ok := PrincipalFromContext(ctx); ok {
		switch p.Kind {
		// ... PR #65's existing cases (PrincipalBootstrap rejects, etc.) ...
		case PrincipalTrustedProxyAbsent:
			return api.NewError(400, "actor_header_required",
				"actor header required on this listener but was missing or empty",
				"", nil)
		}
	}
	return nil
}
```

(Adjust to match PR #65's exact switch shape; the new branches go alongside the existing ones.)

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./internal/daemon/ -run TestActorFor_TrustedProxy -run TestEnsureAttributedWriteAllowed_TrustedProxy -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/auth.go internal/daemon/auth_test.go
git commit -m "feat: route trusted-proxy principals through attributedActor"
```

---

## Task 7: Wire `withTrustedProxyActor` into the middleware chain

**Files:**
- Modify: `internal/daemon/server.go`

- [ ] **Step 1: Update the handler composition**

In `NewServer`, locate the line that builds the handler chain after PR #65 (something like):

```go
s.handler = withCSRFGuards(requireBearer(cfg.authPolicy())(mux))
```

or (if PR #65's identity-bearer wraps it):

```go
s.handler = withCSRFGuards(requireIdentityBearer(...)(mux))
```

Insert `withTrustedProxyActor(cfg)` just inside the auth middleware so it runs after the principal is initialized:

```go
s.handler = withCSRFGuards(requireBearer(cfg.authPolicy())(withTrustedProxyActor(cfg)(mux)))
```

- [ ] **Step 2: Run the full daemon test suite to confirm nothing regressed**

Run: `go test ./internal/daemon/`
Expected: PASS — with no `Auth.Proxy.TrustedActorHeader` set in existing test configs, `withTrustedProxyActor` is a pass-through.

- [ ] **Step 3: Run the linter**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./internal/daemon/`
Expected: zero warnings.

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/server.go
git commit -m "feat: wire withTrustedProxyActor into the middleware chain"
```

---

## Task 8: Integration test — trusted listener credits the header value

**Files:**
- Create: `internal/daemon/trusted_actor_e2e_test.go`

This test uses a pre-bound TCP listener so the trusted-listener allowlist can be set before `NewServer` builds the middleware chain.

- [ ] **Step 1: Write the test**

Create `internal/daemon/trusted_actor_e2e_test.go`:

```go
package daemon_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// startTrustedProxyTestServer binds a loopback TCP listener, builds a
// daemon.Server whose trusted-proxy allowlist contains that listener's
// address, and swaps the listener into httptest. Pre-binding is necessary
// because the allowlist is captured at NewServer time and
// httptest.NewServer would pick its own random port.
func startTrustedProxyTestServer(t *testing.T, headerName string, database *db.DB) *httptest.Server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	cfg := daemon.ServerConfig{
		DB:        database,
		StartedAt: time.Now(),
		Auth: config.AuthConfig{
			Proxy: config.ProxyConfig{
				TrustedActorHeader:    headerName,
				TrustedProxyListeners: []string{l.Addr().String()},
			},
		},
	}
	srv := daemon.NewServer(cfg)
	ts := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, ts.Listener.Close())
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func TestTrustedProxyHeader_CreditsHeaderActor(t *testing.T) {
	// Open a test DB the same way other handler tests do — look in
	// internal/daemon/testhelpers_test.go for the established pattern
	// (e.g., openTestDB) and call it directly rather than going through
	// bootstrapProject, which builds its own server we can't swap a
	// listener into.
	tdb := openTestDB(t)

	ts := startTrustedProxyTestServer(t, "X-Kata-Actor", tdb)

	// Seed via HTTP setup helpers. Issue creation calls attributedActor
	// (post-PR-#65), so pass X-Kata-Actor on the issue-create call.
	// initProject does not call attributedActor; the header is optional
	// there but harmless.
	projectID := trustedProxyCreateProject(t, ts, map[string]string{"X-Kata-Actor": "setup"})
	issueRef := trustedProxyCreateIssue(t, ts, projectID, map[string]string{"X-Kata-Actor": "setup"})

	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "verified end-to-end after the fix landed.",
		"source":  "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{"X-Kata-Actor": "proxy-user"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	var payload struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotNil(t, payload.Event)
	assert.Equal(t, "proxy-user", payload.Event.Actor,
		"event.actor must reflect the header value, not the body actor")
}
```

`doReq` already exists in `internal/daemon/testhelpers_test.go`. `openTestDB`, `trustedProxyCreateProject`, `trustedProxyCreateIssue` are stand-in names — replace with the actual db-setup helper used by `bootstrapProject` and two small local helpers that POST to `/api/v1/projects` (`initProject`) and `/api/v1/projects/{id}/issues` (`createIssue`) via `postJSON`.

- [ ] **Step 2: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestTrustedProxyHeader_CreditsHeaderActor -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/daemon/trusted_actor_e2e_test.go
git commit -m "test: integration test that trusted header is credited as actor"
```

---

## Task 9: Integration test — trusted listener, missing header rejects with 400

**Files:**
- Modify: `internal/daemon/trusted_actor_e2e_test.go`

- [ ] **Step 1: Write the test**

Append:

```go
func TestTrustedProxyHeader_MissingHeaderRejects(t *testing.T) {
	tdb := openTestDB(t)
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor", tdb)

	projectID := trustedProxyCreateProject(t, ts, map[string]string{"X-Kata-Actor": "setup"})
	issueRef := trustedProxyCreateIssue(t, ts, projectID, map[string]string{"X-Kata-Actor": "setup"})

	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "should be rejected because header is missing.",
		"source":  "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, nil) // no X-Kata-Actor on the close call
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, "actor_header_required", env.Error.Code)
}
```

- [ ] **Step 2: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestTrustedProxyHeader_MissingHeaderRejects -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/daemon/trusted_actor_e2e_test.go
git commit -m "test: integration test that missing header on trusted listener rejects"
```

---

## Task 10: Integration test — reads on a trusted listener are not blocked

**Files:**
- Modify: `internal/daemon/trusted_actor_e2e_test.go`

- [ ] **Step 1: Write the test**

Append:

```go
func TestTrustedProxyHeader_ReadsNotBlocked(t *testing.T) {
	tdb := openTestDB(t)
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor", tdb)

	// GET /api/v1/health is a read with no actor and no required setup.
	// On a trusted listener with no header, the middleware stores a
	// trusted-but-absent principal; the health handler never calls
	// attributedActor, so it must still return 200.
	resp, raw := doReq(t, ts, "GET", "/api/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
}
```

- [ ] **Step 2: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestTrustedProxyHeader_ReadsNotBlocked -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/daemon/trusted_actor_e2e_test.go
git commit -m "test: integration test that reads on trusted listener are not blocked"
```

---

## Task 11: Integration test — mode-off path is unchanged

**Files:**
- Modify: `internal/daemon/trusted_actor_e2e_test.go`

- [ ] **Step 1: Write the test**

Append:

```go
func TestTrustedProxyHeader_ModeOffUnchanged(t *testing.T) {
	// Mode off (empty TrustedActorHeader). The header is ignored, and the
	// body actor (or whatever PR #65's other principal sources produce) is
	// used as today.
	tdb := openTestDB(t)
	ts := startTrustedProxyTestServer(t, "" /* mode off */, tdb)

	projectID := trustedProxyCreateProject(t, ts, nil)
	issueRef := trustedProxyCreateIssue(t, ts, projectID, nil)

	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "mode off; body actor must be used.",
		"source":  "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{"X-Kata-Actor": "should-be-ignored"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	var payload struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotNil(t, payload.Event)
	assert.Equal(t, "client-claim", payload.Event.Actor,
		"mode off: header is ignored, body actor wins")
}
```

- [ ] **Step 2: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestTrustedProxyHeader_ModeOffUnchanged -v`
Expected: PASS.

- [ ] **Step 3: Run the entire test + lint pipeline one final time**

Run: `go test ./... && nix run 'nixpkgs#golangci-lint' -- run`
Expected: full PASS, zero warnings across the whole repo.

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/trusted_actor_e2e_test.go
git commit -m "test: integration test that mode-off path is byte-for-byte unchanged"
```

---

## Done

When all 11 tasks are complete:

- `feat/issue-58` carries the design spec (already committed) plus the implementation: `[auth.proxy]` config keys, env overrides, listener matcher, two new principal kinds, the `withTrustedProxyActor` middleware, the chokepoint extensions to `actorFor` and `ensureAttributedWriteAllowed`, the wiring, and four integration tests (header credited, missing header rejects, reads not blocked, mode-off unchanged).
- Every acceptance criterion from issue #58 is mapped to a task: config + env (Tasks 1-2), trusted listener credits header / conflict ignored (Task 8), missing header rejects (Task 9), non-trusted listener ignores header (Task 5 matrix), mode-off byte-for-byte unchanged (Task 11), middleware matrix + chokepoint unit tests (Tasks 5 + 6).
- Confirm with the user before pushing or marking the existing draft PR ready for review.
