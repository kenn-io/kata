---
title: Hosted mode
description: Run the Kata daemon on hosted platforms that provide a Heroku-style PORT environment variable.
last_edited: 2026-10-02
---

# Hosted mode

Hosted mode is the `$PORT` convention used by platforms such as Cloud Run,
Render, Fly.io, Railway, App Engine, and Heroku-style runtimes.

When no `--listen` flag, `KATA_LISTEN`, or config `listen` are set, `kata daemon start
--foreground` binds `0.0.0.0:$PORT` if `PORT` is in the environment. Plain
`kata daemon start` is for local operator use: it starts a background daemon and
returns after startup is confirmed.

Auto-started local child daemons set `KATA_AUTOSTART=1`, so a stray `PORT` on a
developer machine does not flip implicit local daemons onto wildcard TCP.

## Required environment

Set:

```sh
KATA_HOME=/writable/path
```

Set `KATA_TRUST_PRIVATE_NETWORK=1` when Kata binds a non-loopback plaintext
listener.

For an explicit non-loopback service start with private-network trust and no
configured credential, Kata mints a token once at `$KATA_HOME/auth-token` and
reuses it on restart. The file is owned by the daemon user with Unix mode
`0600`. On Windows, its ACL allows only the current user, SYSTEM, and
Administrators. The startup log shows its path, never its contents. Preserve
this file with the data volume. Use its contents for browser token login or
authorized clients.
A nonempty `KATA_AUTH_TOKEN` still overrides all configured file sources.
To mount your own secret, set `KATA_AUTH_TOKEN_FILE`; see
[credential precedence](../reference/configuration.md#daemon-config).

When Kata listens only on local endpoints behind a TLS terminator, a
non-loopback HTTPS `KATA_WEB_PUBLIC_ORIGIN` also requires browser
authentication. Kata reuses an eligible persisted token or creates one for
token login without requiring private-network trust. A non-loopback plaintext
Kata listener still requires private-network trust, even when the public origin
uses HTTPS.

Set `[auth].auto_token = false` or pass `--no-auto-token` to suppress creation;
eligible existing tokens are still reused. Implicit creation and reuse are both
disabled for read-only, tokenless private-network, identity, and auto-start
modes. Proxy authentication skips token creation only when an actor header is
configured and the browser listener is in `trusted_proxy_listeners`. A local-only
daemon with no public web origin does not need an implicit token. An external HTTPS browser origin without another configured
auth method must have a reusable token or allow token creation; otherwise
startup fails. Hosted platforms that route traffic to a non-loopback plaintext
Kata listener require the operator to trust the container network path.

## Bind an interface

Set `KATA_LISTEN=iface:overlay0:7777` or use the same value with `--listen`
or top-level `listen`. Kata resolves the named, up interface to exactly one
eligible non-public IPv4 address before startup. A missing/down interface,
missing IPv4 address, or multiple eligible addresses fails without a fallback
listener. Restart after changing the interface address. `kata daemon status
--json` reports the concrete address and winning config sources without tokens.

## Browser origin and proxying

Set the exact external HTTPS origin when the platform terminates TLS:

```sh
KATA_WEB_PUBLIC_ORIGIN=https://daemon.example
```

The origin is a security input, not display metadata. Kata validates `Host` and
mutation `Origin` against it and never trusts forwarded headers to discover it.
The platform must send the SPA, hashed assets, browser-session routes, data API,
and `/api/v1/events/stream` to the same daemon. Disable response buffering for
the event stream so authenticated invalidations flush immediately.

Opening a hosted deep link without browser authority shows token login and
returns to that path after exchange. The browser retains only its paired
cookie/session-header credentials and CSRF value; a fresh tab needs its own
login because cookie-only recovery is intentionally unavailable.

`KATA_HOME` must point at writable storage. Local container disk is often
ephemeral, so data does not survive instance recycling unless `KATA_HOME` is
backed by a mounted volume or shared storage.

## Health probes

Use either endpoint for liveness or readiness:

```text
GET /api/v1/health
GET /api/v1/ping
```

Both are unauthenticated. Probes must still use an accepted `Host`: the exact
public authority, concrete backend authority, or a configured
`KATA_WEB_ALLOWED_HOSTS` alias. Forwarded headers do not establish authority.

## Shutdown

The daemon handles `SIGTERM` gracefully. HTTP handlers receive up to 10 seconds
to drain, within a shared 25-second budget for handlers, background workers, and
hooks before the process exits.

## Single-instance assumption

kata assumes one daemon per database. Deploying multiple hosted instances
without shared storage gives each instance its own state. Deploying multiple
instances against one SQLite database is not a supported high-availability
shape.

For team workflows that need one central state store, run one daemon against
one database and put platform routing in front of that process.
