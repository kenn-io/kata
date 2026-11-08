---
title: Author cron jobs with an agent
description: Choose cron job settings, verify delivery, and report the schedule and execution owner.
last_edited: 2026-11-03
---

# Author cron jobs with an agent

Use these decision rules before creating or changing a cron job. Agents get
this version-matched page from `kata cron guide` or the MCP guide
`kata.skills {"name":"cron"}`. For field definitions and request examples,
use the [Cron reference](https://katatracker.com/docs/reference/cron/).

## Pick the job kind

Check the target daemon first:

```sh
kata cron capabilities --json
```

If `event_features` does not advertise `cron_v1`, or the check fails, stop
and report the result. Capability support does not grant write permission.

For a recurring reminder, choose a native `notify` job with
`issue.kind: "per-run"` and an open parent issue. The owning daemon creates
child issues and inbox requests; no execution adapter is needed. Omit
`checkout_key` and adapter `options` from these jobs.

For work an agent performs, choose `execute` and confirm an external adapter
is installed and active for this project. Put shell commands in a workflow
and reference its `workflow_uid`. Kata never launches a process. See
[actions](https://katatracker.com/docs/reference/cron/#actions) and
[recurring child issues](https://katatracker.com/docs/reference/cron/#recurring-child-issues).

## Never invent identifiers

Get parent refs, workflow UIDs, issue-date trigger UIDs, exact inbox recipients
and adapter names from the user or an authoritative read. Start with
`kata search "<terms>" --json`, `kata cron workflow list --json`, and
`kata cron job list --json`. Inspect a workflow with
`kata cron workflow show <workflow-uid> --json` before reusing it.

An execution adapter resolves `checkout_key` on its own machine. If the job
needs one and the user has not supplied a known key, ask for it. A guessed
repository name is not a checkout mapping. Native reminders need no checkout.

For a new definition, omit `--uid`: the CLI generates a UID and prints it to
stderr. Retain that UID and reuse it on retry. MCP callers obtain and retain
one with `kata.cron_new_uid` before creating anything. After an uncertain
response, read that UID first; an existing definition must be updated, not
created again. See [request files](https://katatracker.com/docs/reference/cron/#request-files).

## Choose the execution adapter, agent and model

For execution jobs, set `options.adapter` when more than one adapter may
serve the project. Confirm the installed adapter versions honor that key;
older adapters can ignore it. It names an adapter, not a single installation.

For prompt execution, including workflow prompt steps, explicitly set every
option that adapter documents for choosing the agent and model. For example,
`options.agent` may select the prompt agent; use the adapter's documented
model selector rather than guessing `options.model` or relying on defaults.
If its interface provides no selector, ask how the operator configures it.
Match the model to the work: a routine summary need not use the largest one.
Command-only workflows need no prompt-agent selector.

Kata stores these options without interpreting their meaning. Native
recurring reminders reject nonempty adapter options. Keep credentials and
local paths out of `options`; use the adapter's local configuration. See
[adapter responsibilities](https://katatracker.com/docs/reference/cron/#adapter-responsibilities)
and [job fields](https://katatracker.com/docs/reference/cron/#job-definitions).

## Write the schedule in the user's timezone

For cron triggers, explicitly set `trigger.timezone` to the user's IANA zone.
Ask if the zone is unknown. State the chosen time in that zone, not only UTC.
These expressions are starting points; apply the staggering rule below unless
the user requested the exact time.

| Request | Starting expression |
| --- | --- |
| Every morning | `0 9 * * *` |
| Weekdays at 9 | `0 9 * * 1-5` |
| Mondays at 9 | `0 9 * * 1` |
| Fridays at 17 | `0 17 * * 5` |
| Nightly | `<minute> 2 * * *` |
| Hourly | `<minute> * * * *` |

Ask when the request is ambiguous enough to change the result. Follow Kata's
[cron and daylight-saving rules](https://katatracker.com/docs/reference/cron/#cron-expressions);
do not treat local days as fixed 24-hour intervals. That section also explains
which expressions need newer parsers on every hub, spoke and adapter.
Issue-date triggers follow the separate
[planning-date rules](https://katatracker.com/docs/reference/cron/#native-issue-planning-dates).

## Stagger jobs in the project

List existing jobs before choosing a minute. Unless the user requested an
exact time, pick a minute other than `:00` or `:30` and avoid minutes already
used by enabled jobs in this project. Report the final time you chose.

The daemon delivers due native reminders; adapters schedule execution jobs.
`overlap` applies within one job, not across the fleet. An adapter selector
also does not reserve an occurrence against a second installation. See
[adapter responsibilities](https://katatracker.com/docs/reference/cron/#adapter-responsibilities).

## Choose overlap, catch-up and enabled deliberately

For execution jobs, start with `overlap: "forbid"` and `catchup: "latest"`.
For native per-run reminders, use the required `overlap: "allow"` and start
with `catchup: "latest"`. Choose `catchup: "all"` only when the user wants
every missed occurrence; replay is bounded by the native backfill limit.
See [recurring delivery](https://katatracker.com/docs/reference/cron/#recurring-child-issues).

Set `enabled: true` only when the user wants the job running now. Otherwise,
explicitly report that it is disabled. For execution jobs, shared enabled
state does not activate the adapter locally.

## Change an existing job safely

Read `kata cron job show <job-uid> --json` first. Edit the full request file,
then replace the definition with its current `definition_event_uid`:

```sh
kata cron job update <job-uid> --expected-event-uid <event-uid> --file replacement.json --json
```

On a conflict, read again and reapply the intended change. Never blindly
retry with a stale event UID. A native job's parent binding cannot change.
Pause a job by updating `enabled: false`. Archive it with
`kata cron job delete <job-uid> --expected-event-uid <event-uid> --json`;
`kata cron job restore <job-uid> --expected-event-uid <event-uid> --json`
reverses the archive using the current event UID. See
[definitions and retry rules](https://katatracker.com/docs/reference/cron/#definitions).

## Verify the saved definition and first delivery

Use the [request examples](https://katatracker.com/docs/reference/cron/#request-files)
or `kata cron job create --help` when preparing the file. For a native
reminder, pass `--parent <parent-ref>` on create. Save and read back:

```sh
kata cron job create --file job.json --json
kata cron job show <job-uid> --json
```

Confirm the saved trigger, timezone, enabled state and execution options.
Create and update validate definitions; this version has no separate validate
or run-now command. Use an adapter's documented test-run command only if it
provides one. A workflow can be created with
`kata cron workflow create --file workflow.json --json`.

After native delivery, inspect the child under its parent with
`kata show <parent-ref> --json` and the exact recipient's request with
`kata inbox --for <actor/teammate> --json`. Native delivery creates no run
observation. For adapter execution, inspect
`kata cron run list --job-uid <job-uid> --json` and
`kata cron run show <run-uid> --json`, then corroborate with the adapter's
output. Recording a run does not launch or prove execution. See
[run evidence](https://katatracker.com/docs/reference/cron/#independent-run-evidence).

## Report what is scheduled and who runs it

Report the job UID and name, the final schedule in words and its timezone,
whether it is enabled, and the owning daemon or configured execution adapter.
For prompt execution, include the chosen agent and model. Explain how to
pause or archive the job.

Report the next nominal occurrence in that timezone and where the preview
came from. `kata tui` previews times with Kata's schedule code: native **Next**
requires readable progress and an open parent; adapter **Triggers** are
nominal matches, with actual execution depending on local activation. Disabled
jobs have no scheduled delivery. If no reliable preview is available, report
that the next time is unavailable; never invent a timestamp or promise an
adapter will run. See the
[TUI preview rules](https://katatracker.com/docs/reference/cli/#daemon-and-diagnostics).
