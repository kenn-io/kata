---
title: Unix socket activation
description: Run Kata behind a service-manager-owned Unix socket without detached replacement daemons.
last_edited: 2026-11-11
---

# Unix socket activation

Not in the latest release yet.

A systemd user socket can remain available while Kata is stopped. Connecting to
that socket starts the foreground daemon, which adopts the manager's listener
instead of binding or unlinking its path. Explicit Unix client selection is
independently useful with an already-running native Unix daemon.

## Supported listener and ownership rules

Set `socket_activation = "systemd"` in the service's `<KATA_HOME>/config.toml`.
The empty or absent setting keeps ordinary daemon behavior. Activation requires
exactly one pathname Unix stream listening socket, a `0700` immediate parent
directory owned by the daemon user, and a socket owned by that user. TCP,
datagrams, accepted connections, abstract sockets and multiple descriptors are
not supported.

Run `kata daemon start --foreground` directly as the service process. Do not
supply `--listen`, a nonempty `KATA_LISTEN`, config `listen`, or an eligible
hosted `PORT`; these conflict with activation. Do not put a forking wrapper
between the manager and Kata: systemd's descriptor protocol checks the process
PID. The binary does not require CGO or libsystemd.

The actual inherited address is published only when Kata is ready. On shutdown,
Kata closes its descriptor and removes its runtime record, but leaves the
manager's socket intact. The browser listener is still a separate Kata-owned
loopback TCP listener by default; adopting a Unix API socket does not eliminate
that listener.

## Configure a systemd user service

The examples use `~/.local/bin/kata`; replace that executable path with the one
managed by your package or service configuration. Keep the state directory and
token outside an immutable package store. Provision the private token file
before the first connection; do not embed its value in unit files or workspace
configuration.

Create `~/.local/share/kata-managed` with mode `0700`, then place these settings
in its `config.toml`:

```toml
socket_activation = "systemd"

[auth]
token_file = "~/.local/share/kata-managed/token"
```

Create `token` as a private regular file owned by the daemon user, with mode
`0600`, containing a generated bearer value. The normal [credential file
rules](../reference/configuration.md#credentials-and-batching) apply. The default
SQLite database stays in this Kata home; a different backend must be explicitly
configured in the same home.

`~/.config/systemd/user/kata-managed.socket`:

```ini
[Unit]
Description=Kata managed Unix socket

[Socket]
ListenStream=%t/kata-managed/api.sock
SocketMode=0600
DirectoryMode=0700
Accept=no
Service=kata-managed.service

[Install]
WantedBy=sockets.target
```

`~/.config/systemd/user/kata-managed.service`:

```ini
[Unit]
Description=Kata activated daemon
Requires=kata-managed.socket
After=kata-managed.socket

[Service]
Type=simple
Environment=KATA_HOME=%h/.local/share/kata-managed
UnsetEnvironment=KATA_SERVER KATA_DSN KATA_DB KATA_LISTEN PORT KATA_AUTH_TOKEN KATA_AUTH_TOKEN_FILE
ExecStart=%h/.local/bin/kata daemon start --foreground
```

Load the socket, then point the client at it:

```sh
systemctl --user daemon-reload
systemctl --user enable --now kata-managed.socket
export KATA_SERVER="unix://${XDG_RUNTIME_DIR}/kata-managed/api.sock"
export KATA_AUTH_TOKEN_FILE="$HOME/.local/share/kata-managed/token"
kata health --json
```

The first health probe can activate the service. `kata daemon locate` also
connects to probe the selected endpoint and can therefore activate it. Neither
command starts a detached process itself.

`KATA_SERVER`, a workspace `server.url`, or a named catalog `url` can select the
socket without an existing runtime record. See [Daemon discovery](../reference/daemon-discovery.md)
for precedence and source-specific credentials. Explicit socket targets never
fall back to another home or database. They retain conservative filesystem
semantics and are not interchangeable with a pinned local-home profile.

## Restart, stop and upgrade

The durable setting makes process ownership visible even while Kata is stopped.
`kata daemon start` without foreground, `stop`, `restart`, implicit startup and
profile recovery cannot take over that home. A stale binary is not permission
to stop and replace its managed daemon. Runtime metadata also protects an
already-running managed process if the configuration is subsequently edited.

Use the manager for lifecycle changes:

```sh
systemctl --user restart kata-managed.service
systemctl --user stop kata-managed.socket kata-managed.service
```

Stopping only the service leaves its socket active; the next connection may
start the service again. Stop both units when the daemon must remain unavailable.
Kata never calls socket-level `shutdown` on the manager's shared listener.

A binary update can install the new executable but requires a manager restart
to apply it. Commands that promise to save configuration and restart refuse
before writing when they cannot perform the managed restart. Direct operator
configuration edits take effect after a manager restart. The ordinary,
non-activated daemon restart behavior is unchanged.

Authentication policy is unchanged. A preprovisioned token file is sufficient
for authenticated cold activation; no new bootstrap command or database
migration is required. Use `kata ui` with the daemon's separate HTTP(S) browser
origin, not the Unix API selector.
