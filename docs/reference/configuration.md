# Configuration

kata configuration is split between environment variables, committed workspace
bindings, local per-machine overrides, and daemon config.

## Environment variables

| Variable | Meaning |
| --- | --- |
| `KATA_HOME` | Data directory. Defaults to `~/.kata`. |
| `KATA_DSN` | Explicit database DSN. Production storage currently accepts a bare SQLite path or `sqlite://...`. Postgres URLs are recognized but rejected by normal store opening until backend domain methods land. |
| `KATA_DB` | Legacy explicit SQLite database path. Used when `KATA_DSN` is unset. |
| `KATA_AUTHOR` | Default actor for mutations. |
| `KATA_SERVER` | Remote daemon URL. Skips local discovery and auto-start. |
| `KATA_AUTH_TOKEN` | Bearer token for daemon API auth. |
| `KATA_TRUST_PRIVATE_NETWORK` | Set to `1` to permit trusted plaintext bearer use on private non-loopback HTTP. |
| `KATA_ALLOW_INSECURE` | Set to `1` or `true` to allow a configured remote daemon hostname over plain HTTP. Federation uses `kata federation enroll --allow-insecure` and `kata federation join --allow-insecure` instead because enrollment credentials are stored separately. |
| `KATA_TELEMETRY_ENABLED` | Set to `0` to disable anonymous PostHog telemetry. |
| `KATA_HTTP_TIMEOUT` | Per-request CLI timeout for non-streaming daemon calls, such as `30s` or `2m`. Defaults to `5s`; raise it for bulk imports. |
| `KATA_FEDERATION_PULL_INTERVAL_MS` | Federation runner poll interval for tests or latency-sensitive private deployments. |
| `PORT` | Hosted-mode listener port when no explicit listener is configured and the daemon is not an auto-start child. |
| `XDG_RUNTIME_DIR` | Runtime socket parent on Unix when applicable. |

## Database selection

kata resolves its database in this order:

1. `KATA_DSN`
2. `KATA_DB`
3. `[storage].dsn` in `<KATA_HOME>/config.toml`
4. `<KATA_HOME>/kata.db`

Bare paths and `sqlite://` DSNs select SQLite. `postgres://` and
`postgresql://` DSNs are reserved for the incomplete Postgres backend and are
not selectable by normal daemon/CLI store opening yet. `KATA_DB` stays ahead of
`[storage].dsn` so existing shells and scripts keep using their explicit
database path after the config-file key is introduced.

`KATA_DSN` and `[storage].dsn` are shape-validated before use. Unknown schemes
are rejected, and common Postgres-only query parameters on a bare path or
`sqlite://` DSN are treated as likely formatting mistakes. Validation is local:
it does not dial Postgres or stat SQLite paths.

## Workspace binding

`.kata.toml` is committed with the project:

```toml
version = 1

[project]
name = "product"
```

It should stay secret-free.

## Local override

`.kata.local.toml` is gitignored. Use it for machine-specific daemon routing:

```toml
version = 1

[server]
url = "http://100.64.0.5:7777"
```

`KATA_SERVER` wins over the local file.

For trusted private-network hostnames that cannot be represented as literal
non-public IP addresses, opt in per target:

```toml
version = 1

[server]
url = "http://hub.internal:7777"
allow_insecure = true
```

## Daemon config

`<KATA_HOME>/config.toml` can configure storage, listener, and auth behavior:

```toml
listen = "100.64.0.5:7777"

[storage]
dsn = "/var/lib/kata/kata.db"

[auth]
token = "change-me"
trust_private_network = true
```

The `kata daemon start --listen <host:port>` flag wins over the config file.
Auto-started daemons also read the config-file listener value.
An empty `[storage].dsn` means "no storage override"; env vars or the default
database path still apply.

Postgres DSNs may carry credentials. Although they are not selectable yet,
runtime redaction handles both URL and libpq keyword forms defensively; userinfo
and query parameters are stripped before display or hashing.

## Token identity mode

For a shared daemon where each user should have stable attribution:

```toml
[auth]
token = "bootstrap-admin-token"
trust_private_network = true
require_token_identity = true
```

Create per-user tokens before requiring token identity:

```sh
export KATA_AUTH_TOKEN=bootstrap-admin-token
kata tokens create --actor wesm --name laptop
kata tokens list
kata tokens revoke 1
```

`tokens create` prints plaintext once. The daemon stores only a SHA-256 hash.
Lost tokens must be revoked and recreated.

In identity mode, the bootstrap/admin token can manage tokens and perform
reads, but attributed writes require a DB-backed token. The daemon derives the
actor from that token.

## Close throttle

kata refuses structurally dangerous close patterns. The parent-completeness
guard always refuses closing an issue while it has open children. Two further
guards throttle close bursts by one actor under a shared parent:

- sibling-burst: closing more than three sibling issues within 60 seconds is
  refused;
- repeated-message: closing a second sibling with an identical `done` or
  `audit-no-change` message within thirty minutes is refused.

Close each issue as soon as its work is verified. Batching closes at the end of
a run is the pattern this guard is meant to catch, and it can push one actor
over the sibling-burst limit even when each individual issue is legitimate.

Operators can disable both throttles daemon-wide:

```toml
[close.throttle]
enabled = false
```

`enabled = false` relaxes only the two sibling throttles. Normal CLI and API
close paths still run the parent-completeness refusal, message-substance checks,
and evidence checks. The TUI close path skips the message-substance and evidence
checks because an interactive human confirms each close; the structural guards
still apply.

## Telemetry

kata sends limited anonymous telemetry to PostHog when the daemon starts, and
then emits an in-process `daemon_active` heartbeat once per UTC day while the
daemon keeps running. Restarting the daemon may send another heartbeat; kata
does not store heartbeat state in the database.

The events are `daemon_started` and `daemon_active` with `project_count`,
`application=kata`, build version, commit, OS/arch, source, and the database's
stable anonymous `instance_uid` as the distinct ID. They do not send project
names, issue refs, issue content, comments, labels, paths, or actor names. GeoIP
collection is disabled and PostHog person-profile processing is explicitly
turned off. Use distinct `daemon_active` counts for active-install reporting;
`daemon_started` is only for startup-volume diagnostics.

Disable telemetry with:

```sh
export KATA_TELEMETRY_ENABLED=0
```

## Federation credentials

Federation enrollment tokens are separate from daemon API tokens. The hub
stores only token hashes. A spoke stores the plaintext enrollment token in its
local federation credentials file so it can call hub federation transport
routes.

Do not put federation enrollment tokens in `.kata.toml`.

## Hosted mode

When `PORT` is set and no explicit listener is configured, a foreground daemon
binds `0.0.0.0:$PORT`. Hosted mode still requires daemon API auth and explicit
private-network trust. See [Hosted mode](../operations/hosted-mode.md).
