# Hosted mode

Hosted mode is the `$PORT` convention used by platforms such as Cloud Run,
Render, Fly.io, Railway, App Engine, and Heroku-style runtimes.

When no `--listen` flag and no config `listen` are set, a foreground daemon
binds `0.0.0.0:$PORT` if `PORT` is in the environment.

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

The daemon handles `SIGTERM` gracefully, with up to 10 seconds for in-flight
requests.

## Single-instance assumption

kata assumes one daemon per database. Deploying multiple hosted instances
without shared storage gives each instance its own state. Deploying multiple
instances against one SQLite database is not a supported high-availability
shape.

For team workflows that need one central state store, run one daemon against
one database and put platform routing in front of that process.
