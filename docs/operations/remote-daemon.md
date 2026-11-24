---
title: Remote daemon
description: Serve Kata to trusted private-network clients with explicit authentication and transport settings.
last_edited: 2026-11-20
---

# Remote daemon

You can run a Kata daemon that serves clients on other hosts over a private
network. This mode is opt-in and built for trusted environments, not for public
multi-tenant hosting.

## Server setup

Start the daemon on a literal non-public IP address:

```sh
KATA_AUTH_TOKEN=change-me KATA_TRUST_PRIVATE_NETWORK=1 \
  kata daemon start --foreground --listen 100.64.0.5:7777
```

Or configure the server persistently in `<KATA_HOME>/config.toml`:

```toml
listen = "100.64.0.5:7777"

[auth]
token = "change-me"
trust_private_network = true
```

The CLI flag overrides the config file. Auto-started daemons also read the
listener from `config.toml`, so you only need to set it once on a host that
should always bind the same TCP address.

On the host that owns the SQLite database, run the daemon under a process
manager such as launchd, systemd, or a container runtime. Set top-level
`autostart = false` in that home's `config.toml` so CLI commands wait up to 30
seconds during a restart instead of starting a competing daemon. You can still
run `kata daemon start` explicitly. During upgrades, restart the daemon through
its service manager.

## Runtime profiling

The daemon serves Go's standard `net/http/pprof` handlers at `/debug/pprof/` on
its existing listener. Profiling requests follow the same listener and
authentication policy as other endpoints; a token-protected TCP daemon requires
its normal bearer token.

Capture and inspect CPU and heap profiles with standard Go tools:

```sh
curl -H "Authorization: Bearer $KATA_AUTH_TOKEN" \
  'http://100.64.0.5:7777/debug/pprof/profile?seconds=30' \
  -o kata.cpu.pprof
go tool pprof kata.cpu.pprof

curl -H "Authorization: Bearer $KATA_AUTH_TOKEN" \
  'http://100.64.0.5:7777/debug/pprof/heap' \
  -o kata.heap.pprof
go tool pprof kata.heap.pprof
```

The index also links to the goroutine, allocation, mutex-contention, blocking,
thread-creation, command-line, symbol, and execution-trace endpoints. To inspect
an execution trace, capture `/debug/pprof/trace?seconds=5` and open it with
`go tool trace`.

## Client setup

Point your CLI clients at the remote daemon:

```sh
export KATA_SERVER=http://100.64.0.5:7777
export KATA_AUTH_TOKEN=change-me
kata list
```

To configure a single workspace instead, put the URL in a gitignored
`.kata.local.toml`:

```toml
version = 1

[server]
url = "http://100.64.0.5:7777"
```

`KATA_SERVER` overrides `.kata.local.toml` unless you pass `--daemon <name>`.
If none of those are set, the client falls back to `active_daemon` in
`<KATA_HOME>/config.toml`, and then to local daemon discovery or auto-start.

For project-scoped commands such as `list`, `search`, and `create`, the client
resolves `.kata.toml` and Git aliases locally and never asks a remote daemon to
search your directories:

- Without either binding, the command fails with `project_not_initialized`
  (exit code 4) and suggests `kata init` or `--project`.
- If you pass a `--workspace` path that does not exist on the client, the
  command fails with a validation error (exit code 3) that names the path.

Bind a client directory or select an existing project for a single command:

```sh
kata init --project example-project
kata list --project example-project
```

Ordinary remote commands send API requests directly without a preliminary ping,
and refused connections still report `daemon_unavailable` (exit code 7).
Explicit health and discovery commands continue to probe the selected daemon.

Starting with API `0.17.0`, simple `create`, `edit`, `comment`, and `label add`
commands resolve the project inside the mutation itself, so each needs only one
HTTP request. Relationship flags retain their separate project resolution and
any necessary target checks, path-only workspaces still call the explicit
resolve endpoint, and `--comment` sends its own comment request after the
mutation. Upgrade the remote daemon alongside the CLI. Older daemons reject
named project selectors instead of silently performing a different operation.

When a mutation resolves a renamed workspace through its alias, the CLI repairs
`.kata.toml` from the successful response. If that local repair fails, the CLI
reports `workspace_repair_failed` and confirms that the mutation succeeded;
repair the binding without repeating the mutation.

Bare issue references and ULIDs use the workspace alias, including after a
project rename or merge. Passing `--project` or a qualified issue reference
selects that project name instead and leaves the workspace binding unchanged.
Edits with relationships resolve the alias and repair the binding before
submitting the mutation, because their link checks need canonical identity.

To inspect the endpoint these rules select, along with its transport and
canonical request URL, run:

```sh
kata daemon locate --json
```

This command probes configured remotes without printing or resolving their
tokens. External clients should use this supported output rather than
reimplementing the selection order. See
[Daemon discovery](../reference/daemon-discovery.md) for the schema and Unix
socket address form.

To connect to an existing home on the same machine, use a pinned
[local daemon profile](local-daemon-profiles.md) instead of guessing its
loopback URL. Explicit URL overrides, including tunnels, stay pinned and are
never automatically converted or removed.

## Browser access

Running `kata ui` with no arguments opens a selected local profile directly;
otherwise, it starts or discovers the local browser gateway and does not use
`KATA_SERVER`. When `active_daemon` is configured, the gateway selects it
initially.

To open a remote daemon directly, add it as a named `[[daemon]]` entry and run
`kata ui --daemon <name>`. If you configured the remote only through
`KATA_SERVER`, open its canonical `/kata` route in your browser instead, for
example `https://daemon.example/kata`. A remote browser route shows an
in-document token login, exchanges the token for a browser session in memory,
and never puts the token in the URL or browser storage. Identity-token mode
keeps the token actor authoritative, while static-token deployments use the
daemon's normal request-actor policy.

Behind an HTTPS reverse proxy, set `[web].public_origin` to the exact browser
origin and route the UI, assets, `/api/v1/ui/*`, ordinary `/api/v1/*`, and the
event stream to the same daemon. Do not rewrite the browser application to a
different API origin, and do not infer the public origin from forwarded headers.
When that browser listener is also a configured trusted-proxy listener, Kata
exchanges the proxy-asserted actor for a browser session automatically instead
of asking for a daemon token.

If the shared TCP listener binds a wildcard address while clients use an
intentional backend hostname (for example, a container-network alias), add that
exact authority to `[web].allowed_hosts`. Unlike `public_origin`, this admits a
backend request `Host` without declaring a new browser origin. Keep the list
narrow; bearer authentication does not bypass Host validation.

An unauthenticated `--insecure-readonly` listener can display the browser UI
with mutations disabled and updates delivered through polling. Under a writable
configuration, an authenticated user receives the capabilities of that session.

## Plain HTTP guardrails

Mutable non-loopback HTTP requires one of two configurations:

- A daemon API token plus an explicit trusted private-network opt-in.
- An explicit unauthenticated private-network write opt-in on a literal private
  IP.

Plain HTTP targets using bearer tokens must be literal non-public IPs: loopback,
RFC1918, CGNAT, link-local, or ULA. Kata rejects public IPs and DNS hostnames
for plaintext bearer auth; use HTTPS through a reverse proxy or an SSH tunnel
for those addresses.

On private overlay hostnames where you intentionally skip HTTPS, clients can opt
out per target with `KATA_ALLOW_INSECURE=1` or
`[server].allow_insecure = true`. Federation hub enrollment tokens use their own
credential store, so spokes opt in with `kata federation join
--allow-insecure`.

```toml
version = 1

[server]
url = "http://hub.internal:7777"
allow_insecure = true
```

Unix sockets, loopback HTTP, and HTTPS do not require this private-network trust
opt-in.

## Tokenless private-network writes

On a single-user private network where the network itself is the access
boundary, enable tokenless writes in `config.toml`:

```toml
listen = "100.64.0.5:7777"

[auth]
allow_unauthenticated_private_network_writes = true
```

Or start the daemon with the environment variable:

```sh
KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES=1 \
  kata daemon start --listen 100.64.0.5:7777
```

This extends Kata's local no-token trust model to the configured private IP. The
daemon accepts mutations and the event stream without a bearer token and takes
write actors from the client request body, as it does on loopback and Unix
sockets. Any device that can reach the bound address can assert any actor
string, so use this mode only when tailnet or private-network membership is your
intended trust boundary.

The listener must bind a literal private IP address: RFC1918, CGNAT, link-local,
or ULA. Kata rejects hostnames, public IPs, and wildcard binds such as
`0.0.0.0:7777` or `:7777` in this mode, and you cannot combine it with `token`,
`require_token_identity = true`, or `--insecure-readonly`.

Token administration stays authenticated-only in this mode. The daemon rejects
`/api/v1/tokens` requests rather than letting unauthenticated private-network
clients mint, list, or revoke API tokens.

## Identity tokens

For stable per-user attribution, enable identity mode in `config.toml`:

```toml
[auth]
token = "bootstrap-admin-token"
trust_private_network = true
require_token_identity = true
```

Restart the daemon, then use the bootstrap token to mint DB-backed user tokens:

```sh
export KATA_AUTH_TOKEN=bootstrap-admin-token
kata tokens create --actor wesm --name laptop
kata tokens list
kata tokens revoke 1
```

For a temporary agent worker, issue a narrower credential instead of copying a
personal or daemon-wide token onto the worker machine:

```sh
install -d -m 700 /run/user/1000/kata-worker
kata tokens create --actor worker-a --issue example-project#abc4 \
  --expires-in 4h \
  --token-file /run/user/1000/kata-worker/auth-token
```

This grant follows the live issue subtree rooted at `abc4`; see the
[scope guide](../design/issue-scoped-credentials.md) for allowed work and
membership changes. Minting a scoped token requires identity mode even when no
shared token is configured, and a federated project's token must be minted on
its hub.

Have your launcher read `/run/user/1000/kata-worker/auth-token` and pass its
value as `KATA_AUTH_TOKEN` only to the worker CLI or MCP process, alongside
`KATA_SERVER` set to the issuing daemon's exact origin. That file path is
issuance output, not an authentication input. If the file is missing or
unreadable, stop startup without falling back to a coordinator credential.
Supply a local workspace binding so the worker does not need project
initialization on the hub, and revoke the grant during teardown.

If a shared token is configured without `require_token_identity = true`,
`tokens create` refuses before minting. A token created in that legacy mode
would not be accepted as a bearer credential.

In identity mode:

- The bootstrap token can create, list, and revoke user tokens.
- The bootstrap token can perform reads.
- The bootstrap token can create or rotate direct federation enrollments for an
  explicit actor with an unscoped API token, including an expired or revoked
  token.
- Attributed writes require a DB-backed token.
- The daemon derives the actor from the token and ignores body-provided actor
  strings for mutations.

If you are an administrator, inspect retained credentials with
`kata tokens list`, press `C` in the TUI, or open **Credentials** in the Web UI.
See [credential auditing](../guide/web-ui.md#audit-provisioned-credentials) for
access requirements, lifecycle state, and last-use limitations.

Database-backed tokens cannot administer connectors by default. Turn this on
only if every active identity token should administer connectors and
external-root bridges across the daemon. Add this line to the existing `[auth]`
table:

```toml
allow_identity_connector_administration = true
```

You can also set `KATA_ALLOW_IDENTITY_CONNECTOR_ADMINISTRATION=1`. The
identity-mode bootstrap token, browser sessions, and trusted-proxy principals
still cannot administer connectors.

When you run `kata federation enroll`, the CLI authenticates to the hub using
`--hub-token-env` or a daemon-catalog credential whose URL has the same origin;
it never sends your local daemon's global token to the hub. On an identity-mode
hub, first enable the project with an account token by running
`kata federation enable`, then enroll with the hub's bootstrap token and
`--actor` naming an account with an unscoped API token (which may be expired or
revoked). The enrollment token printed by that command is separate and used only
for spoke transport. That enrollment is bound to the owner-selected actor, and
the hub rejects pushed federation events whose actor differs from that bound
actor. See [Federation token boundaries](federation.md#token-boundaries).

The daemon stores token lifecycle events in the event log and preserves them
across backup, restore, and JSONL cutover. Hidden system-project token events
stay out of ordinary project lists, stats, event feeds, and federation.

## Web or OAuth frontends

You can put a web app in front of Kata without daemon-side OAuth. Have the web
app authenticate the browser user, map that identity to a Kata actor handle, and
call the daemon with a server-side Kata bearer token. Keep the daemon token off
the browser; the browser holds only the web app session.

Because the daemon stores only token hashes, your web app cannot retrieve an
existing plaintext Kata token later. Vault plaintext tokens server-side, or mint
and revoke tokens as part of your app's own session lifecycle.

## Trusted-proxy actor header

When a reverse proxy already authenticates your users (through SSO, OAuth, or
mutual TLS), it can assert the Kata actor on each request in a configured
header, and the daemon credits that actor instead of any client-supplied value.
Alongside identity tokens (where the daemon is the auth boundary for direct
clients holding personal tokens) and the web-frontend pattern above, this gives
you a third attribution pattern where the proxy is the auth boundary and the
daemon owns only audit attribution.

This mode is off by default and changes nothing until you configure it. To
enable it, name the header and the listeners where the daemon honors it:

```toml
[auth.proxy]
trusted_actor_header = "X-Kata-Actor"
trusted_proxy_listeners = ["unix:///run/kata/proxy.sock"]
```

You can override these settings with `KATA_TRUSTED_ACTOR_HEADER` and a
comma-separated `KATA_TRUSTED_PROXY_LISTENERS`. An empty or unset environment
variable means "no override", so an empty variable cannot silently turn off a
mode enabled in `config.toml`; to disable it, edit the config file.

After merging environment variables and config:

- **Header empty or absent.** Mode is off, and the daemon ignores the header
  everywhere.
- **Header set with `trusted_proxy_listeners` empty.** The daemon **rejects the
  config at load**. A silent no-op would let you believe proxy attribution is on
  while body-supplied actors keep flowing.
- **Header and at least one listener set.** Mode is on for those listeners,
  while every other listener passes through unchanged.

### Listener addresses

Listener entries must be **literal** bind addresses that match the daemon's
`listen` value, either a Unix socket path (`unix:///run/kata/proxy.sock`) or a
specific `host:port` (`100.64.0.5:7777`). Wildcard binds (`0.0.0.0:7777`,
`:7777`) are never valid entries. An accepted connection reports the specific
interface it arrived on, so a wildcard would never match. Use a Unix socket or a
private IP that only the proxy can reach.

### Security model

The header is trustworthy only because of where the daemon honors it, so your
deployment must hold up its end:

- **Trust is bound to the listed listeners.** A client on any other path cannot
  set the header to spoof an actor. The header is meaningful because nothing but
  the proxy can reach that listener.
- **Strip any client-supplied copy at the proxy.** Have your proxy remove any
  client-supplied copy of the header before forwarding and send a single value.
  The daemon reads only the first value of the configured header and does not
  police duplicates; sanitizing inbound headers at the proxy boundary is your
  responsibility.
- **Terminate each attribution mode on its own listener.** On a trusted
  listener, the proxy-asserted actor overwrites a token identity, so running
  direct token-holding clients and the proxy on the same listener discards their
  token identity. Give the proxy a Unix socket and direct clients a separate TCP
  port.
- **A proxy front-end cannot mint or revoke tokens.** Token-admin endpoints stay
  restricted to the bootstrap or loopback path regardless of the header. The
  proxy asserts identity, never token administration.

### Behavior

On a trusted listener, the header value becomes the actor and the daemon ignores
any body or query actor. If a mutation arrives on a trusted listener with the
header missing or empty, the daemon rejects it with `400 actor_header_required`
instead of falling back to a client-supplied actor; otherwise, a non-proxied
client could omit the header and claim any identity. Reads carry no actor, never
reach actor resolution, and are never blocked by this mode. The header asserts
**identity, not permission**, and does not add per-operation or per-actor
authorization.

## Read-only private-network experiments

To run an unauthenticated read-only experiment on a private network:

```sh
kata daemon start --foreground --listen 100.64.0.5:7777 --insecure-readonly
```

This permits GET requests only; mutations and the event stream still require
authentication. In this development mode, network ACLs, VPNs, or tailnet policy
are your access boundary.

Clients of a plaintext private-network daemon must opt into the same trust as
the server, even when no token is configured:

```sh
export KATA_SERVER=http://100.64.0.5:7777
export KATA_TRUST_PRIVATE_NETWORK=1
```

Kata validates the bearer target whenever it builds a client, so it refuses a
plaintext non-loopback endpoint up front rather than failing later as an opaque
error. The opt-in states that the private network is the access boundary;
without it, use HTTPS, a loopback address, or an SSH tunnel.

You cannot combine `require_token_identity = true` with `--insecure-readonly`.

## What this mode does not provide

Kata provides identity tokens and fixed issue-subtree grants, not per-actor
roles, project ACLs, impersonation scopes, or an OAuth provider. A reverse proxy
can own user authentication and assert the actor (see
[Trusted-proxy actor header](#trusted-proxy-actor-header)). Repository, machine,
and network isolation remain your responsibility.
