---
title: Go client
description: Connect Go programs to a Kata daemon with the typed client, CLI-compatible daemon discovery, and an in-process test server.
last_edited: 2026-11-20
---

# Go client

Connect your Go programs to a Kata daemon with `go.kenn.io/kata/pkg/client`.
The client is generated from the [HTTP API schema](http-api.md) and uses the
same daemon selection, transports, and bearer-token rules as the Kata CLI, so
your integration does not have to reimplement them.

Importing `pkg/client` does not link the daemon, its storage engines, or sync
connectors, so client-only programs stay small. The
[test server](#test-against-a-real-daemon) in `pkg/katatest` runs a real daemon
and links all of them, so import it only in tests.

```sh
go get go.kenn.io/kata
```

To host Kata inside a Go program instead of talking to a daemon, see
[Embedding kata in Go](../development/embedding.md).

## Connect to the user's daemon

`client.Discover` returns a client connected to the same daemon the Kata CLI
would use:

```go
api, err := client.Discover(ctx, client.DiscoverOptions{Workspace: repoRoot})
if errors.Is(err, client.ErrDaemonUnavailable) {
	// No reachable local daemon is available here.
}
```

`Discover` checks the same sources as
[`kata daemon locate`](daemon-discovery.md#selection-order), in order:

1. `KATA_SERVER`.
2. `[server].url` or `[server].daemon` in the nearest `.kata.local.toml`,
   walking upward from `Workspace`, or from the process working directory
   when `Workspace` is empty. `[server].daemon` selects a pinned local profile.
3. The `active_daemon` catalog entry, which can be a remote or a pinned local
   profile.
4. The running default local daemon.

The bearer token comes from the selected source, as it does in the CLI.
The default local daemon uses `KATA_AUTH_TOKEN`, then the current home's
`[auth].token`. A pinned local profile uses its own auth configuration, and its
catalog `token`, `token_file`, or `token_env` can select a client credential in
identity-token mode.

Both `Discover` and `kata daemon locate` inspect the selected daemon without
starting it. To check stopped-profile state and get recovery guidance, run
`kata daemon diagnose`; `kata daemon recover` can start a local profile once
its existing storage and identity pass the recovery checks. To have `Discover`
start a stopped local daemon for you, set
[`StartWith`](#start-a-stopped-local-daemon).

`Discover` does not probe configured remotes, even when you select one by name.
Your first request reports whether the remote is reachable; to check sooner,
call `api.PingWithResponse(ctx)`.

`Discover` finds a PostgreSQL-backed default local daemon from its storage DSN.
Because client-only programs do not include the PostgreSQL driver, they cannot
resolve DSN routing options: the `host`, `port`, `dbname`, `service`, or
`servicefile` query options, or `PGHOST`, `PGPORT`, `PGDATABASE`, or
`PGSERVICE` filling in a part the URL leaves out. When those are used,
`Discover` returns an error. Set `KATA_SERVER` to the daemon's endpoint, which
`kata daemon locate` prints.

When no daemon is selected or a selected local daemon has no reachable runtime,
`Discover` returns an error matching `client.ErrDaemonUnavailable`. A local
profile whose storage or instance identity cannot be verified returns the
corresponding profile-specific error, and a local process whose socket or port
cannot be reached includes that endpoint in the error.

### Check which daemon Discover selected

Call `Discovered` when you need to branch on whether the daemon is remote, for
example to avoid passing it local file paths:

```go
if target, ok := api.Discovered(); ok && target.Remote {
	// Do not send this daemon local file paths.
}
```

`Discovered` returns a `client.DiscoveredDaemon` whose `Remote` field is true
for a configured remote daemon. Kata clients treat a remote daemon as unable to
read the caller's files, even at a loopback address, and resolve projects
without sending local paths. This matches `kind=remote` in
[`kata daemon locate`](daemon-discovery.md#output-contract). A pinned local
profile selected by `[server].daemon` or `active_daemon` is local.

For a client built from an explicit endpoint, such as `New` or `NewForTarget`,
`ok` is false. `DiscoveredDaemon` exposes no credentials.

### Connect to a catalog daemon by name

`client.DaemonCatalog` lists the `[[daemon]]` entries in
`<KATA_HOME>/config.toml` in file order, matching the choices in the TUI daemon
switcher:

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
| `HasToken` | True when the entry sets `token`, `token_file` or `token_env`. Kata reads no credential files or variables here, so the selected source may still be unavailable. |
| `Active` | True for the entry `active_daemon` names. |

Entries carry no token values. If there is no config file, the list is empty;
if the catalog is invalid, `DaemonCatalog` returns the error the CLI reports.
See [Daemon config](configuration.md#daemon-config) for the entry fields.

To connect to one entry, set `DiscoverOptions.Daemon`, which works like the
global `--daemon` flag:

```go
api, err := client.Discover(ctx, client.DiscoverOptions{Daemon: "example-workspace"})
```

The named entry replaces every other selection source and ignores `Workspace`.
`Discover` resolves it with the same catalog rules as `--daemon`:

- A remote entry resolves `token` > `token_file` > `token_env`.
  `KATA_AUTH_TOKEN` overrides the catalog credential without reading its source.
- A local entry uses the same catalog precedence. Without a selector, it falls
  back to `KATA_AUTH_TOKEN`, then the current home's `[auth].token`.
- A failed selected file or an unset or empty selected `token_env` returns an
  error without fallback, unless `KATA_AUTH_TOKEN` overrides the credential for
  a remote entry. Files follow the
  [catalog credential rules](configuration.md#daemon-config).
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

To start a stopped local daemon automatically, set `DiscoverOptions.StartWith`
to the path of a `kata` executable, as the TUI daemon switcher does:

```go
api, err := client.Discover(ctx, client.DiscoverOptions{
	Daemon:    "example-workspace",
	StartWith: kataPath,
})
```

`Discover` runs that executable, waits for it to exit, and then connects:

| Selected daemon | Command |
| --- | --- |
| The current home's daemon, or a plain `local = true` entry | `kata daemon start` |
| A pinned local profile chosen by `Daemon` | `kata --daemon <name> daemon recover` |
| A pinned local profile chosen by `.kata.local.toml` or `active_daemon` | `kata [--workspace <Workspace>] daemon recover` |

`daemon recover` checks the profile's existing storage and instance UID before
and after starting it, and never creates storage. `Discover` never starts a
remote, or a local daemon whose process is running but unreachable. If the
command fails, `Discover` returns an error matching
`client.ErrDaemonUnavailable` with the command's output included. The child
process inherits the calling process's environment and working directory.

## Connect to a known endpoint

When your program already knows which daemon to use, pass the endpoint directly
to a constructor. An endpoint is an HTTP(S) origin such as
`https://daemon.example`, or a Unix socket written as
`unix:///absolute/path/daemon.sock`.

| Constructor | Bearer token sent |
| --- | --- |
| `client.New(endpoint)` | None. |
| `client.NewWithGlobalAuth(ctx, endpoint)` | `KATA_AUTH_TOKEN`, then `[auth].token`. |
| `client.NewWithBearer(ctx, endpoint, token)` | `token`. |
| `client.NewForTarget(ctx, endpoint, client.TargetAuth{Token: token})` | `TargetAuth.Token`. Global auth config is not read. |

Constructors that send a token refuse plaintext HTTP to any host other than
loopback. `TargetAuth.TrustPrivateNetwork` allows literal private IPs, and
`TargetAuth.AllowInsecure` allows any host; see
[plain HTTP guardrails](../operations/remote-daemon.md#plain-http-guardrails).
To set request timeouts, pass `client.WithTransportOptions`.

## Follow the event stream

`api.Events` reads `/api/v1/events/stream` for you, framing the stream, joining
multi-line data, and skipping keepalives. It yields each event as it arrives:

```go
opts := client.EventsOptions{LastEventID: cursor} // ProjectID limits to one project
for event, err := range api.Events(ctx, opts) {
	if err != nil {
		break // see below
	}
	cursor = event.ID
	if event.Oversized {
		// The event passed 8 MiB and was skipped: refetch what you cache, then
		// carry on after event.ID.
		continue
	}
	if event.Reset != nil {
		// The cursor fell behind a purge: drop cached state, then resume from
		// event.Reset.ResetAfterID.
		continue
	}
	handle(event.Type, event.Data) // Data is the event's JSON object
}
```

The loop ends with an error. `client.ErrEventStreamEnded` means the daemon
closed the stream, so reconnect from the last `event.ID`; the daemon also
closes the stream after a reset. To stop reading, cancel `ctx` or `break` out
of the loop. `Events` makes one connection and does not reconnect on its own.
If you need the raw response, call `StreamEventsRaw`.

## Handle errors

Every operation returns an error on a non-success response, and `*WithResponse`
methods also return the response with the raw body in `resp.Body`.

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

`client.StatusCode` returns the HTTP status even when the body is not a Kata
envelope, such as a proxy's error page. When no response arrives, for example
when the connection fails, it returns `0`.

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
- `katatest.WithUnixSocket()` serves on a Unix socket, setting
  `server.Endpoint` to a `unix:///...` endpoint.

As with any daemon that has no identity tokens, write requests must name an
`actor` in the request body.

To exercise `client.Discover`, set `KATA_SERVER` to `server.Endpoint` and
`KATA_AUTH_TOKEN` to the token.

## Cron identities

Call `client.NewCronUID()` once per definition or independent run and retain
that normalized ULID with its intended request. The helper makes no network
request, and generated clients never substitute a new UID after an error.

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

Use `ExpectedRevision: 0` for new evidence, and pass the current revision when
changing mutable evidence. Exact same-write retries return replayed without
another event or revision, even with a stale expected revision. Immutable
identity changes conflict. Distinct run UIDs can share an occurrence key and
issue, and statuses are evidence that never authorize a process.

For stable job or workflow creation, keep a UID and call `ReplaceCronJob` or
`ReplaceCronWorkflow` with that path UID and absent `ExpectedEventUID`. If you
receive a 409, inspect that same UID and compare the complete live definition;
later edits and tombstones are not successful creation retries.

Typed definition options retain exact JSON numbers as `json.Number`, including
inside nested objects and arrays. Preserve those values when re-submitting a
definition, because converting them to float64 can round large integers and
decimals. Both plain and `WithResponse` methods preserve them. Published
response schemas accept additive fields while request schemas stay strict; when
a generated response type differs from the request type, marshal and decode the
definition into its request DTO before submitting it. You can read native
planning dates with `IssuePlanningDatesWithResponse` using ordinary project and
issue read policy. For the full contract, see [Cron](cron.md).
