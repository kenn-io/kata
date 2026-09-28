---
title: Go client
description: Connect Go programs to a Kata daemon with the typed client, CLI-compatible daemon discovery, and an in-process test server.
last_edited: 2026-09-28
---

# Go client

Go programs talk to a Kata daemon through `go.kenn.io/kata/pkg/client`. The
client is generated from the [HTTP API schema](http-api.md) and uses the same
daemon selection, transports, and bearer-token rules as the Kata CLI, so an
integration does not have to reimplement them.

```sh
go get go.kenn.io/kata
```

To host Kata inside a Go program instead of talking to a daemon, see
[Embedding kata in Go](../development/embedding.md).

## Connect to the user's daemon

`client.Discover` returns a client for the daemon the Kata CLI would use:

```go
api, err := client.Discover(ctx, client.DiscoverOptions{Workspace: repoRoot})
if errors.Is(err, client.ErrDaemonUnavailable) {
	// No reachable local daemon is available here.
}
```

Discover checks the same sources as
[`kata daemon locate`](daemon-discovery.md#selection-order), in order:

1. `KATA_SERVER`.
2. `[server].url` or `[server].daemon` in the nearest `.kata.local.toml`,
   walking upward from `Workspace`, or from the process working directory
   when `Workspace` is empty. `[server].daemon` selects a pinned local profile.
3. The `active_daemon` catalog entry, which may be a remote or pinned local
   profile.
4. The running default local daemon.

The bearer token comes from the selected source, as it does for the CLI. The
default local daemon uses `KATA_AUTH_TOKEN`, then the current home's
`[auth].token`. A pinned local profile uses its own auth configuration; its
catalog `token` or `token_env` can select a client credential in identity-token
mode.

Discover and `kata daemon locate` both inspect the selected daemon without
starting it. Use `kata daemon diagnose` for stopped-profile state and recovery
guidance; `kata daemon recover` can start a local profile after its existing
storage and identity pass the recovery checks.

Discover does not probe configured remotes. The first request reports whether
a remote is reachable.

Discover returns an error matching `client.ErrDaemonUnavailable` when no
daemon is selected or a selected local daemon has no reachable runtime. A
local profile whose storage or instance identity cannot be verified returns
the corresponding profile-specific error. A local process whose socket or
port cannot be reached includes that endpoint in the error.

## Connect to a known endpoint

Pass an endpoint to a constructor when your program already knows which daemon
to use. An endpoint is an HTTP(S) origin such as `https://daemon.example`, or a
Unix socket written as `unix:///absolute/path/daemon.sock`.

| Constructor | Bearer token sent |
| --- | --- |
| `client.New(endpoint)` | None. |
| `client.NewWithGlobalAuth(ctx, endpoint)` | `KATA_AUTH_TOKEN`, then `[auth].token`. |
| `client.NewWithBearer(ctx, endpoint, token)` | `token`. |
| `client.NewForTarget(ctx, endpoint, client.TargetAuth{Token: token})` | `TargetAuth.Token`. Global auth config is not read. |

Constructors that send a token refuse plaintext HTTP to a host other than
loopback. `TargetAuth.TrustPrivateNetwork` allows literal private IPs, and
`TargetAuth.AllowInsecure` allows any host; see
[plain HTTP guardrails](../operations/remote-daemon.md#plain-http-guardrails).
Set request timeouts with `client.WithTransportOptions`.

## Handle errors

Every operation returns an error for a non-success response. `*WithResponse`
methods also return the response, with the raw body in `resp.Body`.

Daemon errors carry a structured envelope with a stable `code`:

```go
_, err := api.ShowProjectWithResponse(ctx, opts)
if envelope, ok := errors.AsType[generated.ErrorEnvelope](err); ok {
	log.Printf("kata %s: %s", envelope.ErrorData.Code, envelope.ErrorData.Message)
}
if client.StatusCode(err) == http.StatusUnauthorized {
	// Ask the user for a token.
}
```

`client.StatusCode` reports the HTTP status even when the body is not a Kata
envelope, such as a proxy's error page. It returns `0` when no response
arrived, for example when the connection failed.

## Test against a real daemon

`go.kenn.io/kata/pkg/katatest` starts an in-process Kata service with its own
SQLite database for one test, and stops it when the test ends:

```go
func TestFilesIssue(t *testing.T) {
	server := katatest.New(t, katatest.WithToken("test-token"))
	api := server.Client(t)
	// Or hand server.Endpoint and the token to the code under test.
}
```

- `katatest.WithToken(token)` requires that bearer token on API requests.
  Without it, the service trusts callers the way an unauthenticated local
  daemon does.
- `katatest.WithUnixSocket()` serves on a Unix socket. `server.Endpoint` is
  then a `unix:///...` endpoint.

As with any daemon that has no identity tokens, write requests must name an
`actor` in the request body.

To exercise `client.Discover`, set `KATA_SERVER` to `server.Endpoint` and
`KATA_AUTH_TOKEN` to the token.
