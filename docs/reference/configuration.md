# Configuration

kata configuration is split between environment variables, committed workspace
bindings, local per-machine overrides, and daemon config.

## Environment variables

| Variable | Meaning |
| --- | --- |
| `KATA_HOME` | Data directory. Defaults to `~/.kata`. |
| `KATA_DB` | Explicit SQLite database path. |
| `KATA_AUTHOR` | Default actor for mutations. |
| `KATA_SERVER` | Remote daemon URL. Skips local discovery and auto-start. |
| `KATA_AUTH_TOKEN` | Bearer token for daemon API auth. |
| `KATA_TRUST_PRIVATE_NETWORK` | Set to `1` to permit trusted plaintext bearer use on private non-loopback HTTP. |
| `KATA_HTTP_TIMEOUT` | Per-request CLI timeout for non-streaming daemon calls, such as `30s` or `2m`. Defaults to `5s`; raise it for bulk imports. |
| `KATA_FEDERATION_PULL_INTERVAL_MS` | Federation runner poll interval for tests or latency-sensitive private deployments. |
| `PORT` | Hosted-mode listener port when no explicit listener is configured and the daemon is not an auto-start child. |
| `XDG_RUNTIME_DIR` | Runtime socket parent on Unix when applicable. |

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

## Daemon config

`<KATA_HOME>/config.toml` can configure listener and auth behavior:

```toml
listen = "100.64.0.5:7777"

[auth]
token = "change-me"
trust_private_network = true
```

The `kata daemon start --listen <host:port>` flag wins over the config file.
Auto-started daemons also read the config-file listener value.

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

- sibling-burst: closing more than three sibling issues within five minutes is
  refused;
- repeated-message: closing a second sibling with an identical `done` or
  `audit-no-change` message within thirty minutes is refused.

Closing each issue as soon as its work is verified, rather than batching closes
at the end of a run, keeps an actor under both limits.

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
