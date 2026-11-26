---
title: Hosted mode
description: Run the Kata daemon on hosted platforms that provide a Heroku-style PORT environment variable.
last_edited: 2026-11-20
---

# Hosted mode

Hosted mode follows the `$PORT` convention used by Cloud Run, Render, Fly.io,
Railway, App Engine, and other Heroku-style runtimes.

When `--listen`, `KATA_LISTEN`, and config `listen` are all unset,
`kata daemon start --foreground` binds `0.0.0.0:$PORT` if `PORT` is in the
environment. Plain `kata daemon start` is for local operator use; it starts a background
daemon and returns once startup is confirmed.

Auto-started local child daemons set `KATA_AUTOSTART=1`, so a stray `PORT` on a
developer machine does not push implicit local daemons onto wildcard TCP.

## Required environment

Point `KATA_HOME` at a writable directory:

```sh
KATA_HOME=/writable/path
```

When Kata binds a non-loopback plaintext listener, also set
`KATA_TRUST_PRIVATE_NETWORK=1`.

If you start a non-loopback service explicitly with private-network trust and no
configured credential, Kata mints a token once at `$KATA_HOME/auth-token` and
reuses it on restart. The file is owned by the daemon user with Unix mode `0600`
(on Windows, its ACL allows only the current user, SYSTEM, and Administrators),
and the startup log shows its path without revealing its contents. Preserve this
file with your data volume and use its contents for browser token login or
authorized clients. A nonempty `KATA_AUTH_TOKEN` still overrides all configured
file sources. To mount your own secret instead, set `KATA_AUTH_TOKEN_FILE`; see
[credential precedence](../reference/configuration.md#daemon-config).

Even when Kata listens only on local endpoints behind a TLS terminator, a
non-loopback HTTPS `KATA_WEB_PUBLIC_ORIGIN` still requires browser
authentication. Kata reuses an eligible persisted token or creates one for token
login without requiring private-network trust. A non-loopback plaintext Kata
listener still requires private-network trust, even when the public origin uses
HTTPS.

To suppress automatic token creation, set `[auth].auto_token = false` or pass
`--no-auto-token`; Kata still reuses an eligible existing token. Both implicit
creation and reuse are disabled in read-only, tokenless private-network,
identity, and auto-start modes. Proxy authentication skips token creation only
when an actor header is configured and the browser listener is in
`trusted_proxy_listeners`. A local-only daemon with no public web origin does
not need an implicit token, while an external HTTPS browser origin with no other
configured auth method must have a reusable token or allow token creation, or
startup fails. On hosted platforms that route traffic to a non-loopback
plaintext Kata listener, you must trust the container network path.

## Bind an interface

To bind a specific network interface, set `KATA_LISTEN=iface:overlay0:7777`, or
use the same value with `--listen` or top-level `listen`. Before startup, Kata
resolves the named, active interface to exactly one eligible non-public IPv4
address. If the interface is missing or down, has no IPv4 address, or has
multiple eligible addresses, startup fails without a fallback listener. If the
interface address changes, restart the daemon. Run `kata daemon status --json`
to inspect the concrete address and winning config sources without exposing
tokens.

## Browser origin and proxying

When your platform terminates TLS, set the exact external HTTPS origin:

```sh
KATA_WEB_PUBLIC_ORIGIN=https://daemon.example
```

Treat this origin as a security input, not display metadata. Kata validates
`Host` and mutation `Origin` against it and never trusts forwarded headers to
discover it. Route the SPA, hashed assets, browser-session routes, data API, and
`/api/v1/events/stream` to the same daemon, and disable response buffering for
the event stream so authenticated invalidations flush immediately.

If you open a hosted deep link without browser authority, Kata shows token login
and returns you to that path after exchange. The browser retains only its paired
cookie and session-header credentials along with its CSRF value; a fresh tab
needs its own login because cookie-only recovery is intentionally unavailable.

Point `KATA_HOME` at writable storage. Local container disks are
often ephemeral, so data does not survive instance recycling unless `KATA_HOME`
is backed by a mounted volume or shared storage.

## Health probes

Use either endpoint for liveness or readiness checks:

```text
GET /api/v1/health
GET /api/v1/ping
```

Both endpoints are unauthenticated, though probes must still send an accepted
`Host`: the exact public authority, the concrete backend authority, or a
configured `KATA_WEB_ALLOWED_HOSTS` alias. Forwarded headers do not establish
authority.

## Shutdown

The daemon handles `SIGTERM` gracefully. HTTP handlers get up to 10 seconds to
drain, within a shared 25-second budget for handlers, background workers, and
hooks before the process exits.

## Single-instance assumption

Kata assumes one daemon per database. Deploying multiple hosted instances
without shared storage gives each instance its own state, and deploying multiple
instances against one SQLite database is not a supported high-availability
setup.

For team workflows that need one central state store, run one daemon against one
database and put platform routing in front of that process.
