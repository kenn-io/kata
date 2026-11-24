---
title: Cron
description: Recurring child issues, shared job definitions, and attributed run evidence.
last_edited: 2026-11-20
---

Kata creates recurring child issues and inbox requests from enabled native notification jobs, and stores portable job definitions, workflow definitions, and independent run observations.

For native notification jobs, the owning hub daemon handles delivery, and consumers read the ordinary inbox and decide what to do. For other jobs, external adapters schedule the work, resolve checkout and provider configuration, create or update ordinary issues, deliver notifications, and launch processes. Run observations record evidence only; an observation's status does not control whether anything runs.

You can browse jobs, runs, and workflows for the current project in `kata tui` using the `2`, `3`, and `4` tabs. From the **Jobs** and **Workflows** tabs, you can also create and edit jobs and workflows. For shortcuts, see the [`kata tui` keys](cli.md#daemon-and-diagnostics).

For a run organized as dependent issues with human approval, see [Gated workflows](../workflows/gated-workflows.md).

## Vocabulary

| Name | Meaning |
| --- | --- |
| Job | Configured work: its trigger, action, and execution settings. |
| Workflow | Reusable command or prompt steps and their dependencies. |
| Run | One execution and its reported status and results. |

An execution job can invoke a workflow or supply a single prompt, and each execution has its own run UID. A run can also reference a workflow directly.

Issue responsibility uses Kata's existing `claim`, `assign`, `unassign`, and owner vocabulary, while a federation lease provides exclusive coordination on an issue. Recording a run changes neither ownership nor leases. For details on leases, see [federation leases](../operations/federation.md#leases-and-write-gates).

## Definitions

Both `kata cron job` and `kata cron workflow` support `create`, `list`, `show`, `update`, `edit`, `delete`, and `restore`. These commands return JSON, and creation retains a ULID:

```sh
kata cron job create --uid <job-uid> --file job.json --json
kata cron job list --json
kata cron workflow create --uid <workflow-uid> --file workflow.json --json
kata cron workflow list --json
kata cron job update <job-uid> --expected-event-uid <event-uid> --file replacement.json --json
kata cron job delete <job-uid> --expected-event-uid <event-uid> --json
```

You do not pass an expected event UID when creating a definition. Updates, deletion, and restoration require the current `definition_event_uid` returned by `show`, passed either as `--expected-event-uid` or as `expected_event_uid` in the request body. When both are present, they must match. A stale value fails with a conflict. Re-read `show` before retrying an edit.

After an uncertain create response, inspect `show` with the retained UID. If you retry creation, reuse that same `--uid`; an already-created definition conflicts and never becomes an update. Tombstones remain readable, and `list --include-deleted` includes them. Kata refuses to delete a workflow while a live job in the project uses it. Update or delete that job first.

Enabling a native recurring issue job activates delivery by the owning daemon. Other jobs require an adapter installation to schedule them, and no job launches a process inside Kata. Secret references are names, and checkout keys are portable names. Credentials, host paths, and runtime handles are not part of this shared contract. Opaque option objects retain exact JSON numbers.

### Create with typed flags

For common per-run jobs, you can pass typed flags to `kata cron job create` instead of `--file`. Choose the job's enabled state and policies explicitly. This example stores a disabled native job; enabling it activates delivery by the owning daemon:

```sh
kata cron job create --name "Review" --title "Review check-in" \
  --enabled=false --catchup latest --overlap allow \
  --every 1h --notify --parent <parent-ref> --recipient worker --json
```

An adapter prompt job also requires an explicit adapter selector:

```sh
kata cron job create --name "Inspection" --title "Inspect" \
  --enabled=false --catchup latest --overlap forbid \
  --every 1h --prompt "Inspect the workspace" --adapter example-adapter --json
```

| Flags | Meaning |
| --- | --- |
| `--name`, `--title` | Required job name and per-run issue title. |
| `--enabled` or `--enabled=false` | Required explicit enabled state. |
| `--catchup`, `--overlap` | Required [job policies](#job-definitions); native jobs require `overlap=allow`. |
| `--cron`, `--every`, `--once` | Exactly one trigger: cron expression, positive whole-second duration, or RFC 3339 instant. Native jobs accept cron or intervals of at least 60 seconds. |
| `--timezone` | Required IANA timezone for `--cron`. Optional for interval and once triggers. `Local`, invalid names, and explicitly empty values are rejected. |
| `--notify`, `--parent`, `--recipient` | Native recurring child job with an explicit parent ref and inbox recipient. |
| `--prompt`, `--adapter` | Literal prompt and explicit `options.adapter` selector for an adapter job. |
| `--body`, `--message` | Optional literal issue body and native inbox message. `--message` requires `--notify`. |
| `--adapter-agent`, `--checkout-key` | Optional adapter agent selector and portable checkout key. Both require `--prompt`. |
| `--grace`, `--timeout` | Optional nonnegative whole-second durations. Native `catchup=skip` needs positive grace; native jobs cannot carry an execution timeout. |

For example, you can replace `--every 1h` with `--cron '0 9 * * 1-5' --timezone UTC` for weekday mornings, or use `--once 2026-12-01T09:00:00+05:30` on an adapter job. Typed `--cron` uses `--timezone` as its single timezone source, while `TZ=` and `CRON_TZ=` directives remain available through `--file`. Use `--every` for an interval rather than the `@every` descriptor. An interval measures elapsed time, and omitting a timezone on an interval does not choose the machine's local zone. A once trigger requires an explicit offset or `Z`. Durations must be whole seconds, and Kata ignores digits beyond nanosecond precision.

Prompt, body, and message text stay literal. Kata does not parse shell words or launch a process. `options.adapter` selects an adapter by convention and does not confirm that the adapter is installed or ready. Typed creation saves a job without previewing occurrences or reserving execution.

Reusing a name creates a separate job. To update a job, use a request file or the interactive editor.

### Edit in place

Open an existing definition in your editor with `kata cron job edit <job-uid>` or `kata cron workflow edit <workflow-uid>`. Both stdin and stdout must be terminals. Kata checks `$VISUAL`, then `$EDITOR`, and falls back to `vi`, splitting editor arguments on whitespace without shell expansion.

The buffer contains `name` and `definition`. Kata owns the actor and revision, and you can use `--as` for attribution. Kata ignores only the consecutive leading `//` header. An empty body aborts the command, and an unchanged definition saves nothing. Local or daemon validation errors reopen the draft with the error; leaving an invalid body unchanged stops the command. You must restore a deleted definition before editing it.

Kata saves with the `definition_event_uid` captured when the editor opened. If another writer changes the definition, the header shows their changes and a `CONFLICT` line. Review that diff and delete the conflict line to save your draft over the displayed revision. Each further conflict requires acknowledgment again. Kata does not merge definitions automatically.

When an edit fails, Kata retains a private draft and prints its path, and the header records the exact expected event for manual recovery. Inspect `kata cron job show <job-uid>` before retrying. Strip only the leading header into a new file, then pass the retained event, substituting `workflow` for a workflow draft:

```sh
awk 'BEGIN { header = 1 } header && /^\/\// { next } { header = 0; print }' "$draft" > replacement.json
kata cron job update <job-uid> --file replacement.json --expected-event-uid <retained-event-uid> --json
```

If an intervening write occurred, that guarded retry conflicts. Review the current definition before choosing a newer expected event. A fresh `edit` reads a new base and does not resume the retained draft. If output fails after an accepted save, Kata reports that the definition was saved and keeps the draft for inspection.

### Request files

For `create` and `update`, the `--file` body requires both `name` and `definition`. Updates replace the complete definition rather than patching it. Kata trims `name`, which must then be 1 to 256 bytes. When both are given, optional `actor` must match `--as`. Optional `expected_event_uid` applies to `update` only. The `create` and `update` help commands include copyable request bodies:

```sh
kata cron job create --help
kata cron job update --help
kata cron workflow create --help
kata cron workflow update --help
```

For an adapter prompt job, save this request as `job.json` and configure the `example-project` checkout in the adapter. `options.agent` selects its prompt agent. Option names and supported agents depend on the adapter, and Kata stores these options without interpreting them.

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

Kata decodes definitions strictly, rejects unknown fields, and bounds each definition to 256 KiB. A *portable name* is 1 to 256 bytes of letters, digits, `-`, `_`, and `.`, and cannot be `.` or `..`.

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

At any depth inside `options`, Kata rejects these keys regardless of case: `token`, `api_token`, `api_key`, `password`, `secret`, `credentials`, `authorization`, `cwd`, `path`, `checkout_path`, and `local_path`.

### Triggers

Every trigger has a `kind` and accepts an optional IANA `timezone`, such as `America/New_York`. A trigger cannot carry fields that belong to another kind.

| `kind` | Required fields |
| --- | --- |
| `manual` | None. |
| `cron` | `cron`: a five-field expression (minute, hour, day of month, month, day of week) or a descriptor such as `@daily`. |
| `interval` | `interval_seconds`: a positive integer. |
| `once` | `at`: an RFC 3339 instant. |
| `issue-scheduled` | `issue_uid`: the ULID of the issue whose `scheduled_on` date fires the job. |
| `issue-deadline` | `issue_uid`: the ULID of the issue whose `deadline_on` date fires the job. Optional `lead_seconds` (zero or greater) is how long before the deadline to fire. |

### Cron expressions

A `cron` expression has five fields: minute, hour, day of month, month, and day of week. You can also use a descriptor such as `@hourly`, `@daily`, `@weekly`, `@monthly`, or `@yearly`. Kata does not accept `@reboot`, seconds, years, or `L`, `W`, `#`, and `H` syntax, and it keeps the expression exactly as written.

- Day of week accepts `0` or `7` for Sunday, as well as names such as `MON`. `0 9 * * 7` runs on Sundays, `5-7` means Friday through Sunday, and `5-7/2` means Friday and Sunday.
- When you restrict both day of month and day of week, a day matching either field runs. `0 9 15 * 1` runs on the 15th and on every Monday.
- Ranges can wrap around. `0 22-2 * * *` runs at 22:00, 23:00, 00:00, 01:00, and 02:00.
- A step must be smaller than the span it steps over. Kata rejects `0 */24 * * *` and `*/60 * * * *`; write `@daily` or `@hourly` instead.
- A step starting at one weekday continues through 7, which is Sunday. `0 9 * * 5/2` runs on Fridays and Sundays.
- A schedule must fire. Kata refuses a day of month that never occurs in the chosen months, such as `0 0 30 2 *` for February 30, and rejects the `@manual`, `@triggered`, and `@none` descriptors. `0 0 29 2 *` is accepted. If an older release saved a job with such a schedule, it still opens in `kata cron job edit <job-uid>` so you can fix it.

When Kata rejects an expression, the error says why. For `L`, `W`, `#`, or `H` syntax, it names the unsupported feature. Otherwise it shows the parser's message and adds a note on the five-field format, `@reboot`, or `@every` when one applies. Adapter jobs accept `@every`, while native jobs use an interval trigger as described in [Recurring child issues](#recurring-child-issues).

Schedules follow the trigger timezone's daylight-saving transitions:

- When clocks jump forward an hour, a time inside the skipped hour runs one hour later that day. In `America/New_York`, `30 2 * * *` runs at 03:30 EDT, or 07:30 UTC, on 2026-03-08.
- When clocks fall back, a time inside the repeated hour runs on both passes. In `America/New_York`, `30 1 * * *` runs at 05:30 and 06:30 UTC on 2026-11-01. Hourly schedules run once per elapsed hour through the repeat.
- On transition days in zones that shift by 30 minutes, such as `Australia/Lord_Howe`, or zones that skip local midnight, such as `America/Santiago`, a daily schedule can miss that day's occurrence.

The `kata tui` **Jobs** tab previews these instants with the same schedule code. See the `kata tui` notes in the [CLI reference](cli.md#daemon-and-diagnostics).

Kata releases up to 0.19.0 used a parser that rejects `7` in day of week and wrapping ranges, accepts steps as large as their span, and reads `5/2` as Friday only. Before writing an expression that uses one of these forms, upgrade every hub, spoke, and adapter with its own parser that reads the job. Older peers reject the definition or schedule it differently.

### Actions

| `kind` | Fields and rules |
| --- | --- |
| `execute` | Exactly one of `prompt` (not blank) or `workflow_uid` (a workflow ULID). Requires an `issue` policy. Cannot use an `issue-deadline` trigger. Cannot carry `recipient` or `message`. |
| `notify` | `recipient`: an exact inbox address of at most 128 bytes, such as `worker` or `worker/adapter`, or `current-owner-or-author`. `message`: at most 1024 bytes; required unless `recipient` is `current-owner-or-author` or this is a native recurring issue job. Cannot carry `prompt` or `workflow_uid`, and the job cannot set `checkout_key`. |

A `notify` job with an `issue-scheduled` or `issue-deadline` trigger targets that issue and can omit `issue`. Any other `notify` job requires an `issue` policy of kind `existing` or the native recurring `per-run` policy below.

### Issue policies

| `kind` | Fields and rules |
| --- | --- |
| `existing` | `uid`: the ULID of an existing issue. No other fields. |
| `per-run` | `title` (not blank) and optional `body`. Optional `scheduled_offset_seconds` and `deadline_offset_seconds` are relative date offsets, in seconds, for the issue the adapter creates. No `uid`. |

For `notify`, `per-run` instead requires `parent_uid` and a nonblank `title`. It accepts an optional `body`, with no `uid` or date offsets. This selects native recurring child delivery. Execution jobs do not accept `parent_uid`.

### Recurring child issues

To set up recurring child issues, keep an ordinary parent issue open for the continuing commitment, then create a job with `--parent <ref>`. For example, save this request as `job.json`:

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

The CLI resolves the ref and stores its full UID in `definition.issue.parent_uid`. Because the stored definition stays UID-only, short ID growth and project renaming do not change its parent identity. API clients can still supply `parent_uid` in the request. If the file supplies it alongside `--parent`, both must name the same UID. `--parent` requires `issue.kind: "per-run"`, and execution jobs cannot carry a parent binding.

When no project is otherwise selected, the job uses the parent's project. An explicit `--project` or workspace binding must match that project. Job updates also accept `--parent` together with the current `--expected-event-uid`, while a native job's existing parent binding remains immutable.

Each due occurrence creates one ordinary child with a parent link and an inbox request, and earlier children can remain unfinished. At delivery, the child copies the parent's current owner; an unowned child remains unowned. Routing follows that owner, or the parent author when unowned. To keep routing fixed, set an exact recipient such as `worker/consumer`. If you omit the message, Kata uses its default recurring notification message. Read requests with `kata inbox --for worker/consumer`, then clear the request with `kata notify <child-ref> --to worker/consumer --clear` after handling it. Reading does not acknowledge it.

Kata appends a UTC occurrence timestamp to the child title. The configured title can be at most 991 characters, so the child title stays within 1024 characters. Children carry `cron.occurrence` provenance and no `scheduled_on`, so the ordinary date sweeper does not request the same occurrence again. Clearing a request, closing a child, or restarting the daemon never rewinds delivery. Native delivery creates no run observation, and a consumer can record its own execution evidence separately.

Native jobs accept only `cron` and `interval` triggers and require `overlap: "allow"`. Calendar expressions use the trigger's IANA timezone, with UTC as the default, and follow that timezone's daylight-saving transitions as described in [Cron expressions](#cron-expressions). An embedded `TZ=` or `CRON_TZ=` must agree with `timezone`. For an elapsed two-hour cadence, use `{"kind":"interval","interval_seconds":7200}`. Intervals must be at least 60 seconds, and native delivery does not support `@every`. Intervals start at the first binding time plus one interval, while calendar schedules start strictly after that binding time without historical backfill.

| `catchup` | After downtime or a pause |
| --- | --- |
| `all` | Create every missed child, at most 1000 per job per sweep. Later sweeps continue from durable progress. |
| `latest` | Create only the newest missed occurrence. Calendar scans examine at most 20,000 occurrences per sweep, retain a fixed scan horizon, and deliver when that scan finishes. Intervals find the newest occurrence directly. |
| `skip` | Require positive `grace_seconds`; discard older occurrences and deliver those within the grace window, with the same bounded sweep limits. |

The existing daemon sweep runs at startup and every 30 seconds by default. Keep the authoritative hub running for unattended delivery. Local projects deliver locally, while federated projects deliver only on their enabled owning hub. Spokes never deliver.

Direct spokes with push enabled can create, update, delete, and restore native recurring jobs through their existing enrollment. For each request, the spoke checks hub support, pushes pending local events including a newly created parent, and forwards the write synchronously. The hub validates the job, preserves its progress anchor, and attributes the write to the enrollment's bound actor. The complete preflight and write have a 15-second limit, and no offline write is queued. Relay, disabled, and push-disabled bindings retain the refusal. If forwarding is unavailable, run the same command against the project's owning hub, for example with `--daemon <hub-name>`. Upgrade an older hub to advertise native cron authoring; `cron_v1` alone does not advertise that capability.

You can use the returned `definition_event_uid` right away to authorize the next update or lifecycle action, even before pull. One exception applies: when the hub converted an adapter job to native and the spoke has not pulled that change yet, the spoke still routes delete and restore for that job locally and reports a conflict. Wait for the next pull, or run the action against the hub. Local `show` and `list` catch up through normal replication. Forwarded jobs report the local project ID and zero job `id` and `revision` until a local projection exists, and response events are omitted. If a write response is lost, the outcome can be unknown. Inspect the job on the hub or retry creation with the same `--uid`; an already-created UID returns a conflict containing the hub's current job. HTTP callers should prefer stable-UID PUT creation because POST allocates a UID that can be lost with the response.

An old peer that lacks `cron_v1` cannot receive a project containing cron state, including native recurring metadata on ordinary issue events, and capability refusal happens before a baseline or cursor change. Projects with native recurring delivery history cannot be adopted into a different hub through spoke snapshots; the refusal preserves their existing local authority and history.

Create, update, delete, and restore retain ordinary authorization and expected revision checks. Once bound, a job's parent cannot change and the job cannot become an adapter job. Edits preserve committed progress and the original interval anchor, while an unfinished latest scan restarts against the new definition.

To pause delivery, disable the job or close its parent. An archived project or a deleted parent also pauses delivery. Re-enabling, reopening, or restoring resumes the configured catch-up policy. A pause does not skip occurrences: with `catchup: "all"`, resuming creates every child missed while paused, including the time since creation for a job created disabled. Use `latest` or `skip` when a pause must not backfill. The TUI's [reopen prompt](cli.md) counts every missed occurrence, not only the 1000 that one sweep delivers. Deleting the job stops delivery until restoration.

Keep the parent in the job's project. Kata rejects moving the parent alone while its native job exists, including when the job is disabled or deleted. Restoring the job can resume its binding. To move the job and parent together, merge projects.

Child creation, its initial notification, and the parent's `cron.progress.<job-uid>` checkpoint commit together. Each delivery therefore changes the parent's revision and update time; re-read the parent before a conditional edit. Backup, restore, federation, and project merge preserve this metadata. Delivery fails closed if progress is missing or malformed, and ordinary issue writes and spoke ingest cannot change or delete progress or occurrence provenance. Recurrence templates also reject these system keys, including when older templates are materialized. Inspect that state before recovery; there is no implicit reset that could recreate already delivered children. Native jobs reject execution timeouts, secrets, adapter options, checkout keys, and issue date offsets.

### Workflow definitions

A workflow definition has `version` `1` and a non-empty `steps` array. It can also carry `about`, `input`, and `options`, following the same key rules as job options. Each step has these fields:

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

To run commands from a job, reference a workflow. Keep `action.kind` as `execute`, use `workflow_uid` instead of `prompt`, and put each command in a workflow step. The referenced workflow must already exist in the same project.

Save the workflow above as `workflow.json` and create it with a retained UID:

```sh
kata cron workflow create --uid 01J00000000000000000000010 --file workflow.json --json
```

The UID here is an example. Choose your own ULID and use it in both the create command and the following job body. Save this request as `command.json`:

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

The external adapter schedules the job and runs its workflow in the configured checkout, while Kata stores the definitions and run evidence.

## MCP recurring inbox example

This example uses a synthetic `example-agent/worker` inbox and a receiver at `https://receiver.example/inbox`. It assumes the MCP server already runs with [MCP Events](../operations/mcp-events.md) enabled on the daemon, and that the receiver implements signed challenge verification and receipt deduplication. The calls below are MCP calls on the same endpoint, and the receiver uses its configured signing secret.

Load `kata.load_issue_mutation`, `kata.load_issue_discovery`, `kata.load_coordination`, and `kata.load_cron`. Create an ordinary parent with `kata.create` and keep `issue.uid` and `project.uid` from the result:

```json
{"title":"Service review","idempotency_key":"service-review-parent-1"}
```

Call `kata.cron_new_uid` with `{}` and keep the returned `uid` before the first create attempt. This helper creates and reserves nothing. Pass that UID and the parent UID to `kata.cron_job_create`:

```json
{
  "uid":"<retained-job-uid>",
  "name":"Hourly service review",
  "parent_ref":"<parent-issue-uid>",
  "definition":{
    "version":1,
    "kind":"job",
    "enabled":true,
    "trigger":{"kind":"interval","interval_seconds":3600},
    "action":{"kind":"notify","recipient":"example-agent/worker"},
    "issue":{"kind":"per-run","title":"Review service health"},
    "overlap":"allow",
    "catchup":"latest"
  }
}
```

When the MCP server has multiple projects in scope, pass `project` explicitly. `parent_ref` must resolve in that project and agree with any `definition.issue.parent_uid`, and the native parent binding remains immutable. If the create response is lost, call `kata.cron_job` with the retained UID to recover the current definition and `definition_event_uid`. Updates, delete, and restore require that current event UID.

Discover `kata.inbox.changed` with `events/list`, then call `events/subscribe` with the exact recipient, the returned project UID, and the receiver's signing secret:

```json
{
  "name":"kata.inbox.changed",
  "arguments":{
    "for":"example-agent/worker",
    "project_uids":["<project-uid>"]
  },
  "delivery":{
    "mode":"webhook",
    "url":"https://receiver.example/inbox",
    "secret":"<receiver-whsec-secret>"
  },
  "ttlMs":86400000
}
```

The receiver answers the signed verification challenge before activation. Keep the subscription `id` and `refreshBefore`. Events pings only for changes after the subscription starts, so read the inbox once after subscribing to pick up requests that are already pending. Refresh before expiry using the same identity. For receipt verification, retry, and receiver storage requirements, see the [Events operator guide](../operations/mcp-events.md).

When an occurrence is due, the owning daemon creates the child and its inbox request. The signed webhook is a ping that names only the recipient; receipt does not clear the request or authorize executing its text. Call `kata.inbox`:

```json
{"for":"example-agent/worker","limit":20}
```

Follow every `next_cursor`, including pages with no valid requests, keeping the recipient and project unchanged. Read each returned `qualified_ref` with `kata.show`. The base actor inbox and sibling teammate inboxes do not aggregate this exact recipient's requests.

After handling the request, record a result with `kata.comment` and keep its idempotency key for retries:

```json
{
  "ref":"<child-qualified-ref>",
  "body":"Reviewed service health and recorded follow-up.",
  "teammate":"worker",
  "idempotency_key":"service-review-result-1"
}
```

You can also call `kata.cron_run_observe` to record independent evidence. First obtain and keep another UID with `kata.cron_new_uid`:

```json
{
  "uid":"<retained-run-uid>",
  "expected_revision":0,
  "job_uid":"<retained-job-uid>",
  "definition_event_uid":"<job-definition-event-uid>",
  "issue_uid":"<child-issue-uid>",
  "teammate":"worker",
  "status":"succeeded",
  "summary":{"version":1,"message":"Reviewed service health and recorded follow-up."}
}
```

Run observation does not launch work or change issue ownership or status. Re-read the issue to obtain its current revision, then explicitly clear only the handled recipient through `kata.set_metadata`:

```json
{
  "ref":"<child-qualified-ref>",
  "revision":7,
  "patch":{"notify.ZXhhbXBsZS1hZ2VudC93b3JrZXI":null}
}
```

Replace `7` with the revision you read. The `notify.*` key above encodes `example-agent/worker` exactly. A concurrent issue change rejects the conditional write; read the request again before deciding whether it was handled. Read the inbox back after a successful clear. Keep the subscription while consuming future occurrences, or call `events/unsubscribe` with its original name, arguments, and webhook delivery URL when that receiver stops consuming them.

Cron workflows store reusable adapter steps. Execution jobs and workflows need an external adapter, while enabled native notification jobs deliver through the owning daemon. `kata.load_recurrence` is a separate RRULE issue-template interface.

## Capabilities

Run `kata cron capabilities --json` to read the project's UID and advertised `event_features` without changing state. The corresponding GET endpoint is `/api/v1/projects/{project_id}/cron/capabilities`. The `event_features` list advertises only `cron_v1`, covering portable definitions, run observations, and native recurring child delivery. Missing, unknown, or failed advertisements remain distinct outcomes. This is an ordinary project read that creates no permissions, and ordinary authentication, project isolation, and issue-scoped route restrictions apply.

### Upgrade limits

These limits apply to projects with cron data and the adapters that consume it. For ordinary issue synchronization, use the normal [hub-first upgrade procedure](../operations/federation.md#compatibility-and-rolling-upgrades); spokes can upgrade later.

Kata 0.19.0 advertised both `cron_v1` and `cron_notify_children_v1`, while Kata 0.20.0 advertises only `cron_v1` for all cron support, with no alias for the removed name. Fully compatible mixed-version cron operation is not supported across this change:

- A 0.20.0 spoke cannot read a 0.19.0 hub project that requires the removed capability for native recurring-child state. Upgrade the hub first.
- Adapters that require `cron_notify_children_v1` must switch to `cron_v1` when their daemon upgrades.
- Before using the new expression forms, such as Sunday `7` or wrap-around ranges, upgrade every peer and adapter that reads a project's definitions.
- Check existing expressions before upgrading. Kata now rejects steps as large as their field's span. Use `@daily` instead of an hourly `*/24` step, or `@hourly` instead of a minute `*/60` step.

Stored definitions and events stay unchanged, and deleting a cron job does not remove its retained history or the project's cron capability requirement. For the expression rules, see [Cron triggers](#triggers).

## Independent run evidence

To notify an inbox recipient when an observation reports a failure, see [Alert on cron run failures](../guide/cron-failure-alerts.md).

An adapter creates one independent run UID and keeps it across retries:

```sh
kata cron run observe <run-uid> --json-input observation.json --json
kata cron run list --json
kata cron run show <run-uid> --json
```

Run lists default to 100 records. For the defaults on each surface, see [Run list page size](#run-list-page-size).

The HTTP operation is `PUT /api/v1/projects/{project_id}/cron/runs/{run_uid}`, with operation ID `observeCronRun`. If you omit JSON `teammate`, Kata inherits `--teammate` or `KATA_TEAMMATE`, and an explicit empty flag suppresses that inheritance. An explicit JSON string or `null` freezes attribution across retries and ignores the environment. An explicit flag must match the frozen value, where `null` matches an empty flag. An empty JSON string remains invalid and is never replaced by the environment. For example:

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

You must supply at least one paired job and event reference or workflow and event reference, and you can supply both pairs. References identify historical definitions, including earlier versions and tombstones, within the project. An optional issue UID identifies an ordinary existing issue in that project. Distinct run UIDs can share the same occurrence key and issue, and logging a run reserves no occurrence.

UID, project, definition references, occurrence key, issue UID, attributed actor, teammate, executor label, and server-created time are immutable, while status, summary, start time, and end time are mutable evidence. Supported statuses are `running`, `succeeded`, `failed`, `cancelled`, and `unknown`, and they impose no transition or launch protocol. Summary version 1 permits a message and nonnegative integer input and output token counts and is bounded to 64 KiB. The whole observation and its event are bounded to 96 KiB, and Kata rejects unknown fields. Times are RFC3339 instants, and an end cannot precede a start. An occurrence key is at most 1024 characters, and actor and optional executor label are at most 256 bytes. Teammates use Kata's existing 1 to 64 character ASCII names.

A new observation uses expected revision 0, while a changed observation requires its current revision. An identical retry of the same UID and canonical evidence returns `replayed: true`, emits no event, and changes no revision even when its expected revision is stale. A different immutable identity always conflicts. Authenticated actor attribution overrides an untrusted request actor, and Kata rechecks revocation in the write transaction. Logging does not change an issue's owner, dates, lease, status, or readiness.

Responses contain `run`, the exact committed `events`, and `replayed`. Run listing is project-scoped, accepts `--job-uid` to list one job's runs, and orders results by creation time and UID descending. Each run row also carries `job_name`, `workflow_name`, and `issue_short_id`, resolved when the row is read from the current job, workflow, and issue rows. A deleted job, workflow, or issue still names its runs, and a name is omitted when the run has no such reference. Kata does not store, export, or federate these names. Pass `next_before_uid` with `--before-uid` for another page. Ordinary federation publishes observations from spokes, folds evidence by original event provenance per run UID, and preserves independent runs in baselines and backup. Run identity conflicts fail rather than replacing a run.

An adapter with a machine-readable cause for a run's outcome starts `summary.message` with the line `reason: <code>`, then puts any detail on the next line. The `<code>` matches `[a-z0-9_.-]{1,64}`, and any status can carry one. Kata does not parse or validate the reason line; it treats it as ordinary message text under the summary rules above. Clients that know the convention can show the code and filter on it client-side. Each adapter defines and documents its own codes, and where two adapters mean the same thing they share the name, as `agent_quota`, `agent_login`, and `launch_unproven` do. The `reason: ` marker keeps short messages such as `admitted` from reading as codes.

## Native issue planning dates

Run `kata show <issue-ref> --planning-dates --json` or send a GET request to `/api/v1/projects/{project_id}/issues/{ref}/planning-dates` to read one consistent issue revision with required nullable `scheduled_on` and `deadline_on` objects. Each present object contains `field`, raw `value`, resolved `timezone`, and a UTC `instant`. The flat response also contains `project_id`, `issue_uid`, and ordinary `revision`.

Native date parsing applies the issue timezone first, then the recurrence fallback for scheduled dates, then the daemon default timezone and UTC. Deadline dates have no recurrence fallback, and explicit UTC values resolve to UTC. The read uses native civil-time and DST behavior; it evaluates no clock and changes no readiness or persisted state. Normal project and issue-subtree read policy applies.

### Reached schedules

An adapter can start date-driven work from the event stream instead of polling. When an open issue's `scheduled_on` is reached, the daemon's date sweeper records one `issue.scheduled_reached` event by `system`. Its payload is the planning-date object above: `field`, raw `value`, resolved `timezone`, and UTC `instant`.

- The sweeper runs at daemon startup and every 30 seconds, so the event follows the instant by up to 30 seconds. A date that passed while the daemon was down is recorded at the next startup.
- Schedules that were already overdue before the upgrade record no event. The first sweep after upgrading stores its time as the database's floor. The sweeper records a date only if its instant is at or after the floor, or if its current value was set after the floor. Setting a past date after the upgrade therefore records it at the next sweep. Overdue schedules still surface through the inbox as before.
- Kata records each raw value once per issue. A rescheduled issue records its new value when that value is reached, while a value that was already recorded is not recorded again even if the date moves away and back.
- The event does not change the issue or its revision, and Kata records it whether or not the inbox slot it would notify is occupied.
- Closed issues are skipped. An enabled spoke records no event of its own; it receives the hub's event when it pulls hub changes. Hub-to-hub relay does not carry the event.
- `deadline_on` records no event; deadlines remain notification-only.

`kata events --tail` and hooks subscribed to the event type, `issue.*`, or `*` receive it.

## Adapter responsibilities

Adapters must exclude `notify` jobs with `issue.kind: "per-run"` from their schedulers; those jobs belong to the native hub. Existing strict decoders reject the new `parent_uid` shape instead of scheduling it.

Shared `enabled` configuration does not activate an adapter installation. Each adapter owns its local activation, scheduler, overlap and catchup handling, checkout and secret mappings, process lifecycle, and conversation recovery. A second adapter can independently execute the same occurrence. Each execution keeps a separate run UID, and no shared observation reserves the issue or occurrence.

A job can name its adapter in `options.adapter`. An adapter does not schedule or manually run a job whose key names another adapter, and it reports why in its own status. Any adapter can run a job without the key, while Kata stores the key and arbitrates nothing. The value names an adapter rather than an installation, and each adapter decides which of its installations runs the job. `herdr-kata` and `tern-kata` are examples, and each adapter documents its own value. Adapters that do not read the key ignore it; for example, Herdr Kata v0.1.0 logs `ignored options key: adapter` and still runs the job.

Local transcripts, process IDs, session handles, credentials, and delivery buffers are not shared run evidence. An adapter can buffer a failed observation write and retry it later without blocking its own work.

Before starting new scheduled work, the adapter must refresh current definitions; offline cached data is for labeled reads. Portable `cron` triggers use the configured trigger timezone, and issue-date triggers use the planning-date instant above, where issue timezone, recurrence fallback, daemon default, and UTC remain authoritative. Future `scheduled_on` and `someday` defer ordinary execute readiness, while `deadline_on` never does. Deadline actions are notification-only.

Kata's default-date sweeper continues to request the current owner's attention, or the author's when unowned, for reached native schedules and deadlines. The same-source, zero-lead `current-owner-or-author` alias must not produce a second adapter notification, while custom offsets and recipients use ordinary notifications. Inbox addresses are exact: `worker` does not aggregate `worker/adapter`. Reads do not clear requests; the recipient handler clears a request only after handling it and reads back. Attention is a replaceable per-issue and recipient slot, so replacement and clear can race.

Shared definitions and run evidence inherit ordinary authentication, project- and issue-scoped roles, actor attribution, token revocation, and credential-origin checks. Capability discovery advertises compatibility only. Shared federation and backup do not transfer local activation, checkout credentials, execution buffers, or conversation handles; preserve those separately under the adapter's backup workflow. PostgreSQL runtime roles still cannot perform owner-only migrations.

[Herdr Kata](https://github.com/salmonumbrella/herdr-kata/blob/main/docs/native.md) is one adapter that implements this contract, and its documentation covers setup. [tern-kata](https://github.com/Dangerboi/tern-kata/blob/main/docs/guide/worker.md) is another; its worker runs prompt and workflow jobs through [omp](https://github.com/can1357/oh-my-pi) on the user's machine, and its guide covers setup.

## Storage and backup

SQLite and PostgreSQL schema 31 store cron data in `cron_jobs`, `cron_workflows`, and `cron_runs`. JSONL export and import retain shared definitions, including tombstones, along with bounded run evidence. Native recurring delivery reuses ordinary issue metadata and parent links and adds no tables or schema version. Keep parent checkpoints and child provenance in the same backup. Runs with `running` or `unknown` status do not prevent an explicitly requested restore, federation reset, adoption, or project merge; those operations keep their ordinary identity, revision, and credential guards. For owner-only migration, backup, runtime grants, and rollback requirements, see [PostgreSQL migrations](../development/postgres-migrations.md).

## Routes, events, and records

All paths below are project-scoped. Prefix CLI entries with `kata cron` and MCP entries with `kata.`. Because the CLI and MCP create definitions through stable-UID `PUT`, the collection `POST` operations are HTTP only.

| Method | Path | Operation ID | CLI | MCP tool |
| --- | --- | --- | --- | --- |
| `GET` | `/api/v1/projects/{project_id}/cron/capabilities` | `getCronCapabilities` | `capabilities` | `cron_capabilities` |
| `GET` | `/api/v1/projects/{project_id}/cron/jobs` | `listCronJobs` | `job list` | `cron_jobs` |
| `POST` | `/api/v1/projects/{project_id}/cron/jobs` | `createCronJob` | HTTP only | — |
| `GET` | `/api/v1/projects/{project_id}/cron/jobs/{cron_uid}` | `showCronJob` | `job show` | `cron_job` |
| `PUT` | `/api/v1/projects/{project_id}/cron/jobs/{cron_uid}` | `replaceCronJob` | `job create` / `job update` | `cron_job_create` / `cron_job_update` |
| `DELETE` | `/api/v1/projects/{project_id}/cron/jobs/{cron_uid}` | `archiveCronJob` | `job delete` | `cron_job_delete` |
| `POST` | `/api/v1/projects/{project_id}/cron/jobs/{cron_uid}/restore` | `restoreCronJob` | `job restore` | `cron_job_restore` |
| `GET` | `/api/v1/projects/{project_id}/cron/workflows` | `listCronWorkflows` | `workflow list` | `cron_workflows` |
| `POST` | `/api/v1/projects/{project_id}/cron/workflows` | `createCronWorkflow` | HTTP only | — |
| `GET` | `/api/v1/projects/{project_id}/cron/workflows/{cron_uid}` | `showCronWorkflow` | `workflow show` | `cron_workflow` |
| `PUT` | `/api/v1/projects/{project_id}/cron/workflows/{cron_uid}` | `replaceCronWorkflow` | `workflow create` / `workflow update` | `cron_workflow_create` / `cron_workflow_update` |
| `DELETE` | `/api/v1/projects/{project_id}/cron/workflows/{cron_uid}` | `archiveCronWorkflow` | `workflow delete` | `cron_workflow_delete` |
| `POST` | `/api/v1/projects/{project_id}/cron/workflows/{cron_uid}/restore` | `restoreCronWorkflow` | `workflow restore` | `cron_workflow_restore` |
| `GET` | `/api/v1/projects/{project_id}/cron/runs` | `listCronRuns` | `run list` | `cron_runs` |
| `GET` | `/api/v1/projects/{project_id}/cron/runs/{run_uid}` | `showCronRun` | `run show` | `cron_run` |
| `PUT` | `/api/v1/projects/{project_id}/cron/runs/{run_uid}` | `observeCronRun` | `run observe` | `cron_run_observe` |

`kata.cron_new_uid` generates a UID locally and has no HTTP route.

### Run list page size

| Surface | Default page size | Maximum page size |
| --- | --- | --- |
| HTTP `listCronRuns` (omitted `limit`) | 100 | 100 |
| CLI `kata cron run list` | 100 | 100 |
| TUI Runs tab | Newest 100 (fixed) | 100 |
| MCP `kata.cron_runs` | 20 | 100 |

CLI `--limit` and MCP `limit` accept values from 1 to 100, and MCP uses the shared default for paginated results. HTTP, CLI, and MCP return `next_before_uid`; pass it as `before_uid`, or `--before-uid` in the CLI, for the next page. The TUI loads only the newest page.

### Events and records

Workflow routes are `/api/v1/projects/{project_id}/cron/workflows` and `/api/v1/projects/{project_id}/cron/workflows/{cron_uid}`. Responses use `workflow` or `workflows`. Job actions and run observations use `workflow_uid`, and run observations pair it with `workflow_definition_event_uid`. Events use `cron.workflow.*`, and JSONL workflow records use `cron_workflow`.
