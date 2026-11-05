---
title: Operate cron across a team
description: Set up shared cron projects, choose adapter runtimes, and inspect delivery and run evidence across hubs and devices.
last_edited: 2026-11-05
---

# Operate cron across a team

Use one company project to share cron definitions and run evidence while each
member keeps a personal hub and chooses where adapters run. The owning company
hub delivers native recurring child issues. External adapters schedule and
execute other jobs on the runtimes their operators activate.

This guide connects the [cron contract](../reference/cron.md),
[relay setup](../guide/project-relays.md), and [federation operations](federation.md).
Project relays require Kata 0.19.0 or later; check the
[cron upgrade limits](../reference/cron.md#upgrade-limits) before mixing versions.

## Choose the shape

```text
Company hub (root for example-project)
  ↕                         ↕
Personal hub (relay)       Device (bridge leaf)
  ↕
Device (bridge leaf)

Adapter runtimes may connect to any of these daemons.
```

Root, relay and leaf are roles for one project. A personal hub can root its own
projects while relaying the company project. All copies of the company project
retain its UID; paths, aliases and local activation stay on each device. Follow
[Choose the shape you need](../guide/project-relays.md#choose-the-shape-you-need)
for connection limits and [Federation roles](federation.md#roles) for authority.

A bridge leaf connected directly to the company hub still uses relay protocol.
It is different from an administrator-provisioned legacy direct spoke. That
distinction matters when writing native recurring jobs.

## Who does what

| Responsibility | Company hub / root | Personal relay or bridge leaf | Legacy direct spoke | Adapter runtime |
| --- | --- | --- | --- | --- |
| Native recurring delivery | Enabled owning hub delivers | Never delivers | Never delivers | Excludes native per-run notify jobs |
| Adapter job scheduling | Only through an activated adapter | Only through an activated adapter | Only through an activated adapter | Owns scheduling and process lifecycle |
| Definition writes | Ordinary project writer | Ordinary credentials for adapter jobs/workflows; write native jobs against the root | Native jobs may forward with direct `push`; adapter jobs/workflows use ordinary credentials | Uses ordinary daemon credentials |
| Run observation | Stores evidence | Stores and replicates evidence | Stores and replicates evidence | Reports a retained run UID with ordinary credentials |
| Reads | Project-scoped | Project-scoped replica | Project-scoped replica | Reads its selected daemon and project |
| Capability advertisement | Advertises protocol support | Advertises protocol support | Advertises protocol support | Checks support; advertisement grants no authority |

The owning rules are [native delivery and forwarding](../reference/cron.md#recurring-child-issues),
[federation token boundaries](federation.md#token-boundaries),
[run evidence](../reference/cron.md#independent-run-evidence),
[capabilities](../reference/cron.md#capabilities), and
[adapter responsibilities](../reference/cron.md#adapter-responsibilities).
Definitions and run evidence replicate; local scheduler activation does not.

## Set up a team

1. Configure the company daemon for [identity tokens](remote-daemon.md#identity-tokens)
   and restart it. The bootstrap token administers credentials; ordinary account
   tokens perform attributed project writes. A shared token without identity
   mode cannot mint usable member tokens.
2. Using an ordinary account with access, create the company project:

   ```sh
   kata projects create example-project
   ```

3. Using daemon-owner authority, create a team and restrict the project to it:

   ```sh
   kata teams create operators
   kata teams members add operators --actor operator
   kata projects access set example-project --visibility teams --team operators
   kata projects access show example-project
   ```

   Follow [Team visibility](federation.md#team-visibility) for membership and
   policy changes. Membership follows the canonical account actor; a teammate
   label does not grant project access.
4. Using the bootstrap credential, issue each member an ordinary account token:

   ```sh
   kata tokens create --actor operator --team operators
   ```

   Deliver each token through your credential provisioning process. Keep it out
   of project files and transcripts. Use the account token for attributed work.
5. With an ordinary account token that can access the project, enable sharing:

   ```sh
   kata federation enable --project example-project
   ```

   Members then follow [relay setup](../guide/project-relays.md#before-connecting)
   to connect their personal hub or a device leaf. Configure upstream credentials
   in the receiving daemon's service environment. Preview before connecting;
   use an unused local project name and bind a workspace after connection.
6. Choose how members author native jobs. Ordinary member tokens create bridges,
   which cannot forward native writes. Run native job commands against the
   owning hub with ordinary credentials, for example using `--daemon company-hub`
   after configuring that catalog entry. Alternatively, an administrator can
   provision a [direct enrollment](federation.md#token-boundaries) with `push`.
   Native forwarding also requires `native_cron_writes` in federation metadata.
7. Check protocol support on each selected daemon and project:

   ```sh
   kata cron capabilities --project example-project --json
   ```

   `cron_v1` advertises compatibility. It neither advertises native forwarding
   nor grants permission to execute. Install adapters on the intended runtimes
   using the setup guides linked under [Adapter responsibilities](../reference/cron.md#adapter-responsibilities).

## Decide which runtime runs a job

Set `options.adapter` on execute jobs to name the intended adapter family.
Check that every installed adapter version honors it; older versions may ignore
it and still run the job. The key does not select an installation. Activate
scheduling only on the installations meant to execute that job.

Shared `enabled` does not activate an installation. Two active runtimes can
independently execute one occurrence, each with its own run UID. Recording a run
does not reserve the occurrence. Keep an explicit runtime assignment in your
operating procedure and check each adapter's local status. See
[Adapter responsibilities](../reference/cron.md#adapter-responsibilities) for
selection, activation, overlap, credentials and refresh requirements.

## Watch delivery and runs

For adapter-reported evidence in the selected project:

```sh
kata cron run list --project example-project --json
kata cron run list --project example-project --job-uid <job-uid> --json
kata cron run show <run-uid> --project example-project --json
kata events --project example-project --tail
```

Use `next_before_uid` for further history; see [Run list page size](../reference/cron.md#run-list-page-size)
for HTTP, CLI, TUI and MCP defaults. In the TUI, press `2` for Jobs, `3` for Runs
and `4` for Workflows. These [cron tabs](../reference/cli.md#daemon-and-diagnostics)
show one selected project, and Runs loads its newest 100 observations.

Read `actor`, `teammate` and optional `executor_label` to identify reported work.
Adapters can start a summary with `reason: <code>`; the
[reason-line convention](../reference/cron.md#independent-run-evidence) is ordinary
message text, so clients interpret it themselves. The event stream includes
`cron.*` definition and observation events.

Successful native delivery creates no run observation. Inspect child issues,
their inbox requests and the parent's progress instead. Reading the inbox does
not acknowledge a request. Follow [Recurring child issues](../reference/cron.md#recurring-child-issues)
for provenance, routing and acknowledgement.

## Edit safely

Read the current definition, retain its UID, and replace the full document with
its `expected_event_uid`. A conflict means someone changed it: read again and
decide how to reconcile. Never retry with a newly fetched revision blindly. See
[Request files](../reference/cron.md#request-files) for commands and conflict rules.

Set `enabled: false` to pause a job. Closing a native job's parent also pauses
native delivery. With `catchup: "all"`, resuming backfills occurrences missed
while paused, including time since creation for a job created disabled. Choose
`latest` or `skip` when that is not intended; follow the full
[native pause and catchup rules](../reference/cron.md#recurring-child-issues).

## Upgrade a mixed fleet

Upgrade the owning hub first. Coordinate each daemon upgrade with any adapters
that require the removed `cron_notify_children_v1` capability. Upgrade every
peer and adapter that reads the project before using newer expression forms.
Use [Cron upgrade limits](../reference/cron.md#upgrade-limits) alongside the
[federation rolling-upgrade procedure](federation.md#compatibility-and-rolling-upgrades).

## Back up and recover

Back up shared definitions, tombstones and run evidence with the ordinary
[backup procedure](backup-restore.md). Keep native parent progress and child
provenance together; see [Cron storage and backup](../reference/cron.md#storage-and-backup).
Federation reset snapshots carry definitions and run history; use the
[reset recovery procedure](federation.md#reset-recovery).

Back up local activation, checkout and secret mappings, credentials and buffered
observations separately under each adapter's backup procedure. Shared backups
and federation do not transfer that installation state; see
[Adapter responsibilities](../reference/cron.md#adapter-responsibilities).

## Known limits

### Runs that never finish

Kata does not automatically finalize abandoned runs or enforce execution
`timeout_seconds`. Adapter enforcement is adapter-owned. A reported `running`
status may outlive its process; see [Run evidence](../reference/cron.md#independent-run-evidence).

### Which runtimes are alive

Kata has no runtime inventory. An optional, self-reported `executor_label` on a
[run](../reference/cron.md#independent-run-evidence) identifies evidence, not liveness.

### Skipped occurrences

Native delivery has no per-occurrence skip/reason record for a disabled job,
closed parent, non-authority daemon or discarded catchup occurrence. A skip can
still advance durable parent progress. See [Native delivery](../reference/cron.md#recurring-child-issues).

### Execution on replicas

Relays and spokes expose shared execute definitions to local adapters. Kata has
no central runtime execution gate; shared `enabled`, credentials, adapter
selection and local scheduling rules still apply. See [Adapter responsibilities](../reference/cron.md#adapter-responsibilities).

### Who may edit execute jobs

An ordinary project writer can change an execute job's prompt or `enabled`.
[Team visibility](federation.md#team-visibility) controls project access; it does
not assign separate cron editors and executors.

### Reading across projects

[Cron reads](../reference/cron.md#routes-events-and-records) are project-scoped.
Select each project to inspect its jobs and runs.

### Pausing many jobs

The [job lifecycle](../reference/cron.md#definitions) has no bulk or project-wide
pause, run-now or cancel operation. Change individual definitions and use the
adapter's own process controls for active executions.

### Run history growth

There is no automatic run-retention policy. Include retained evidence in
[capacity and backup planning](../reference/cron.md#storage-and-backup).
