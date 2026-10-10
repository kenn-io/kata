---
title: Native cron
description: Shared job and workflow definitions and attributed run evidence.
last_edited: 2026-10-09
---

Kata stores portable job and workflow definitions and independent run observations.
External adapters decide when to run, resolve checkout and provider configuration,
create or update ordinary issues, deliver notifications, and launch processes.
An observation records evidence; its status does not control whether anything runs.

## Vocabulary

| Name | Meaning |
| --- | --- |
| Job | Configured work: its trigger, action, and execution settings. |
| Workflow | Reusable command or prompt steps and their dependencies. |
| Run | One execution and its reported status and results. |

A job can invoke a workflow or supply a single prompt. Each execution has its
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
require the current `expected_event_uid`, either as the flag or in the request
body; when both are present they must match. Conflicts require inspecting the
current definition. A lost create response can be inspected using the retained
UID. A repeated create does not silently become an update. Tombstones remain
readable, and `list --include-deleted` includes them. Deleting a workflow is
refused while a live job in the project uses it; update or delete that job
first.

Enabling a job changes shared configuration; it creates no timer or process.
Secret references are names, and checkout keys are portable names. Credentials,
host paths, and runtime handles are not part of this shared contract. Opaque
option objects retain exact JSON numbers.

### Request files

The `--file` body for `create` and `update` is a JSON object with a `name` and
a `definition`. Kata trims `name`, which must then be 1–256 bytes. Optional `actor` must
match `--as` when both are given; optional `expected_event_uid` applies to
`update` only. A job request file looks like this:

```json
{
  "name": "Weekday review",
  "definition": {
    "version": 1,
    "kind": "job",
    "enabled": true,
    "trigger": {"kind": "cron", "cron": "0 9 * * 1-5", "timezone": "America/New_York"},
    "action": {"kind": "execute", "prompt": "Review open pull requests and summarize risks."},
    "issue": {"kind": "per-run", "title": "Weekday review", "deadline_offset_seconds": 28800},
    "overlap": "forbid",
    "catchup": "skip",
    "checkout_key": "main-repo",
    "timeout_seconds": 3600,
    "secret_refs": {"GITHUB_TOKEN": "github-review"}
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
| `issue` | Issue policy; see [issue policies](#issue-policies). Required for `execute`. |
| `overlap` | Required; `forbid` or `allow`. |
| `catchup` | Required; `skip`, `latest`, or `all`. |
| `checkout_key` | Optional portable name. Not allowed with `notify`. |
| `grace_seconds`, `timeout_seconds` | Optional; zero or greater. |
| `secret_refs` | Optional object mapping portable names to portable names. |
| `options` | Optional JSON object for the adapter. |

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

### Actions

| `kind` | Fields and rules |
| --- | --- |
| `execute` | Exactly one of `prompt` (not blank) or `workflow_uid` (a workflow ULID). Requires an `issue` policy. Cannot use an `issue-deadline` trigger. Cannot carry `recipient` or `message`. |
| `notify` | `recipient`: an exact inbox address of at most 128 bytes, such as `worker` or `worker/adapter`, or `current-owner-or-author`. `message`: at most 1024 bytes; required unless `recipient` is `current-owner-or-author`. Cannot carry `prompt` or `workflow_uid`, and the job cannot set `checkout_key`. |

A `notify` job with an `issue-scheduled` or `issue-deadline` trigger targets
that issue and may omit `issue`. Any other `notify` job requires an `issue`
policy of kind `existing`.

### Issue policies

| `kind` | Fields and rules |
| --- | --- |
| `existing` | `uid`: the ULID of an existing issue. No other fields. |
| `per-run` | `title` (not blank) and optional `body`. Optional `scheduled_offset_seconds` and `deadline_offset_seconds` are relative date offsets, in seconds, for the issue the adapter creates. No `uid`. |

`per-run` is valid only with `execute`.

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
  "name": "Review then test",
  "definition": {
    "version": 1,
    "about": "Review a change, then run the tests.",
    "steps": [
      {"key": "review", "kind": "prompt", "prompt": "Review the change."},
      {"key": "test", "kind": "command", "command": "make test", "after": ["review"], "retries": 1}
    ]
  }
}
```

## Capabilities

`kata cron capabilities --json` reads the project's UID and advertised
`event_features` without changing state. The corresponding GET is
`/api/v1/projects/{project_id}/cron/capabilities`; the
`X-Kata-Event-Features` header advertises `cron_v1`. Missing, unknown, or
failed advertisements remain distinct outcomes. This is an ordinary project
read and creates no permissions. Ordinary authentication, project isolation,
and issue-scoped route restrictions apply.

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

## Adapter responsibilities

Shared `enabled` configuration does not activate an adapter installation. Each
adapter owns its local activation, scheduler, overlap and catchup handling,
checkout and secret mappings, process lifecycle, and conversation recovery.
A second adapter may independently execute the same occurrence. Each execution
keeps a separate run UID; no shared observation reserves the issue or occurrence.
Local transcripts, process IDs, session handles, credentials, and delivery
buffers are not native run evidence. An adapter may buffer a failed observation
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

SQLite and PostgreSQL schema 31 store native cron data in `cron_jobs`,
`cron_workflows`, and `cron_runs`. JSONL export and import retain shared
definitions, including tombstones, and bounded run evidence. Runs with
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
