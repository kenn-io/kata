---
title: Share projects between hubs
description: Connect a selected project to another Kata daemon, relay it to your devices, and understand sync and recovery.
last_edited: 2026-10-21
---

# Share projects between hubs

A project relay gives you a local copy of a shared project and can pass that
project on to your other devices. Each daemon keeps its own database. You can
read and edit locally while disconnected; changes synchronize when the daemons
can reach each other again.

**Availability:** project relays require Kata 0.19.0 or later on every
participating daemon.
The connection preview checks the upstream daemon's protocol support.
Publishing an existing local project is newer `main` functionality: the source
needs API `0.32.0`, and the hub must advertise `relay_publication_version: 1`.

## Choose the shape you need

Connect directly to a root hub when you only need a local replica. Add an
intermediate relay when several devices should reach the project through your
own hub:

```text
Root hub: shared-project
          ↕
Personal hub: shared-replica
              --serve-downstream
          ↕
Device: work-project (leaf)
```

The root owns the project's authoritative history, signing key and lease
decisions. The personal hub is a spoke of the root and a hub for the device.
A leaf has an upstream connection and does not serve the project onward.
These are roles for one project: the same daemon can root one project, relay
another and keep others private.

Each path supports up to eight hubs, including the root, plus one final leaf.

The names above may differ, but all three copies retain the same project UID.
Only the selected project travels over each connection. Workspace paths and
aliases stay local. A relay does not grant access to the hub's other projects.

Use a [remote daemon](../operations/remote-daemon.md) when every client needs
to read and write one central copy. See [Sharing models](../workflows/sharing.md)
for the other choices.

## Before connecting

You need:

- For a new replica, an existing, federation-enabled project on the upstream
  hub. Its operator can enable it with
  `kata federation enable --project shared-project`, using an ordinary account
  with project access in identity mode.
- A writable **ordinary account token** from that hub, issued with
  `kata tokens create`. The account must have access to the project. Expiring
  account tokens are accepted while valid; expiry suspends sync until the
  credential is replaced. A daemon bootstrap token, an issue-scoped token or an
  existing enrollment token cannot take its place. See [Identity tokens](../operations/remote-daemon.md#identity-tokens)
  and [Team visibility](../operations/federation.md#team-visibility).
- Owner access to the daemon receiving the replica. Bridge administration uses
  the owner CLI or API; an ordinary browser session cannot connect a bridge.
- For a new replica, an unused local project name. Connect creates the replica;
  do not run `kata init` with that name first. An unrelated project with the same name is
  a collision, even if its issues look identical.

To share a populated standalone local project instead, use
[Publish your existing local project](#publish-your-existing-local-project).

Check the [checkpoint capacity](../operations/federation.md#relay-checkpoint-capacity)
before choosing this for a large, long-lived project. Recovery currently needs
a checkpoint that fits in 64 MiB, and retained receipts impose a growing floor
on its size. Normal synchronization does not enforce that size limit.
Acknowledged delivery records also remain until the binding ends, so disk usage
grows with traffic and the number of downstream replicas.

## Connect the receiving daemon

Run these steps on the daemon that will hold the new replica. In the example,
that is the personal hub. Its upstream root is called `team-hub` in its catalog.

`--daemon` selects the daemon you are administering. `--hub-daemon` selects an
upstream entry in **that daemon's** catalog. Keep those two targets distinct,
especially when administering a remote daemon.

### 1. Configure the upstream account

Add this entry to the receiving daemon's `<KATA_HOME>/config.toml`:

```toml
[[daemon]]
name = "team-hub"
url = "https://hub.example"
token_env = "KATA_TEAM_HUB_TOKEN"
```

Provide `KATA_TEAM_HUB_TOKEN` to the receiving **daemon process** through its
service environment, then restart that daemon to load the catalog. Exporting
the variable only in a CLI shell does not update an already running daemon.
Keep the actual token out of repository files and agent transcripts.

Use a root HTTP(S) origin, with no path prefix. Non-loopback plain HTTP needs
the catalog's explicit `allow_insecure` opt-in for a trusted private network;
redirects are rejected. See [Daemon configuration](../reference/configuration.md#daemon-config).

### 2. Preview the connection

```sh
kata federation bridge connect --project shared-replica \
  --hub-daemon team-hub --hub-project shared-project \
  --serve-downstream --as local-member --preflight
```

Check the upstream project and account, the local account, and the
`bidirectional` direction. Preview contacts the hub but does not enroll or
create the replica. Omit `--serve-downstream` if this daemon will be a leaf.
`--as` records the local account; it does not change the account authenticated
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
bidirectional sync. It keeps the upstream account token on this daemon; neither
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
a script needs the full diagnostics. The web project's **Refresh sync** action
also reads the recorded project state.

To bind a workspace to the replica after connection, run
`kata init --project shared-replica` in that workspace. In identity mode, use an
ordinary account with project access for this command and issue work; the
bootstrap credential used for administration cannot perform attributed writes.

## Publish your existing local project

Use `--adopt-existing` to publish one standalone local project and connect that
same identity to the hub. The hub keeps serving. Original project and issue
UIDs, stored short IDs, comments, authors, teammates, timestamps and retained
event history survive the first relay synchronization.

The source and published hub keep the complete original history. Downstream
replicas receive all original portable events and issue/comment state.
Host-local `project.created`, `project.renamed` and `project.author_rewritten`
events stay on the source and published hub; they do not rename independently
named replicas or rewrite those replicas' local catalogs.
Labels retain their names and issue associations on a new replica. Native
label snapshots recreate label attribution as `federation` with a local
timestamp; original label authors and timestamps remain on the source and hub.

Configure the upstream account as above. It must be an ordinary writable,
unscoped database account with permission to create a project on the hub.
You administer your local daemon; the hub needs no owner-issued direct grant.
The first version requires the same project name locally and on the hub:

```sh
kata federation bridge connect --project source-project \
  --hub-daemon team-hub --hub-project source-project \
  --adopt-existing --serve-downstream --as local-member --preflight
```

Review the account, destination, issue/comment/event counts, visibility and
source-pause warning. Native project creation currently previews visibility
`all`; team membership does not make the published project private. This
workflow accepts no custom visibility restriction. Preflight changes no
project, credential or file.

The source must be active, standalone and self-contained. Issues, comment
replies and edits, labels, intra-project links and soft-deleted issues are
supported. Existing federation, unresolved claims, integrations, automation,
purge/reset boundaries and non-replayable history are refused. Current
[cross-project links](../operations/federation.md#connect-a-project-through-the-daemon-catalog)
remain unsupported. Publication also refuses historical references outside
the project, even when the corresponding live link was removed. The frozen
envelope must fit the 64 MiB limit and the applicable checkpoint capacity.

Run the same command without `--preflight` to publish and connect. Omit
`--serve-downstream` when this source will remain a leaf. Kata pauses source
writes before capturing the transfer; reads and unrelated projects remain
available. It retains the exact transfer bytes across interruption. Writes
resume only after credentials are durable, the initial baseline is applied,
and deliveries, root receipts, claims and quarantine are clear.

```sh
kata federation bridge status --project source-project --json
```

A `pending` connect response or `adoption_pending` bridge state means cutover
is unfinished. `operation_uid`, `phase` and `source_paused` identify the
retained work. Normal sync or an identical connect retry completes it; do not
edit around the pause. See
[publication recovery](../operations/federation.md#publish-a-populated-standalone-project)
for retry and disconnect rules, including credential-write failures.

Original authors and teammates remain source claims. The publishing account
is accountable for publication; preserved names do not prove those people
authenticated to the hub historically.

## Add a device below the relay

On the personal hub, configure [identity tokens](../operations/remote-daemon.md#identity-tokens)
and issue an ordinary account token with access to `shared-replica`. On the
device, configure a catalog entry named `personal-hub` with that token and the
personal hub's URL. Then repeat the preview and connect steps, changing the
selectors:

```sh
kata federation bridge connect --project work-project \
  --hub-daemon personal-hub --hub-project shared-replica \
  --as device-member --preflight
kata federation bridge connect --project work-project \
  --hub-daemon personal-hub --hub-project shared-replica \
  --as device-member
```

The device is a leaf because these commands omit `--serve-downstream`. It needs
an account on the personal hub, not the root's account token. To add another
relay below the personal hub instead, its upstream token must belong to the
local account that connected the personal hub's bridge. Other authorized
members may enroll leaves.

## What sync means for your work

Edits travel upstream and accepted changes travel downstream. A nearby relay
acknowledging an edit means that hop has stored it. The root's signed receipt
confirms root acceptance; pending push counts wait for that receipt.

Issue and comment creation show one of three states:

| State | Meaning |
| --- | --- |
| `pending` | Created locally; still waiting for root proof. |
| `verified` | The root receipt certifies the accountable actor. |
| `legacy` | Historical content without creation proof. |

These states describe attribution, not review or approval of the work.
Source actor and teammate labels remain visible while proof is pending. For
work forwarded through the personal hub, the root records that hub's upstream
account as accountable. The device's source labels remain separate.

Ordinary edits are local-first. [Leases](../operations/federation.md#leases-and-write-gates)
offer optional coordination; they are not a prerequisite for every edit.
Reads can be stale, and concurrent changes have
[consistency limits](../operations/federation.md#consistency-limitations).
Cross-project issue moves are unsupported while either project is federated;
negotiated relay projects also reject cross-project links.

Embeddings are optional. A project can select one producer and share compatible
vectors so each device need not generate them again. See
[Sharing embedding vectors](../operations/federation.md#sharing-embedding-vectors)
for configuration, waiting states and reuse limits. Lexical search needs no
embedding provider.

## Recover or disconnect

A bridge status of `revoked` means the hub refused its credential and can clear
when access returns. Explicit enrollment revocation, such as
`kata federation revoke <id>` on the hub, is permanent; the status label alone
does not establish that it happened.

| Situation | Next step |
| --- | --- |
| Network or hub temporarily unavailable | Keep the bridge and its queued work. Sync retries when connectivity returns. |
| Upstream account token expired or was revoked | Update the catalog credential, restart the receiving daemon to load changed config or environment, and repeat connect for the same upstream account. This retains queued work. |
| Team membership was removed | Have the upstream operator restore access. The existing connection rechecks authority before resuming. |
| `signing_rejected` | Repair the signing source or hub key registration. Reconnecting is not the remedy. |
| Adoption is pending or its publication response was lost | Repeat the identical connect command with `--adopt-existing`, the same project, catalog and account. Keep the database, credentials and frozen artifact. |
| Adoption reports a credential-write failure | Repair local private-file access and repeat the identical connect. Published hub content remains; do not remove the retained candidate credential. |
| Local project-name or identity collision | Resolve the existing project or choose an unused name; do not initialize another copy to bypass the conflict. |
| Root purge or history reset | Let normal sync install the signed checkpoint. See [reset recovery](../operations/federation.md#reset-recovery) for retained-work blockers. |
| Hub is permanently gone | Review [local-only disconnect](../operations/federation.md#detach-a-bridge-whose-hub-is-gone). It discards undelivered transport work and cannot revoke the remote grant. |

For a normal disconnect, finish pending deliveries and disconnect or revoke
downstream enrollments first. Preview, then detach:

```sh
kata federation bridge disconnect --project shared-replica --preflight
kata federation bridge disconnect --project shared-replica
```

This revokes the saved upstream enrollment and keeps the local project and
issues. Retry the same request after an interruption. Do not use `--local-only`
as a routine sync repair.

An adoption may disconnect before transmission (`preparing` or `frozen`) or
after a definite rejection (`rejected`). An uncertain or known-published
operation must resume and drain before ordinary disconnect; disconnect never
abandons that source fence or deletes published hub content.

The [operator reference](../operations/federation.md) owns the full credential,
retention, reset and signing rules. The [technical notes](../design/federation.md)
explain the protocol and its trust boundaries.
