---
title: Cron
description: Recurring child issues, shared job definitions, and attributed run evidence.
last_edited: 2026-10-31
---

Kata creates recurring child issues and inbox requests from enabled native
notification jobs. The owning hub daemon handles delivery; consumers read the
ordinary inbox and decide what to do.

Kata also stores portable job and workflow definitions and independent run
observations. External adapters schedule other jobs, resolve checkout and provider
configuration, create or update ordinary issues, deliver notifications, and launch processes.
An observation records evidence; its status does not control whether anything runs.

Browse jobs, runs, and workflows in `kata tui` with the `2`, `3`, and `4` tabs for the current project. On the Jobs tab, create, edit, pause, archive, and restore jobs; see the [`kata tui` keys](cli.md#daemon-and-diagnostics).

## Vocabulary

| Name | Meaning |
| --- | --- |
| Job | Configured work: its trigger, action, and execution settings. |
| Workflow | Reusable command or prompt steps and their dependencies. |
| Run | One execution and its reported status and results. |

An execution job can invoke a workflow or supply a single prompt. Each execution has its
own run UID. A workflow can also be referenced directly by a run.

Issue responsibility uses Kata's existing `claim`, `assign`, `unassign`, and
owner vocabulary. A federation lease provides exclusive coordination on an
issue. Recording a run changes neither ownership nor leases. See
[federation leases](../operations/federation.md#leases-and-write-gates) for that mechanism.

## Definitions

`kata cron job` and `kata cron workflow` support `create`, `list`, `show`,
`update`, `delete`, and `restore`. Commands return JSON. Creation retains a ULID:

```sh
kata cron job create --uid <job-uid> --file job.json --json
kata cron job list --json
kata cron workflow create --uid <workflow-uid> --file workflow.json --json
kata cron workflow list --json
kata cron job update <job-uid> --expected-event-uid <event-uid> --file replacement.json --json
kata cron job delete <job-uid> --expected-event-uid <event-uid> --json
```

Creation requires no expected event UID. Updates, deletion, and restoration
require the current `definition_event_uid` returned by `show`, passed as
`--expected-event-uid` or `expected_event_uid` in the request body. When both
are present they must match. A stale value fails with a conflict; re-read
`show` before retrying an edit. After an uncertain create response, inspect
`show` with the retained UID. If retrying create, reuse that same `--uid`;
an already-created definition conflicts and never becomes an update. Tombstones remain
readable, and `list --include-deleted` includes them. Deleting a workflow is
refused while a live job in the project uses it; update or delete that job
first.

Enabling a native recurring issue job activates delivery by the owning daemon.
Other jobs require an adapter installation to schedule them. No job launches a
process inside Kata.
Secret references are names, and checkout keys are portable names. Credentials,
host paths, and runtime handles are not part of this shared contract. Opaque
option objects retain exact JSON numbers.

### Request files

The `--file` body for `create` and `update` requires both `name` and
`definition`. Updates replace the complete definition; they are not patches.
Kata trims `name`, which must then be 1–256 bytes. Optional `actor` must
match `--as` when both are given; optional `expected_event_uid` applies to
`update` only. Create and update help includes copyable request bodies:

```sh
kata cron job create --help
kata cron job update --help
kata cron workflow create --help
kata cron workflow update --help
```

For an adapter prompt job, save this as `job.json`. Configure the
`example-project` checkout in the adapter. `options.agent` selects its prompt
agent; option names and supported agents depend on the adapter. Kata stores
these options without interpreting them.

```json
{
  "name": "Daily summary",
  "definition": {
    "version": 1,
    "kind": "job",
    "enabled": true,
    "trigger": {"kind": "cron", "cron": "0 9 * * *", "timezone": "UTC"},
    "action": {"kind": "execute", "prompt": "Review open issues and summarize next actions."},
    "issue": {"kind": "per-run", "title": "Daily summary"},
    "overlap": "forbid",
    "catchup": "latest",
    "checkout_key": "example-project",
    "options": {"agent": "codex"}
  }
}
```

Definitions are strictly decoded: unknown fields are rejected, and each
definition is bounded to 256 KiB. A *portable name* is 1–256 bytes of letters,
digits, `-`, `_`, and `.`, and is neither `.` nor `..`.

### Job definitions

| Field | Rule |
| --- | --- |
| `version` | Required; must be `1`. |
| `kind` | Required; must be `"job"`. |
| `enabled` | Boolean; defaults to `false`. |
| `trigger` | Required; see [triggers](#triggers). |
| `action` | Required; see [actions](#actions). |
| `issue` | Issue policy; see [issue policies](#issue-policies). Required for `execute` and native recurring issues. |
| `overlap` | Required; `forbid` or `allow`. |
| `catchup` | Required; `skip`, `latest`, or `all`. |
| `checkout_key` | Optional portable name. Not allowed with `notify`. |
| `grace_seconds`, `timeout_seconds` | Optional; zero or greater. |
| `secret_refs` | Optional object mapping portable names to portable names. |
| `options` | Optional JSON object for the adapter. `options.adapter` can name the adapter that runs the job; see [adapter responsibilities](#adapter-responsibilities). |

`options` objects, at any depth, cannot contain these keys (compared without
case): `token`, `api_token`, `api_key`, `password`, `secret`, `credentials`,
`authorization`, `cwd`, `path`, `checkout_path`, and `local_path`.

### Triggers

Every trigger has a `kind` and accepts an optional IANA `timezone`, such as
`America/New_York`. A trigger cannot carry fields that belong to another kind.

| `kind` | Required fields |
| --- | --- |
| `manual` | None. |
| `cron` | `cron`: a five-field expression (minute, hour, day of month, month, day of week) or a descriptor such as `@daily`. |
| `interval` | `interval_seconds`: a positive integer. |
| `once` | `at`: an RFC 3339 instant. |
| `issue-scheduled` | `issue_uid`: the ULID of the issue whose `scheduled_on` date fires the job. |
| `issue-deadline` | `issue_uid`: the ULID of the issue whose `deadline_on` date fires the job. Optional `lead_seconds` (zero or greater) is how long before the deadline to fire. |

### Cron expressions

A `cron` expression has five fields: minute, hour, day of month, month, and day
of week. It may instead be a descriptor such as `@hourly`, `@daily`, `@weekly`,
`@monthly`, or `@yearly`. Kata does not accept seconds, years, `L`, `W`, `#`,
or `H` syntax. Kata keeps the expression exactly as written.

- Day of week accepts `0` or `7` for Sunday, and names such as `MON`.
  `0 9 * * 7` runs on Sundays, `5-7` means Friday through Sunday, and `5-7/2`
  means Friday and Sunday.
- When both day of month and day of week are restricted, a day matching either
  field runs. `0 9 15 * 1` runs on the 15th and on every Monday.
- A range may wrap around. `0 22-2 * * *` runs at 22:00, 23:00, 00:00, 01:00,
  and 02:00.
- A step must be smaller than the span it steps over. Kata rejects `0 */24 * * *`
  and `*/60 * * * *`; write `@daily` or `@hourly` instead.
- A step that starts at one weekday continues through 7, which is Sunday.
  `0 9 * * 5/2` runs on Fridays and Sundays.

Schedules follow the trigger timezone's daylight-saving transitions:

- When clocks jump forward an hour, a time inside the skipped hour runs one
  hour later that day. In `America/New_York`, `30 2 * * *` runs at 03:30 EDT
  (07:30 UTC) on 2026-03-08.
- When clocks fall back, a time inside the repeated hour runs on both passes.
  In `America/New_York`, `30 1 * * *` runs at 05:30 and 06:30 UTC on
  2026-11-01. Hourly schedules run once per elapsed hour through the repeat.
- On transition days in zones that shift by 30 minutes, such as
  `Australia/Lord_Howe`, or that skip local midnight, such as
  `America/Santiago`, a daily schedule can miss that day's occurrence.

Kata releases up to 0.19.0 used a parser that rejects `7` in day of week and
wrapping ranges, accepts steps as large as their span, and reads `5/2` as
Friday only. Before writing an expression that uses one of these forms, upgrade
every hub, spoke, and adapter with its own parser that reads the job. Older
peers reject the definition or schedule it differently.

### Actions

| `kind` | Fields and rules |
| --- | --- |
| `execute` | Exactly one of `prompt` (not blank) or `workflow_uid` (a workflow ULID). Requires an `issue` policy. Cannot use an `issue-deadline` trigger. Cannot carry `recipient` or `message`. |
| `notify` | `recipient`: an exact inbox address of at most 128 bytes, such as `worker` or `worker/adapter`, or `current-owner-or-author`. `message`: at most 1024 bytes; required unless `recipient` is `current-owner-or-author` or this is a native recurring issue job. Cannot carry `prompt` or `workflow_uid`, and the job cannot set `checkout_key`. |

A `notify` job with an `issue-scheduled` or `issue-deadline` trigger targets
that issue and may omit `issue`. Any other `notify` job requires an `issue`
policy of kind `existing` or the native recurring `per-run` policy below.

### Issue policies

| `kind` | Fields and rules |
| --- | --- |
| `existing` | `uid`: the ULID of an existing issue. No other fields. |
| `per-run` | `title` (not blank) and optional `body`. Optional `scheduled_offset_seconds` and `deadline_offset_seconds` are relative date offsets, in seconds, for the issue the adapter creates. No `uid`. |

For `notify`, `per-run` instead requires `parent_uid` and a nonblank `title`.
It accepts an optional `body`, but no `uid` or date offsets. This selects native
recurring child delivery. `parent_uid` is not accepted on execution jobs.

### Recurring child issues

Keep an ordinary parent issue open for the continuing commitment, then create a
job with `--parent <ref>`. For example, save this request as `job.json`:

```json
{
  "name": "Monthly service review",
  "definition": {
    "version": 1,
    "kind": "job",
    "enabled": true,
    "trigger": {"kind": "cron", "cron": "0 9 1 * *", "timezone": "UTC"},
    "action": {"kind": "notify", "recipient": "current-owner-or-author"},
    "issue": {
      "kind": "per-run",
      "title": "Review service",
      "body": "Check health and record any follow-up work."
    },
    "overlap": "allow",
    "catchup": "all"
  }
}
```

Create the job with the parent's bare short ID, qualified ref, or full UID:

```sh
kata cron job create --parent example-project#abc4 --file job.json --json
```

The CLI resolves the ref and stores its full UID in `definition.issue.parent_uid`.
The stored definition stays UID-only, so short ID growth and project renaming do
not change its parent identity. API clients can still supply `parent_uid` in the
request. If the file supplies it alongside `--parent`, both must name the same
UID. `--parent` requires `issue.kind: "per-run"`; execution jobs cannot carry a
parent binding.

The job uses the parent's project when no project is otherwise selected. An
explicit `--project` or workspace binding must match that project. Job updates
also accept `--parent`, together with the current `--expected-event-uid`; a
native job's existing parent binding remains immutable.

Each due occurrence creates one ordinary child with a parent link and an inbox
request. Earlier children may remain unfinished. The child copies the parent's
current owner at delivery; an unowned child remains unowned. Routing follows that
owner, or the parent author when unowned. Set an exact recipient such as
`worker/consumer` to keep routing fixed. An omitted message uses Kata's default
recurring notification message. Read with `kata inbox --for worker/consumer`,
then clear the request with `kata notify <child-ref> --to worker/consumer --clear`
after handling it. Reading does not acknowledge it.

The title gains a UTC occurrence timestamp. The configured title can be at
most 991 characters, so the child title stays within 1024 characters. Children carry
`cron.occurrence` provenance and no `scheduled_on`, so the ordinary date sweeper
does not request the same occurrence again. Clearing a request, closing a child,
or restarting the daemon never rewinds delivery. Native delivery creates no run
observation; a consumer may record its own execution evidence separately.

Native jobs accept only `cron` and `interval` triggers and require
`overlap: "allow"`. Calendar expressions use the trigger's IANA timezone, with
UTC as the default. They follow that timezone's daylight-saving transitions,
as described in [Cron expressions](#cron-expressions).
An embedded `TZ=` or `CRON_TZ=` must agree with `timezone`. Use
`{"kind":"interval","interval_seconds":7200}` for an elapsed two-hour cadence; intervals must be at least 60 seconds;
`@every` is not supported for native delivery. Intervals start at the first
binding time plus one interval. Calendar schedules start strictly after that
binding time, without historical backfill.

| `catchup` | After downtime or a pause |
| --- | --- |
| `all` | Create every missed child, at most 1000 per job per sweep. Later sweeps continue from durable progress. |
| `latest` | Create only the newest missed occurrence. Calendar scans examine at most 20,000 occurrences per sweep, retain a fixed scan horizon, and deliver when that scan finishes. Intervals find the newest occurrence directly. |
| `skip` | Require positive `grace_seconds`; discard older occurrences and deliver those within the grace window, with the same bounded sweep limits. |

The existing daemon sweep runs at startup and every 30 seconds by default. Keep the authoritative hub running
for unattended delivery. Local projects deliver locally; federated projects
deliver only on their enabled owning hub. Spokes never deliver, even with push
writes enabled, and cannot create, edit, delete, or restore native recurring
definitions. Run the same command against the project's owning hub, for example
with `--daemon <hub-name>`. An old
peer that lacks `cron_v1` cannot receive a project containing cron state,
including native recurring metadata on ordinary issue events. Capability refusal
happens before a baseline or cursor change. Projects with native recurring delivery history cannot be adopted into
a different hub through spoke snapshots; the refusal preserves their existing
local authority and history.

Create, update, delete, and restore retain ordinary authorization and expected
revision checks. Once bound, a job's parent cannot change and the job cannot
become an adapter job. Edits preserve committed progress and the original
interval anchor; an unfinished latest scan restarts against the new definition.
Disable the job or close its parent to pause delivery. An archived project or a
deleted parent also pauses it. Re-enabling, reopening, or restoring resumes the
configured catch-up policy. A pause does not skip occurrences: with
`catchup: "all"`, resuming creates every child missed while paused. This
includes the time since creation for a job created disabled. Use `latest` or
`skip` when a pause must not backfill. Deleting the job stops delivery until
restoration.
Keep the parent in the job's project. Kata rejects moving the parent alone
while its native job exists, including when the job is disabled or deleted;
restoring the job can resume its binding. Merge projects to move the job and
parent together.

Child creation, its initial notification, and the parent's
`cron.progress.<job-uid>` checkpoint commit together. Each delivery therefore
changes the parent's revision and update time; re-read the parent before a
conditional edit. Backup, restore, federation,
and project merge preserve this metadata. Delivery fails closed if progress is
missing or malformed; ordinary issue writes and spoke ingest cannot change or
delete progress or occurrence provenance. Recurrence templates also reject these
system keys, including when older templates are materialized. Inspect that state before recovery;
there is no implicit reset that could recreate already delivered children.
Native jobs reject execution timeouts, secrets, adapter options, checkout keys,
and issue date offsets.

### Workflow definitions

A workflow definition has `version` `1` and a non-empty `steps` array. It may
also carry `about`, `input`, and `options` (with the same key rules as job
options). Each step has these fields:

| Field | Rule |
| --- | --- |
| `key` | Required portable name, unique within the workflow. |
| `kind` | `command` (requires `command`, no `prompt`) or `prompt` (requires `prompt`, no `command`). |
| `after` | Optional keys of earlier steps this step depends on. |
| `retries` | Optional; 0–100. |
| `options` | Optional JSON object for the adapter. |

```json
{
  "name": "nightly-tests",
  "definition": {
    "version": 1,
    "steps": [
      {"key": "check", "kind": "command", "command": "git diff --check"},
      {"key": "test", "kind": "command", "command": "go test ./...", "after": ["check"]}
    ]
  }
}
```

### Adapter command jobs

A job runs commands by referencing a workflow. `action.kind` remains
`execute`; use `workflow_uid` instead of `prompt`, and put each command in a
workflow step. The referenced workflow must already exist in the same project.

Save the workflow above as `workflow.json` and create it with a retained UID:

```sh
kata cron workflow create --uid 01J00000000000000000000010 --file workflow.json --json
```

The UID here is an example. Choose your own ULID and use it in both the create
command and the following job body. Save this request as `command.json`:

```json
{
  "name": "nightly-tests",
  "definition": {
    "version": 1,
    "kind": "job",
    "enabled": true,
    "trigger": {"kind": "cron", "cron": "0 2 * * *", "timezone": "UTC"},
    "action": {"kind": "execute", "workflow_uid": "01J00000000000000000000010"},
    "issue": {"kind": "per-run", "title": "Nightly tests"},
    "overlap": "forbid",
    "catchup": "latest",
    "checkout_key": "example-project"
  }
}
```

```sh
kata cron job create --uid <job-uid> --file command.json --json
```

The external adapter schedules the job and runs its workflow in the configured
checkout. Kata stores the definitions and run evidence.

## Capabilities

`kata cron capabilities --json` reads the project's UID and advertised
`event_features` without changing state. The corresponding GET is
`/api/v1/projects/{project_id}/cron/capabilities`; the
`X-Kata-Event-Features` header advertises only `cron_v1`, covering portable
definitions, run observations, and native recurring child delivery. Missing,
unknown, or failed advertisements remain distinct outcomes. This is an ordinary project
read and creates no permissions. Ordinary authentication, project isolation,
and issue-scoped route restrictions apply.

### Upgrade limits

These limits apply to projects with cron data and the adapters that consume
it. For ordinary issue synchronization, use the normal
[hub-first upgrade procedure](../operations/federation.md#compatibility-and-rolling-upgrades);
spokes can upgrade later.

Kata 0.19.0 advertised both `cron_v1` and `cron_notify_children_v1`.
Kata 0.20.0 advertises only `cron_v1` for all cron support, with no alias for
the removed name. Fully compatible mixed-version cron operation is not
supported across this change:

- A 0.20.0 spoke cannot read a 0.19.0 hub project that requires the removed
  capability for native recurring-child state. Upgrade the hub first.
- Adapters that require `cron_notify_children_v1` must switch to `cron_v1`
  when their daemon upgrades.
- Upgrade every peer and adapter that reads a project's definitions before
  using the new expression forms, such as Sunday `7` or wrap-around ranges.
- Check existing expressions before upgrading: steps as large as their
  field's span are now rejected. Use `@daily` instead of an hourly `*/24`
  step, or `@hourly` instead of a minute `*/60` step.

Stored definitions and events stay unchanged. Deleting a cron job does not
remove its retained history or the project's cron capability requirement.
See [Cron triggers](#triggers) for the expression rules.

## Independent run evidence

An adapter creates one independent run UID and retains it across retries:

```sh
kata cron run observe <run-uid> --json-input observation.json --json
kata cron run list --limit 100 --json
kata cron run show <run-uid> --json
```

The HTTP operation is `PUT /api/v1/projects/{project_id}/cron/runs/{run_uid}`
(`observeCronRun`). An omitted JSON `teammate` inherits `--teammate` or
`KATA_TEAMMATE`; an explicit empty flag suppresses that inheritance. An explicit
JSON string or `null` freezes attribution across retries and ignores the
environment. An explicit flag must match the frozen value (`null` matches an
empty flag). An empty JSON string remains invalid; it is never replaced by the
environment. For example:

```json
{
  "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA",
  "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAB",
  "occurrence_key": "daily:2026-10-06",
  "actor": "worker",
  "teammate": "adapter",
  "executor_label": "local-worker",
  "status": "running",
  "summary": {"version": 1, "message": "Review started"},
  "expected_revision": 0
}
```

At least one paired job/event or workflow/event reference is required; both pairs
may be supplied. References identify historical definitions, including earlier
versions and tombstones, within the project. An optional issue UID is an
ordinary existing issue in that project. Distinct run UIDs may share the same
occurrence key and issue. No occurrence is reserved by logging a run.

UID, project, definition references, occurrence key, issue UID, attributed actor,
teammate, executor label, and server-created time are immutable. Status, summary,
start time, and end time are mutable evidence. Supported statuses are `running`,
`succeeded`, `failed`, `cancelled`, and `unknown`; they impose no transition or
launch protocol. Summary version 1 permits a message and nonnegative integer
input/output token counts and is bounded to 64 KiB. The whole observation and
its event are bounded to 96 KiB. Unknown fields are rejected. Times are RFC3339
instants; an end cannot precede a start. An occurrence key is at most 1024
characters; actor and optional executor label are at most 256 bytes. Teammates
use Kata's existing 1–64 character ASCII names.

A new observation uses expected revision 0. A changed observation requires its
current revision. An identical retry of the same UID and canonical evidence
returns `replayed: true`, emits no event, and changes no revision even when its
expected revision is stale. A different immutable identity always conflicts.
Authenticated actor attribution overrides an untrusted request actor, and
revocation is rechecked in the write transaction. Logging does not change an
issue's owner, dates, lease, status, or readiness.

Responses contain `run`, the exact committed `events`, and `replayed`. Run
listing is project-scoped, accepts `--job-uid` to list one job's runs, and
orders by creation time and UID descending. Use `next_before_uid` with `--before-uid` for another
page. Ordinary federation publishes observations from spokes, folds evidence
by original event provenance per run UID, and preserves independent runs in
baselines and backup. Run identity conflicts fail rather than replacing a run.

An adapter with a machine-readable cause for a run's outcome starts
`summary.message` with the line `reason: <code>`, then puts any detail on the
next line. The `<code>` matches `[a-z0-9_.-]{1,64}`. Any status may carry one.
Kata does not parse or validate the reason line; it is ordinary message text
under the summary rules above. Clients that know the convention can show
the code and filter on it client-side. Each adapter defines and documents its
own codes; where two adapters mean the same thing they share the name, as
`agent_quota`, `agent_login`, and `launch_unproven` do. The `reason: ` marker
keeps short messages such as `admitted` from reading as codes.

## Native issue planning dates

`kata show <issue-ref> --planning-dates --json` and GET
`/api/v1/projects/{project_id}/issues/{ref}/planning-dates` read one consistent
issue revision with required nullable `scheduled_on` and `deadline_on` objects.
Each present object contains `field`, raw `value`, resolved `timezone`, and a
UTC `instant`. The flat response also contains `project_id`, `issue_uid`, and
ordinary `revision`.

Native date parsing applies the issue timezone first, then the recurrence
fallback for scheduled dates, then daemon default timezone and UTC. Deadline
dates have no recurrence fallback. Explicit UTC values resolve to UTC. The read
uses native civil-time and DST behavior; it evaluates no clock and changes no
readiness or persisted state. Normal project and issue-subtree read policy applies.

### Reached schedules

An adapter can start date-driven work from the event stream instead of polling.
When an open issue's `scheduled_on` is reached, the daemon's date sweeper
records one `issue.scheduled_reached` event by `system`. Its payload is the
planning-date object above: `field`, raw `value`, resolved `timezone`, and UTC
`instant`.

- The sweeper runs at daemon startup and every 30 seconds, so the event follows
  the instant by up to 30 seconds. A date that passed while the daemon was down
  is recorded at the next startup.
- Schedules that were already overdue before the upgrade record no event. The
  first sweep after upgrading stores its time as the database's floor. The
  sweeper records a date only if its instant is at or after the floor, or if
  its current value was set after the floor. Setting a past date after the
  upgrade therefore records it at the next sweep. Overdue schedules still
  surface through the inbox as before.
- Each raw value is recorded once per issue. A rescheduled issue records its new
  value when that value is reached. A value that was already recorded is not
  recorded again, even if the date moves away and back.
- The event does not change the issue or its revision, and it is recorded
  whether or not the inbox slot it would notify is occupied.
- Closed issues are skipped. An enabled spoke records no event of its own; it
  receives the hub's event when it pulls hub changes. Hub-to-hub relay does
  not carry the event.
- `deadline_on` records no event; deadlines remain notification-only.

`kata events --tail` and hooks subscribed to the event type, `issue.*`, or `*`
receive it.

## Adapter responsibilities

Adapters must exclude `notify` jobs with `issue.kind: "per-run"` from their
schedulers; those jobs belong to the native hub. Existing strict decoders reject
the new `parent_uid` shape instead of scheduling it.

Shared `enabled` configuration does not activate an adapter installation. Each
adapter owns its local activation, scheduler, overlap and catchup handling,
checkout and secret mappings, process lifecycle, and conversation recovery.
A second adapter may independently execute the same occurrence. Each execution
keeps a separate run UID; no shared observation reserves the issue or occurrence.

A job may name its adapter in `options.adapter`. An adapter neither schedules
nor manually runs a job whose key names another adapter, and says why in its own
status. Any adapter may run a job without the key. Kata stores the key and
arbitrates nothing. The value names an adapter, not an installation; each
adapter decides which of its installations runs the job. `herdr-kata` and
`tern-kata` are examples, and each adapter documents its own value. Adapters
that do not read the key ignore it: Herdr Kata v0.1.0, for example, logs
`ignored options key: adapter` and still runs the job.

Local transcripts, process IDs, session handles, credentials, and delivery
buffers are not shared run evidence. An adapter may buffer a failed observation
write and retry it later without blocking its own work.

The adapter must refresh current definitions before starting new scheduled work;
offline cached data is for labeled reads. Portable `cron` triggers use the
configured trigger timezone. Issue-date triggers use the planning-date instant
above: issue timezone, recurrence fallback, daemon default and UTC remain
authoritative. Future `scheduled_on` and `someday` defer ordinary execute
readiness; `deadline_on` never does. Deadline actions are notification-only.

Kata's default-date sweeper continues to request the current owner's attention,
or the author's when unowned, for reached native schedules and deadlines. The
same-source, zero-lead `current-owner-or-author` alias must not produce a second
adapter notification. Custom offsets and recipients use ordinary notifications.
Inbox addresses are exact: `worker` does not aggregate `worker/adapter`. Reads do
not clear requests; the recipient handler clears only after handling and reads
back. Attention is a replaceable per-issue/recipient slot, so replacement and clear
can race.

Shared definitions and run evidence inherit ordinary authentication, project/
issue-scoped roles, actor attribution, token revocation and credential-origin
checks. Capability discovery advertises compatibility only. Shared federation and
backup do not transfer local activation, checkout credentials, execution buffers
or conversation handles; preserve those separately under the adapter's backup
workflow. PostgreSQL runtime roles still cannot perform owner-only migrations.

[Herdr Kata](https://github.com/salmonumbrella/herdr-kata/blob/main/docs/native.md)
is one adapter that implements this contract; its documentation covers setup.

## Storage and backup

SQLite and PostgreSQL schema 31 store cron data in `cron_jobs`,
`cron_workflows`, and `cron_runs`. JSONL export and import retain shared
definitions, including tombstones, and bounded run evidence. Native recurring
delivery reuses ordinary issue metadata and parent links; it adds no tables or
schema version. Keep parent checkpoints and child provenance in the same backup. Runs with
`running` or `unknown` status do not prevent an explicitly requested restore,
federation reset, adoption, or project merge; those operations keep their
ordinary identity, revision and credential guards. See
[PostgreSQL migrations](../development/postgres-migrations.md) for owner-only
migration, backup, runtime grants, and rollback requirements.

## Routes, events, and records

Workflow routes are `/api/v1/projects/{project_id}/cron/workflows` and
`/api/v1/projects/{project_id}/cron/workflows/{cron_uid}`. Responses use
`workflow` or `workflows`; job actions and run observations use `workflow_uid`,
and run observations pair it with `workflow_definition_event_uid`. Events use
`cron.workflow.*`, and JSONL workflow records use `cron_workflow`.
