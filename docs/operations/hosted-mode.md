# Hosted mode

Hosted mode is the `$PORT` convention used by platforms such as Cloud Run,
Render, Fly.io, Railway, App Engine, and Heroku-style runtimes.

When no `--listen` flag and no config `listen` are set, `kata daemon start
--foreground` binds `0.0.0.0:$PORT` if `PORT` is in the environment. Plain
`kata daemon start` is for local operator use: it starts a background daemon and
returns after startup is confirmed.

Auto-started local child daemons set `KATA_AUTOSTART=1`, so a stray `PORT` on a
developer machine does not flip implicit local daemons onto wildcard TCP.

## Required environment

Set:

```sh
KATA_AUTH_TOKEN=<token>
KATA_TRUST_PRIVATE_NETWORK=1
KATA_HOME=/writable/path
```

Without both `KATA_AUTH_TOKEN` and `KATA_TRUST_PRIVATE_NETWORK=1`, the daemon
refuses the non-loopback bind. Hosted platforms commonly terminate TLS
upstream, so the operator must explicitly assert trust in the container network
path.

## Browser origin and proxying

Set the exact external HTTPS origin when the platform terminates TLS:

```toml
[web]
public_origin = "https://daemon.example"
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

Both are unauthenticated.

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
