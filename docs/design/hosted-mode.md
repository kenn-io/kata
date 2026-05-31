# Kata hosted mode (`$PORT` convention)

When no `--listen` flag and no config `listen` are set, the daemon binds
`0.0.0.0:$PORT` if `PORT` is in the environment. This is the contract
Heroku originated and that Cloud Run, Render, Fly.io, Railway, App
Engine, and other PaaS runtimes all follow: the platform picks a port,
injects it via `PORT`, and expects the process to bind every interface
at `0.0.0.0:$PORT`.

The auto-start child sets `KATA_AUTOSTART=1` so a stray `PORT` on a
developer's machine does not flip implicit local daemons onto wildcard
TCP.

## Required environment

- `KATA_AUTH_TOKEN=<token>` plus `KATA_TRUST_PRIVATE_NETWORK=1`. Without
  both, the daemon refuses the non-loopback bind — the platform
  terminates TLS upstream, so the operator must explicitly assert trust
  in the container's network path.
- `KATA_HOME` must point at a writable path (e.g. `/tmp/kata`); the
  daemon writes runtime files and the SQLite DB under it. Local
  container disk is ephemeral, so data does not survive instance
  recycling unless `KATA_HOME` is backed by a mounted volume or shared
  storage.

## Health probes

`GET /api/v1/health` and `GET /api/v1/ping` are unauthenticated and
suitable for platform liveness / readiness probes.

## Shutdown

The daemon handles SIGTERM gracefully (up to 10s for in-flight
requests).

## Single-instance assumption

kata assumes one daemon per DB; deploying multiple hosted instances
without shared storage gives each its own state.
