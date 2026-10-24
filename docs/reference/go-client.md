---
title: Go client
description: Connect Go programs to a Kata daemon with the typed client, CLI-compatible daemon discovery, and an in-process test server.
last_edited: 2026-10-24
---

# Go client

Go programs talk to a Kata daemon through `go.kenn.io/kata/pkg/client`. The
client is generated from the [HTTP API schema](http-api.md) and uses the same
daemon selection, transports, and bearer-token rules as the Kata CLI, so an
integration does not have to reimplement them.

Importing `pkg/client` does not link the daemon, its storage engines,
telemetry, or sync connectors, so client-only programs stay small. The
[test server](#test-against-a-real-daemon) in `pkg/katatest` runs a real
daemon and links all of them; import it only from tests.

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
storage and identity pass the recovery checks. Set
[`StartWith`](#start-a-stopped-local-daemon) to have Discover start it.

Discover does not probe configured remotes, including one selected by name.
The first request reports whether a remote is reachable; call
`api.PingWithResponse(ctx)` to check sooner.

Discover finds a PostgreSQL-backed default local daemon from its storage DSN.
Client-only programs do not include the PostgreSQL driver, so they cannot
resolve DSN routing options: the `host`, `port`, `dbname`, `service`, or
`servicefile` query options, or `PGHOST`, `PGPORT`, `PGDATABASE`, or
`PGSERVICE` filling in a part the URL leaves out. Discover then returns an
error. Set `KATA_SERVER` to the daemon's endpoint, which `kata daemon locate`
prints.

Discover returns an error matching `client.ErrDaemonUnavailable` when no
daemon is selected or a selected local daemon has no reachable runtime. A
local profile whose storage or instance identity cannot be verified returns
the corresponding profile-specific error. A local process whose socket or
port cannot be reached includes that endpoint in the error.

### Check which daemon Discover selected

Call `Discovered` to branch on whether the daemon is remote, such as to avoid
passing it local file paths:

```go
if target, ok := api.Discovered(); ok && target.Remote {
	// Do not send this daemon local file paths.
}
```

`Discovered` returns a `client.DiscoveredDaemon`. Its `Remote` field is true
for a configured remote daemon. Kata clients treat such a daemon as unable to
read the caller's files, even at a loopback address, and resolve projects
without sending local paths. It matches `kind=remote` in
[`kata daemon locate`](daemon-discovery.md#output-contract). A pinned local
profile selected by `[server].daemon` or `active_daemon` is local.

`ok` is false for a client built from an explicit endpoint, such as `New` or
`NewForTarget`. `DiscoveredDaemon` exposes no credentials.

### Connect to a catalog daemon by name

`client.DaemonCatalog` lists the `[[daemon]]` entries in
`<KATA_HOME>/config.toml` in file order, the same choices the TUI daemon
switcher offers:

```go
daemons, err := client.DaemonCatalog()
for _, d := range daemons {
	fmt.Println(d.Name, d.Endpoint, d.HasToken, d.Active)
}
```

| Field | Value |
| --- | --- |
| `Name` | The entry's `name`. |
| `Local` | True for a `local = true` entry, including a pinned local profile. |
| `Endpoint` | `local` for a local entry. For a remote entry, the URL's host and port, such as `daemon.example:7443`. |
| `HasToken` | True when the entry sets `token` or `token_env`. Kata does not read the `token_env` variable here, so it can still be unset. |
| `Active` | True for the entry `active_daemon` names. |

Entries carry no token values. Without a config file the list is empty; an
invalid catalog returns the error the CLI reports. See
[Daemon config](configuration.md#daemon-config) for the entry fields.

Set `DiscoverOptions.Daemon` to connect to one entry, as the global `--daemon`
flag does:

```go
api, err := client.Discover(ctx, client.DiscoverOptions{Daemon: "example-workspace"})
```

The named entry replaces every other selection source, and `Workspace` is
ignored. Discover resolves it with the same catalog rules as `--daemon`:

- A remote entry sends its `token`, or the value of the variable `token_env`
  names. `KATA_AUTH_TOKEN` overrides both.
- A local entry sends its `token` or `token_env` value. Without either, it
  falls back to `KATA_AUTH_TOKEN`, then the current home's `[auth].token`.
- A `token_env` variable that is unset or empty returns an error, unless
  `KATA_AUTH_TOKEN` overrides it for a remote entry.
- A local entry connects to the running default local daemon. A pinned local
  profile (`home` and `instance_uid`) connects to that profile's daemon after
  checking its instance UID.
- A stopped local entry returns an error matching
  `client.ErrDaemonUnavailable` unless `StartWith` is set.
- A name with no catalog entry returns an error matching
  `client.ErrDaemonNotInCatalog`.

`Discovered` reports `Remote` as true for a remote entry and false for a local
one.

### Start a stopped local daemon

Set `DiscoverOptions.StartWith` to the path of a `kata` executable to start
the selected local daemon when it is stopped, as the TUI daemon switcher does:

```go
api, err := client.Discover(ctx, client.DiscoverOptions{
	Daemon:    "example-workspace",
	StartWith: kataPath,
})
```

Discover runs that executable, waits for it to exit, and connects:

| Selected daemon | Command |
| --- | --- |
| The current home's daemon, or a plain `local = true` entry | `kata daemon start` |
| A pinned local profile chosen by `Daemon` | `kata --daemon <name> daemon recover` |
| A pinned local profile chosen by `.kata.local.toml` or `active_daemon` | `kata [--workspace <Workspace>] daemon recover` |

`daemon recover` checks the profile's existing storage and instance UID before
and after starting it, and never creates storage. Discover never starts a
remote, or a local daemon whose process is running but unreachable. If the
command fails, Discover returns an error matching `client.ErrDaemonUnavailable`
that includes the command's output. The child inherits the calling process's
environment and working directory.

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

## Native cron identities

Use `client.NewCronUID()` once for a definition or independent run and
retain that normalized ULID with its intended request. The helper makes no
network request. Generated clients never substitute a new UID after an error.

```go
runUID, err := client.NewCronUID()
if err != nil {
    return err
}
// Persist runUID and body before submission; reuse both after a timeout.
result, err := api.ObserveCronRunWithResponse(ctx,
    &generated.ObserveCronRunRequestOptions{
        PathParams: &generated.ObserveCronRunPath{
            ProjectID: projectID, RunUID: runUID,
        },
        Body: &body,
    })
```

New evidence uses `ExpectedRevision: 0`; changed mutable evidence needs the
current revision. Exact same-write retries return replayed without another
event or revision, even with a stale expected revision. Immutable identity
changes conflict. Distinct run UIDs may share an occurrence key and issue.
Statuses are evidence and never authorize a process.

For stable job/workflow creation, retain a UID and use `ReplaceCronJob` or
`ReplaceCronWorkflow` with that path UID and absent `ExpectedEventUID`. A 409
requires inspecting that same UID and comparing the complete live definition;
later edits and tombstones are not successful creation retries.

Typed definition options retain exact JSON numbers as `json.Number`, including
nested objects and arrays. Preserve those values when re-submitting a definition;
converting them to float64 can round large integers and decimals. Both plain
and `WithResponse` methods preserve them. Published response schemas accept
additive fields while request schemas remain strict; where the generated
response type differs from the request type, marshal and decode the definition
into its request DTO before submitting it. Native planning dates are available
through `IssuePlanningDatesWithResponse` using ordinary project and issue read
policy. See [Native cron](cron.md) for the full contract.
