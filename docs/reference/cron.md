---
title: Cron
description: Recurring child issues, shared job definitions, and attributed run evidence.
last_edited: 2026-11-05
---

Kata creates recurring child issues and inbox requests from enabled native
notification jobs. The owning hub daemon handles delivery; consumers read the
ordinary inbox and decide what to do.

Kata also stores portable job and workflow definitions and independent run
observations. External adapters schedule other jobs, resolve checkout and provider
configuration, create or update ordinary issues, deliver notifications, and launch processes.
An observation records evidence; its status does not control whether anything runs.

Browse jobs, runs, and workflows in `kata tui` with the `2`, `3`, and `4` tabs for the current project. On the Jobs tab, create and edit jobs; see the [`kata tui` keys](cli.md#daemon-and-diagnostics).

For a run organized as dependent issues with human approval, see
[Gated workflows](../workflows/gated-workflows.md).

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

The `kata tui` Jobs tab previews these instants with the same schedule code:
see the `kata tui` notes in the [CLI reference](cli.md#daemon-and-diagnostics).

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
deliver only on their enabled owning hub. Spokes never deliver.

Direct spokes with push enabled can create, update, delete, and restore native
recurring jobs through their existing enrollment. Each request checks hub
support, pushes pending local events (including a newly created parent), and
forwards the write synchronously. The hub validates the job, preserves its
progress anchor, and attributes the write to the enrollment's bound actor.
The complete preflight and write have a 15-second limit; no offline write is queued.
Relay, disabled, and push-disabled bindings retain the refusal. If forwarding is
unavailable, run the same command against the project's owning hub, for example
with `--daemon <hub-name>`. An older hub must be upgraded to advertise native
cron authoring; `cron_v1` alone does not advertise that capability.

The returned `definition_event_uid` can immediately authorize the next update
or lifecycle action, even before pull. One exception: when the hub converted an
adapter job to native and the spoke has not pulled that change yet, the spoke
still routes delete and restore for that job locally and reports a conflict.
Wait for the next pull, or run the action against the hub. Local `show` and `list` catch up through
normal replication. Forwarded jobs report the local project ID and zero job
`id` and `revision` until a local projection exists; response events are omitted.
A lost write response means the outcome may be unknown. Inspect the job on the
hub or retry creation with the same `--uid`; an already-created UID returns a
conflict containing the hub's current job. HTTP callers should prefer stable-UID
PUT creation because POST allocates a UID that may be lost with the response.

An old peer that lacks `cron_v1` cannot receive a project containing cron state,
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
`skip` when a pause must not backfill. The TUI's [reopen
prompt](cli.md) counts every missed occurrence, not just the
1000 that one sweep delivers. Deleting the job stops delivery until
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

## Adapter conformance

Adapter authors can adopt **adapter conformance profile v1** to give portable
jobs consistent scheduling and outcome semantics. MUST, SHOULD and MAY below
apply only to adapters claiming this profile. Reading `cron_v1` documents alone
does not imply conformance. Kata stores definitions and independent evidence;
it does not launch adapter work or enforce this execution contract.

### Eligibility and schedule instants

A conforming scheduler MUST exclude deleted and disabled jobs and jobs naming
another adapter in `options.adapter`. It MUST follow the existing
[native-job exclusion](#adapter-responsibilities). A `manual` trigger runs only
on an adapter-local request, never as scheduled work. Non-native adapter
notifications retain the existing [delivery rules](#adapter-responsibilities).
Local activation is still required.

`issue-scheduled` and `issue-deadline` retain the existing
[issue-date readiness and delivery rules](#adapter-responsibilities). This
profile adds no date-dependent scheduling rules for those triggers.

Calendar instants MUST use [Kata's grammar and DST rules](#cron-expressions).
An embedded `TZ=` or `CRON_TZ=` directive takes precedence over the trigger's
`timezone`; otherwise use that timezone, then UTC. An interval's anchor MUST be
replicated job `created_at`. Its first instant is one interval after creation:
`created_at + k * interval_seconds`, for integers `k >= 1`. A cron `@every` uses
that anchor and the parser's effective duration, truncated to whole seconds.
The shared grammar rejects durations below one second. A `once` instant at or
before creation MUST NOT fire.

Without a checkpoint, initialize the horizon to the later of creation and
installation or project enrollment, even when the job is first seen disabled.
Freeze it while ineligible; enumerate strictly after it when eligible. Keep
installation time stable across restarts; do not backfill earlier work. Native
recurring issues use their first-binding anchor; this profile does not change it.

### Checkpoints, catchup and overlap

A checkpoint contains an inclusive enumeration horizon `through`, opaque
`definition_event_uid`, and pending instants. On every decision, combine retained
pending UNION new instants in `(through, now]`, sort and deduplicate them.
`through` says what was enumerated, not what ran. After each eligible decision,
set it to `max(through, now)`; ineligible calls freeze it as described below.
Retained pending can be later than `now` after a clock adjustment;
selected future instants remain pending until due.

Apply catchup to the combined set on every call, even when a gate opens at the
same clock:

| Policy | Selection |
| --- | --- |
| `all` | All candidates in ascending order, subject to declared limits. |
| `latest` | Newest candidate only, including a future pending candidate. |
| `skip` | Age at most `grace_seconds` plus declared tolerance, including equality. |

The adapter MUST publish in its own documentation its scheduler tolerance
from 0 to 120 seconds, exact count limit and age window, and `failure_policy`.

After **every** catchup policy, apply the age window inclusively, then retain
the newest count up to the declared limit. Drop oldest first. Zero means
unlimited; a nonzero count is at least 100 and a nonzero window is at least
86,400 seconds. These limits preserve the newest 100 selected candidates
within the preceding 24 hours, including retained pending work. Other retention schemes are outside v1.

With `overlap: "forbid"`, local prepared, running or held work occupies the gate.
Uncertain termination also occupies it, including an `unknown` report. These
are local states, not new Kata statuses. While occupied, retain selected work
as pending unless the optional deferral rule below discards it. With a free gate, launch the oldest due selected instant and retain
the rest. With `allow`,
launch all due selected instants. Unknown work releases occupancy only after
reconciliation establishes termination.

The gate covers one runtime. Multiple adapters may execute the same occurrence;
observations reserve nothing. Each execution keeps its own run UID. Occurrence
keys MUST be stable across restarts and observation retries; canonical UTC
RFC3339Nano instants are recommended.

### Definition changes and pauses

Before launching scheduled work, refresh the definition. Any different
`definition_event_uid` replaces it; do not infer ordering from opaque UIDs.
Discard old pending even for metadata-only edits, preserve `through`, and store
the new identity. Already
launched runs retain frozen definition references and local holds.

Disabled, other-adapter, native and manual calls freeze the horizon and retain
pending unless the revision changes. Apply revision replacement even while
ineligible; deletion also clears pending. Re-enable, restore and reassignment
apply catchup after the preserved horizon, subject to the declared count and
age limits. Unlike this profile, [native `all`](#recurring-child-issues) retains
missed occurrences without an age window and drains them within its per-sweep
budget. Pending instants discarded on an adapter revision change are not
reconstructed. Adopters SHOULD describe changes to their earlier interval
anchors or checkpoint behavior.

### Attempts, deadlines and outcomes

A step allows at most `1 + retries` attempts. Retry only certainly failed
attempts, never unknown or successful ones. All attempts share one run UID.
Dependencies MUST succeed before a dependent starts. The declared
`failure_policy` selects the branch behavior:
`continue` permits independent branches after terminal failure; `stop` skips
unstarted steps and dependents from the earliest failed/cancelled/unknown terminal,
except tied terminals and their successful prerequisites (including equality).
Started steps retain results. Retry-eligible failures are nonterminal.

A positive `timeout_seconds` sets one deadline from run start for every attempt.
Zero means no Kata deadline; document adapter defaults separately. Cancellation
has an effective instant. The cutoff is the earlier of that instant and the
run-wide deadline, when either exists; cancellation governs equal instants.
No new attempt starts at or after the cutoff. An attempt completed strictly
before it retains its result even when evaluated later; equality is subject to
the cutoff. A run completed before cancellation keeps its final outcome.
At the cutoff, stop attempts and retain the local hold while termination
remains uncertain.

While no cutoff has been reached, incomplete work is `running` unless an
outcome is unknown. Terminal run outcomes have this precedence:

1. `unknown` when an outcome or termination remains uncertain.
2. `cancelled` when operator cancellation interrupted work and termination is certain.
3. `failed` for a terminal step failure or a timeout with certain termination.
4. `succeeded` when all steps succeeded.

Use the existing [reason-line convention](#independent-run-evidence) for causes.
The optional drop records below add three reason names without an observation
state machine.

### Optional drop records and deferral

An adapter declaring `drop_records: true` MUST record every candidate discarded
without launch, grouped once per nonempty reason in a committed Plan call.
Candidates are formed after revision filtering; old pending removed by revision
replacement, deletion or exclusion is not a drop. Fresh catchup drops after a
replacement's preserved horizon still count. Excluded/deleted calls have none.

| Reason | Meaning |
| --- | --- |
| `catchup_missed` | Removed by latest, skip grace, count or age policy. |
| `overlap_skipped` | Removed at an occupied forbid gate under the rule below. |
| `paused_passed_over` | Reserved for discarded pause windows when pause controls exist. |

For each committed group, the adapter MUST allocate a fresh run UID and record
`status: cancelled`, no `started_at`, and `ended_at` at the decision clock.
Use the newest dropped instant's occurrence key. Set `summary.version: 1` and
start `summary.message` with `reason: <code>`, followed on the second line by
`<count> occurrences <oldest>..<newest>` in UTC RFC3339Nano, preserving fractions.
When run summary v2 is advertised, its `reason` member MAY be used instead.
These three shared names extend the existing reason-line convention; Kata
still does not parse that line.

Pair job and definition-event references with initial `expected_revision: 0`.
Atomically persist the checkpoint and entire observation intent before delivery.
Retries MUST preserve UID, attribution, key, summary and original timestamp.
The kit checks the pure decision, not durability or delivery.

`forbid_deferral` defaults to `queue`. Adapters SHOULD select
`drop_after_grace`: after catchup, while `forbid` is occupied (`active_runs > 0`,
including unresolved unknown), discard selected instants whose age is strictly
greater than grace plus tolerance as `overlap_skipped`. Equality remains pending.
With a free gate, launch the oldest retained instant regardless of age. This
samples current occupancy only: equality followed by a free gate after grace
can launch late. With grace and tolerance zero, only overdue work at a still
occupied gate is discarded. Historical occupancy needs state outside this kit.
Queue remains the default in both fixture v1 and v2. Changing that default
requires a later fixture version.

### Recommended failure alerts

For scheduled runs, adapters SHOULD alert the job's `author` as follows.
Manual runs report to their caller. Kata provides no alert route; delivery
belongs to the adapter and is not fixture-checked.

| Outcome | Alert and deduplication |
| --- | --- |
| `failed` or `unknown` | Immediately once per run UID, using one shared adverse-outcome key. Later revisions neither repeat nor retract the alert. |
| `cancelled` with `catchup_missed` | One grouped count per job per sweep, keyed by job and a stable adapter-local sweep/batch ID retained across replay. |
| `cancelled` with `overlap_skipped` or `paused_passed_over` | None; retain run-list evidence. |
| Operator cancellation, succeeded or running | None. |

Alerts are best effort. Log an alert sink error once and drop it; never block
run recording. Log-and-drop does not guarantee receipt.

### Check a Go adapter

Implement `go.kenn.io/kata/pkg/cron/conformance.Adapter` around the runtime's
pure `Plan` and `Settle` decisions. Public DTOs carry job definition JSON,
clock, checkpoint, local occupancy and timestamped offered attempts. Skipped
offers omit dependency-timing checks; structural validation still applies.
These DTOs do not expose internal Kata types or launch an executor.

```go
func TestAdapterProfile(t *testing.T) {
    conformance.Run(t, adapterDecisionBridge{})
}
```

Replace `adapterDecisionBridge` with your implementation. `Declare` selects:

| Member | Required value or range |
| --- | --- |
| `name` | Nonblank implementation name. |
| `tolerance_seconds` | 0–120; zero when omitted. |
| `catchup_limit` | 0/unlimited or at least 100; zero when omitted. |
| `catchup_window_seconds` | 0/unlimited or 86,400–9,223,372,036; zero when omitted. |
| `failure_policy` | Required: `continue` or `stop`. |
| `drop_records` | Optional boolean, default false. |
| `forbid_deferral` | Omitted means queue; explicit `queue` or `drop_after_grace`. Empty/null rejects. |

Schema `kata.cron.adapter-conformance/v1` includes seven default bundles with
54 frozen scenarios. `Scenarios()` returns fresh default vectors; `Load(raw)` strictly decodes a v1
bundle supplied by the caller. V1 does not
exercise successful dependent-step execution: an adapter that always skips
dependents can pass v1. Passing v1 therefore does not establish that workflow
progression works. Choose explicit fixture v2 for that coverage.
`Suite(ctx, declaration)` keeps a bundle's literal expectations only when its
policy matches. The seven default bundles use zero tolerance, count 1000,
unlimited age, `continue`, recording off and `queue`. The optional drop bundle
uses those defaults except window 86,400, `drop_records: true` and
`forbid_deferral: "drop_after_grace"`. Other bundle-policy combinations use
`Reference`-generated expectations and policy boundaries. Implementation names
do not affect matching; omitted deferral and explicit `queue` are equivalent.

`Reference` is deterministic and offline; running it alone does not
test an adapter. Enumeration stops with an error beyond 100,000 new instants,
never silently discarding unexamined work. After acknowledging that gap,
rebuild the checkpoint at now with current revision and no pending to resume.
Built-in suites and CLI export/check cannot materialize `catchup_limit` above
100,000. Such declarations remain valid. To check them from Go, call `RunSuite`
with a hand-built `FixtureSuite` carrying the adapter's declaration and a
smaller candidate trace.

Fixture version `kata.cron.adapter-conformance/v2` retains those v1 decisions
and adds five workflow cases: successful and waiting dependencies, multiple
successful parents, and blocked or waiting multi-parent dependencies. Their
expected decisions are independent literals under both `continue` and `stop`.
These finite fixed vectors remain literal oracles when declaration settings
change; older policy-sensitive vectors use reference-materialized nondefault
answers. Tied-terminal and stop-skipped offer timing requirements remain normative;
`Reference` unit tests enforce them, but these exported literals do not pin them.
Use Go `RunSuite` with a hand-built `FixtureSuite` to check additional boundary
inputs. Selecting v2 changes the fixture suite, not stored cron definitions.

To check v2 from Go, select the suite explicitly:

```go
suite, err := conformance.NewSuite(t.Context(), conformance.SchemaV2, adapter.Declare())
if err != nil {
    t.Fatal(err)
}
conformance.RunSuite(t, adapter, suite)
```

`Run(t, adapter)`, `Suite`, `Check`, and `ExportFiles` retain their v1 defaults.
`NewSuite` and the suite's `Check` and `ExportFiles` methods share version-aware
comparison and export with the CLI. Unsupported versions are rejected.

Sequential decisions pass the actual previous checkpoint (`use_checkpoint`)
or settlement occupancy (`use_occupancy`) across intervening decisions.
`Execute` exposes this driver; `Check` uses the same comparisons as the harness
and CLI. Expected answers must never substitute for actual adapter state.

With recording declared, implement `DropRecorder.PlanWithDrops(ctx, input)`;
the harness calls it once instead of Plan. Without the declaration it is
ignored. Recording and deferral are independent switches. Recording adds an
eighth `drops.json` bundle and a required non-null `drops` array to each plan
output, including `[]` for none. Groups are ordered by reason, unique, and have
ascending nonempty unique instants. Settle outputs never carry drops.

Default export bytes and output shapes remain frozen. A conformance claim
names the fixture schema version; changing frozen outputs requires a new one.

### Check another runtime offline

```sh
kata cron conformance export --dir ./adapter-vectors
kata cron conformance check --answers ./answers.jsonl
# Stronger workflow coverage is explicit; v1 remains the default.
kata cron conformance export --schema kata.cron.adapter-conformance/v2 --dir ./adapter-vectors-v2
```

These commands need no project, daemon or network. Export writes bundles plus
`manifest.json` and refuses existing destinations. Supply a complete declaration
with `--declaration ./declaration.json` for other policy choices. Use `-` for
standard input with either `--declaration` or `--answers`; input is capped at
16 MiB and cancellable while reading a file or standard input pipe.

Check selects the suite from the answer header. Use the exact exported schema;
there is no fallback between versions. V2 export adds `workflow.json`, and every
bundle and the manifest declare v2. The answers file starts with a
schema/declaration header:

```json
{"schema":"kata.cron.adapter-conformance/v1","declaration":{"name":"example-adapter","tolerance_seconds":0,"catchup_limit":1000,"catchup_window_seconds":0,"failure_policy":"continue"}}
```

Each following JSONL line is `{ "name": "scenario/name", "outputs": [...] }`,
with one actual output per ordered decision and exactly one answer per scenario.
Each output object requires every member below, including empty arrays, empty
`excluded`, and `occupied: false`. Members must not be null. Unknown members,
wrong types and missing required members are malformed input. Nested nulls are
also invalid, including attempt-count map values and array elements; use numeric
zero for an unconsumed attempt.

| Output | Required wire members |
| --- | --- |
| Plan | `launch`: ascending instant array; `checkpoint`: object below; `excluded`: string below. |
| Checkpoint | `through`: enumeration instant; `definition_event_uid`: revision identity; `pending`: ascending instant array. |
| Settle | `attempts`: map of step keys to integer counts; `skipped`: step-key array; `status`: run status; `occupied`: boolean. |
| Recorded plan | All plan members plus `drops`: non-null array of groups with `reason` and nonempty ascending `instants`. |

`excluded` is empty when included, or `deleted`, `disabled`, `notify`, `manual`,
`other_adapter`, or `once_past` (one-shot instant at or before creation).
Settle status is `running`, `succeeded`, `failed`, `cancelled`, or `unknown`.

The CLI report is
`{kata_api_version:1,schema,passed,failed:[{name,diff}]}` as JSON; the Go
`Report` has the last three members. Equivalent RFC3339 offsets compare as the
same instant. Valid decisions that differ from expected values, missing,
duplicate and unknown scenario answers exit 1. Malformed input, unsupported
schemas and invalid declarations exit 3. JSONL must have no blank lines; the
usual final newline is allowed, but additional empty lines are rejected.

The kit does not certify process termination, observation delivery, alerts,
issue-date readiness or cross-runtime arbitration. Issue-date triggers retain
[existing adapter rules](#adapter-responsibilities). Keep job/installation
identity, frozen definition references, run UIDs and occurrence keys stable in
production; this offline kit does not inspect those I/O paths.

## Adapter responsibilities

Adapters must exclude `notify` jobs with `issue.kind: "per-run"` from their
schedulers; those jobs belong to the native hub. Existing strict decoders reject
the new `parent_uid` shape instead of scheduling it.

Shared `enabled` configuration does not activate an adapter installation. Each
adapter owns its local activation, scheduler, overlap and catchup handling,
checkout and secret mappings, process lifecycle, and conversation recovery.
Adapters claiming the [conformance profile](#adapter-conformance) follow its
scheduling, overlap, catchup and outcome rules; other adapters retain their own
rules.
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
configured trigger timezone. Adapters claiming the profile follow its
[embedded timezone precedence](#eligibility-and-schedule-instants). Issue-date
triggers use the planning-date instant above: issue timezone, recurrence
fallback, daemon default and UTC remain
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
[tern-kata](https://github.com/Dangerboi/tern-kata/blob/main/docs/guide/worker.md)
is another: its worker runs prompt and workflow jobs through
[omp](https://github.com/can1357/oh-my-pi) on the user's machine; its guide
covers setup.

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
