# Sharing models

kata has three sharing shapes. Pick the smallest one that fits the workflow.

## Local shared project

Use this when one person or one machine has multiple workspaces, worktrees, or
agents.

All workspaces commit the same `.kata.toml` project name and use the same local
`KATA_HOME` database. The daemon remains local, and no network configuration is
needed.

```sh
kata init --project product
```

This is the default shape for agent coordination on one workstation.

## Private-network remote daemon

Reach for this when several trusted hosts should share one daemon and one
database.

The daemon runs on the host that owns the SQLite database and listens on a
private IP address. Clients set `KATA_SERVER` or `.kata.local.toml`.

This model gives users one central copy of project state. It still assumes a
trusted private network and a deliberately small auth model.

Read [Remote daemon](../operations/remote-daemon.md) before exposing any TCP
listener.

## Federation

Choose federation when each participant keeps a local daemon and database, but
selected projects should converge through a hub.

Federation is opt-in per project. It favors local availability and offline
queues over immediate single-copy reads. Operators must understand stale reads,
lease behavior, quarantine, reset, and the difference between daemon API tokens
and federation enrollment tokens.

Read [Federation](../operations/federation.md) before enabling it.

## Hosted mode

Hosted mode is a deployment convention for PaaS platforms that inject `PORT`.
It is still one daemon per database. It is useful for controlled deployments
where the platform terminates TLS and provides process management.

Read [Hosted mode](../operations/hosted-mode.md) before deploying to Cloud Run,
Render, Fly.io, Railway, App Engine, or similar platforms.

## Choosing a model

| Need | Use |
| --- | --- |
| One developer, several worktrees or agents | Local shared project |
| Trusted team needs immediate central reads/writes | Remote daemon |
| Multiple local-first replicas with offline tolerance | Federation |
| PaaS process management with `$PORT` | Hosted mode |
| Public multi-tenant SaaS authorization | Not implemented yet |

The local daemon should not be casually exposed to a LAN or public interface.
Use explicit listener configuration, bearer tokens, and trusted private network
settings for any remote setup.
