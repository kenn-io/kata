---
title: TickTick sync
description: Import a scoped TickTick task project into Kata and send ordinary task completion back through the public API.
last_edited: 2026-10-10
---

# TickTick sync

TickTick sync imports one TickTick task project into one Kata project. The daemon
polls every five minutes by default. Opt into `--status-sync=two-way` to send
explicit Kata completion events back to TickTick and receive upstream status
changes. Titles, descriptions, priorities, and assignments remain incoming-only.
This feature requires a build containing TickTick sync; it is not part of older
releases.

## Configure the daemon

Use a personal API token from TickTick's **Settings → Account → API Token**, or
an OAuth access token with `tasks:read` and, for completion writeback,
`tasks:write`. Follow the [official public API documentation](https://developer.ticktick.com/docs/index.html).
Kata consumes an existing access token; it does not run an OAuth login or refresh
flow. The selected account must be able to read the selected project. Two-way
mode also requires write access. An explicit read/comment project permission is
rejected when enabling two-way mode.

Set `KATA_TICKTICK_TOKEN` in the daemon's service environment. To select another
environment variable, configure `<KATA_HOME>/config.toml`:

```toml
[ticktick_sync]
token_env = "EXAMPLE_TICKTICK_TOKEN"
```

Restart the daemon after changing its config or service environment. Each run
captures the token once; later runs see a rotated token. Requests use only
`https://api.ticktick.com`; redirects are rejected. Tokens never enter binding
config, exports, logs, or CLI output. Configure the selected remote daemon when
using a remote Kata client. Set `autostart_idle_timeout = "0"` if polling must
continue without connected clients. Embedded services use `Config.TickTickSync`.

## Enable and operate

Use the project ID from the public API or the project segment of its TickTick
web link. Select a task project, not a notes project or an archived project.

```sh
kata sync ticktick enable --ticktick-project project-1
kata sync ticktick once
kata sync ticktick status
kata --agent sync ticktick status
kata --json sync ticktick status
kata sync ticktick enable --status-sync=two-way
kata sync ticktick disable
```

| Enable option | Behavior |
| --- | --- |
| `--ticktick-project` | TickTick project ID; required initially and immutable thereafter. |
| `--interval` | Duration or integer seconds, at least one second; initial default `5m`. |
| `--status-sync` | `one-way` initially, or `two-way`; omission preserves the saved mode. |
| `--title-prefix=false` | Keep source titles and add the source-managed `ticktick` label. |

Kata's global `--project` selects the native Kata project. Re-enable without
options preserves the source, presentation, interval, mode, and private content
checkpoint. One external binding owns each native project, including after
being disabled. Use another Kata project for another source. Federation spokes
receive imports through their hub; enable sync on the hub. Disable pauses
polling and retains mappings and pending intent.

## Field ownership and status

| TickTick value | Kata issue |
| --- | --- |
| Title | `[TickTick] Original title`, or the original title with prefixes disabled; blanks become `(untitled)`. |
| Content, description, checklist | Body with checked/unchecked items and a TickTick attribution link. |
| Start/due date, timezone, recurrence rule | Body text; no local schedule or recurrence is created. |
| Priority | `5` → P1, `3` → P2, `1` → P3, `0` → unassigned. |
| Assignee username | Owner `ticktick:<username>`; absent means unassigned. |
| Status `0` / `2` | Open / closed with reason `done`. |

Authors are `ticktick-unknown`. External usernames are not automatically mapped
to local users. Note tasks and abandoned tasks (`-1`) are skipped; previously
imported issues remain intact. Unknown statuses, invalid identities, or invalid
content fail preparation before imports begin.

The API does not supply a task content revision. Kata persists a fingerprint and
observation time for each task. Identical content keeps its original version
across retries, restarts, and reordered responses. A changed fingerprint gets a
new observed version. The newest observed content can replace older local scalar
edits; unchanged content preserves local edits. The observation time is when
Kata fetched the task list, or when it read a missing task on its own. It is not
the original TickTick creation or edit time. Status changes do not advance the
content version or replace local scalar edits. Changing only status direction
also keeps unchanged content at its saved version. One-way mode records the
current TickTick status on every run. A repeat of the last recorded status
changes nothing, so a local close or reopen stays until TickTick's status
changes.

Only explicit local close/reopen events accepted in two-way mode queue writes.
Opt-in does not send existing local states in bulk. Pending local intent wins
until fresh readback acknowledges its exact event. An ordinary close uses the
public completion endpoint after rechecking the project and task, then verifies
the resulting status. Already-completed tasks need no write. Imported status
changes produce no outbound echo. Same-state reads preserve local closure
evidence.

**Outbound reopen is blocked.** The public API does not document a safe reopen
operation or guarantee status-only partial updates. Reopen the task in TickTick
and retry; fresh readback then settles the retained local intent. **Recurring-task
completion writeback is also blocked** because completing an occurrence can
advance or create another occurrence. Complete it in TickTick. Incoming recurring
status can still be observed. Kata never sends full task objects to change
status, so provider-owned fields are preserved.

Status reports pending intent and sanitized errors. Blocked or ambiguous writes
retain intent for later verification. A timeout or server failure after a POST
is not blindly retried; the next run reads the task before considering another
completion. Returning to one-way mode cancels pending writes. Ordinary JSONL
restore clears private sync observations, disables polling, and requires an
operator to re-enable the binding.

## Recovery and limits

The current-project data endpoint is documented as an unpaginated collection of
open tasks. Kata reads it in full each run. It does not use the capped
completed-task history endpoint as a complete archive. A mapped task missing
from the collection is read individually only while its Kata issue is open, at
most 100 task IDs per run in a durable, sorted rotation. A task whose first
import failed is read the same way until it is imported or TickTick no longer
has it. This recovers completion without rereading completed history on every
run. If the Kata issue was closed locally while TickTick still showed the task
open, Kata reads the task once more to record its completion. After that, Kata
stops reading the task and forgets its fingerprint. If the task returns, for
example after a reopen in either tool, Kata starts from the source version saved
on its mapping. Unchanged content cannot replace newer local edits, and a
TickTick reopen still reopens the issue. TickTick edits made while the task was
untracked import with the next TickTick edit. New historical completed tasks are
not guaranteed to be backfilled. Absence and task `404` responses never delete
or close local issues. Moved, deleted, abandoned, and inaccessible tasks remain
local; a task returned under another project is skipped. Explicit auth failures
stop preparation. Archived/closed projects stop sync and retain local issues.

Manual and scheduled runs share a durable claim, progress tracker, and one
request-per-second client. Two-way status reads cost one task request each; the
status pass checks project access once per run, and every completion write
rechecks it. HTTP 429 responses establish a shared `Retry-After` cooldown,
capped at 15 minutes; absent or invalid delays use ten seconds. Reads retry at
most three times for rate limits or server errors. Authentication failures are
not retried. Completion POSTs are attempted once, with fresh verification before
later attempts. A request times out after 30 seconds; a run after 20 minutes.
Enable validation has a two-minute deadline. Claims can recover after a
30-minute stale horizon. Disable and configuration changes fence an older run's
writes.

Bounds are 10,000 tracked tasks (open tasks plus missing tasks with open Kata
issues), 8 MiB per JSON response, 1 MiB per task text field, 64 MiB per
serialized import, and 2 MiB for the private fingerprint checkpoint. These are
failure guards, not supported-size promises. TickTick does not document a
numeric rate quota; pacing is conservative rather than a quota guarantee. Large
projects or prolonged cooldowns can exceed a deadline. Bindings run serially, so
one slow source can delay another.

Preparation validates the complete fetched data and stages the fingerprint
checkpoint before guarded import chunks. If a later chunk fails, retry retains
the original observed versions and replays safely. Successful chunks and their
events remain committed. The shared status worker rotates through existing
mappings independently of content imports, so a content failure does not hide a
status transition. Check daemon credentials, project access, pending warnings,
and size limits, then retry with `once`.
