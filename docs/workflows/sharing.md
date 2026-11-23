# Sharing models

Kata offers three ways to share project state. Pick the smallest setup that fits
how you work.

## Local shared project

Use a local shared project when one person or one machine has multiple
workspaces, worktrees, or agents. Each workspace commits the same project name
in `.kata.toml` and shares the local `KATA_HOME` database. The daemon stays
local and requires no network configuration.

```sh
kata init --project product
```

This is the default setup for coordinating agents on a single workstation.

## Private-network remote daemon

Use a remote daemon when several trusted hosts need to share one daemon and one
database. The daemon runs on the host that owns the SQLite database and listens
on a private IP address, while clients connect by setting `KATA_SERVER` or
`.kata.local.toml`.

This gives everyone a single central copy of project state, assuming a trusted
private network and a deliberately small auth model. Before you expose any TCP
listener, read [Remote daemon](../operations/remote-daemon.md).

## Federation

Choose federation when each participant keeps a local daemon and database, and
you want selected projects to sync through a hub.

You opt in per project, trading immediate single-copy reads for local
availability and offline queues. Because reads come from your local copy,
updates can lag while a connection is offline. Leases offer optional
coordination, and both sync and recovery have explicit limits.

To share selected projects through a personal or intermediate hub, follow the
[project relay guide](../guide/project-relays.md), which covers connecting,
downstream devices, and recovery. If you need direct-spoke enrollment with
daemon-owner credentials, see the [operator reference](../operations/federation.md).
In identity mode, the owner uses the bootstrap token and passes an existing hub
account with `--actor`.

## Hosted mode

Hosted mode is a deployment convention for PaaS platforms that inject `PORT`.
You still run one daemon per database. Hosted mode fits controlled deployments
where the platform handles TLS termination and process management.

Before deploying to Cloud Run, Render, Fly.io, Railway, App Engine, or similar
platforms, read [Hosted mode](../operations/hosted-mode.md).

## Choose a model

| Need | Use |
| --- | --- |
| One developer, several worktrees or agents | Local shared project |
| Trusted team needs immediate central reads/writes | Remote daemon |
| Multiple local-first replicas with offline tolerance | Federation |
| Receive selected projects and relay them to other devices | [Project relays](../guide/project-relays.md) |
| PaaS process management with `$PORT` | Hosted mode |
| Public multi-tenant SaaS authorization | Not implemented yet |

Do not expose your local daemon casually on a LAN or public interface. For any
remote setup, configure the listener explicitly and use bearer tokens along with
trusted private-network settings.
