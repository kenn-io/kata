---
title: Share projects between hubs
description: Connect a selected project to another Kata daemon, relay it to your devices, and understand sync and recovery.
last_edited: 2026-11-21
---

# Share projects between hubs

A project relay gives you a local copy of a shared project and can pass that
project on to your other devices. Each daemon keeps its own database. You can
read and edit locally while disconnected, and changes synchronize once the
daemons can reach each other again.

Project relays require Kata 0.19.0 or later on every participating daemon. The
connection preview checks whether the upstream daemon supports the protocol.

## Choose the shape you need

If you only need a local replica, connect directly to a root hub. If several
devices need to reach the project through your own hub, add an intermediate
relay:

```text
Root hub: shared-project
          ↕
Personal hub: shared-replica
              --serve-downstream
          ↕
Device: work-project (leaf)
```

The root owns the project's authoritative history, signing key, and lease
decisions. The personal hub is a spoke of the root and a hub for the device. A
leaf has an upstream connection and does not serve the project onward. These roles apply per project. The same daemon can root one project, relay
another, and keep others private.

Each path supports up to eight hubs, including the root, plus one final leaf.

The local project names in the diagram can differ, while all three copies keep
the same project UID. Only the selected project travels over each connection.
Workspace paths and aliases stay local, and a relay does not grant access to
the hub's other projects.

If every client needs to read and write one central copy, use a
[remote daemon](../operations/remote-daemon.md) instead. See
[Sharing models](../workflows/sharing.md) for the other options.

## Before connecting

Make sure you have the following before you start:

- **An existing, federation-enabled project on the upstream hub.** The hub's
  operator can enable it with `kata federation enable --project shared-project`,
  using an ordinary account with project access in identity mode.
- **A writable ordinary account token from that hub.** Issue it with
  `kata tokens create` for an account that has access to the project. Expiring
  account tokens work while valid, and expiry suspends sync until you replace
  the credential. A daemon bootstrap token, an issue-scoped token, or an
  existing enrollment token cannot take its place. See
  [Identity tokens](../operations/remote-daemon.md#identity-tokens) and
  [Team visibility](../operations/federation.md#team-visibility).
- **Owner access to the daemon receiving the replica.** Bridge administration
  uses the owner CLI or API; an ordinary browser session cannot connect a
  bridge.
- **An unused local project name.** Connecting creates the replica. Do not run
  `kata init` with that name first. An unrelated project with the same name is
  a collision, even if its issues look identical.

Before you choose a relay for a large, long-lived project, check
[checkpoint capacity](../operations/federation.md#relay-checkpoint-capacity).
Recovery currently requires a checkpoint that fits in 64 MiB, and retained
receipts impose a growing floor on its size. Normal synchronization does not
enforce that size limit. Acknowledged delivery records also remain until the
binding ends, so disk usage grows with traffic and the number of downstream
replicas.

## Connect the receiving daemon

Run these steps on the daemon that holds the new replica. In this example, that
daemon is the personal hub, and its upstream root is named `team-hub` in its
catalog.

`--daemon` selects the daemon you are administering, and `--hub-daemon` selects
an upstream entry in **that daemon's** catalog. Keep those two targets
distinct, especially when you administer a remote daemon.

### 1. Configure the upstream account

Add this entry to the receiving daemon's `<KATA_HOME>/config.toml`:

```toml
[[daemon]]
name = "team-hub"
url = "https://hub.example"
token_file = "~/.config/kata/tokens/team-hub"
```

On a device, store the account token in that file, owned by the user running
the receiving daemon, and run `chmod 600` to make it owner-only. The
**daemon process** must be able to read the file. Restart the daemon to load
the catalog; Kata reads the token file when you connect. After you connect,
sync uses the saved project-scoped credential.

For containers and service managers that already inject secrets, use
`token_env = "KATA_TEAM_HUB_TOKEN"` instead. Supply the variable to the
receiving daemon's service environment. Exporting it only in a CLI shell does
not update a daemon that is already running. Keep the actual token out of
repository files and agent transcripts. Catalog credentials follow the
precedence `token` > `token_file` > `token_env`, and if the selected source
fails, the connection stops without falling back.

Use a root HTTP(S) origin with no path prefix. Non-loopback plain HTTP needs
the catalog's explicit `allow_insecure` opt-in for a trusted private network,
and Kata rejects redirects. See
[Daemon configuration](../reference/configuration.md#daemon-config).

### 2. Preview the connection

```sh
kata federation bridge connect --project shared-replica \
  --hub-daemon team-hub --hub-project shared-project \
  --serve-downstream --as local-member --preflight
```

Check the upstream project and account, the local account, and the
`bidirectional` direction. Preview contacts the hub without enrolling or
creating the replica. If this daemon is a leaf, omit `--serve-downstream`.
`--as` records the local account and does not change the account authenticated
by the upstream token.

If the hub requires request signing, include the signing-source flags in both
preview and connect. After connecting, register the key for the new enrollment
on the hub. Follow [Bridge request signing](../operations/federation-signing.md#bridges)
for the flags and registration order.

### 3. Connect with the same choices

```sh
kata federation bridge connect --project shared-replica \
  --hub-daemon team-hub --hub-project shared-project \
  --serve-downstream --as local-member
```

The daemon saves a separate, project-scoped transport credential and starts
bidirectional sync. It keeps the upstream account token on this daemon. Neither
the CLI response nor a downstream device receives it. If the response is lost,
repeat the same command to resume the retained enrollment.

### 4. Check progress

```sh
kata federation bridge status --project shared-replica --json
kata federation status --json
```

Bridge status reads locally recorded state; `connected` is not a fresh hub
health check. Inspect sync timestamps and pending work before treating the
replicas as caught up. Use `--agent` for concise agent output, or `--json` when
a script needs the full diagnostics. The web project's sync summary refreshes
its recorded state every 30 seconds while the tab is visible and when you return.
To read it now, open the project's sync chip in the project header and use
**Refresh** in its panel.

To bind a workspace to the replica after connecting, run
`kata init --project shared-replica` in that workspace. In identity mode, use
an ordinary account with project access for this command and for issue work;
the bootstrap credential used for administration cannot perform attributed
writes.

## Add a device below the relay

On the personal hub, configure
[identity tokens](../operations/remote-daemon.md#identity-tokens) and issue an
ordinary account token with access to `shared-replica`. On the device, add a
catalog entry named `personal-hub` with that token and the personal hub's URL.
Then repeat the preview and connect steps with the updated selectors:

```sh
kata federation bridge connect --project work-project \
  --hub-daemon personal-hub --hub-project shared-replica \
  --as device-member --preflight
kata federation bridge connect --project work-project \
  --hub-daemon personal-hub --hub-project shared-replica \
  --as device-member
```

Because these commands omit `--serve-downstream`, the device joins as a leaf.
It needs an account on the personal hub, not the root's account token. To add
another relay below the personal hub instead, use an upstream token that
belongs to the local account that connected the personal hub's bridge. Other
authorized members can enroll leaves.

## What sync means for your work

Edits travel upstream, and accepted changes travel downstream. When a nearby
relay acknowledges an edit, that hop has stored it. The root's signed receipt
confirms root acceptance, and pending push counts wait for that receipt.

Issue and comment creation show one of three states:

| State | Meaning |
| --- | --- |
| `pending` | Created locally; still waiting for root proof. |
| `verified` | The root receipt certifies the accountable actor. |
| `legacy` | Historical content without creation proof. |

These states describe attribution, not review or approval of the work. Source
actor and teammate labels stay visible while proof is pending. For work
forwarded through the personal hub, the root records that hub's upstream
account as accountable, and the device's source labels remain separate.

Ordinary edits are local-first. [Leases](../operations/federation.md#leases-and-write-gates)
offer optional coordination and are not a prerequisite for every edit. Reads
can be stale, and concurrent changes have
[consistency limits](../operations/federation.md#consistency-limitations).
Cross-project issue moves are unsupported when either project is connected
through a relay, including a hub with active downstream relays. The root can
link issues across projects that share a relay group, root authority, and
audience, and relays retain the link until both issues arrive. If a relay
reports `409 unsupported_event_features`, upgrade that peer and retry. The
event stream pauses at the crossing until the peer is upgraded.

Embeddings are optional. A project can select one producer and share compatible
vectors so each device does not need to generate them again. See
[Sharing embedding vectors](../operations/federation.md#sharing-embedding-vectors)
for configuration, waiting states, and reuse limits. Lexical search needs no
embedding provider.

## Recover or disconnect

A bridge status of `revoked` means the hub refused its credential, and it can
clear when access returns. Explicit enrollment revocation on the hub, such as
`kata federation revoke <id>`, is permanent; the status label alone does not
establish that it happened.

| Situation | Next step |
| --- | --- |
| Network or hub temporarily unavailable | Keep the bridge and its queued work. Sync retries when connectivity returns. |
| Upstream account token expired or was revoked | Update the catalog credential, restart the receiving daemon to load changed config or environment, and repeat connect for the same upstream account. This retains queued work. |
| Team membership was removed | Have the upstream operator restore access. The existing connection rechecks authority before resuming. |
| `signing_rejected` | Repair the signing source or hub key registration. Reconnecting is not the remedy. |
| Local project-name or identity collision | Resolve the existing project or choose an unused name; do not initialize another copy to bypass the conflict. |
| Root purge or history reset | Let normal sync install the signed checkpoint. See [reset recovery](../operations/federation.md#reset-recovery) for retained-work blockers. |
| Hub is permanently gone | Review [local-only disconnect](../operations/federation.md#detach-a-bridge-whose-hub-is-gone). It discards undelivered transport work and cannot revoke the remote grant. |

For a normal disconnect, finish pending deliveries and disconnect or revoke
downstream enrollments first. Preview the disconnect, then detach:

```sh
kata federation bridge disconnect --project shared-replica --preflight
kata federation bridge disconnect --project shared-replica
```

Disconnecting revokes the saved upstream enrollment and keeps the local project
and its issues. If the request is interrupted, retry the same command. Do not
use `--local-only` as a routine sync repair.

For full credential, retention, reset, and signing rules, see the
[operator reference](../operations/federation.md). For the protocol and its
trust boundaries, see the [technical notes](../design/federation.md).
