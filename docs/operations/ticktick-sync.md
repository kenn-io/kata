---
title: TickTick sync
description: Import a scoped TickTick task project into Kata and send ordinary task completion back through the public API.
last_edited: 2026-11-20
---

# TickTick sync

TickTick sync imports one TickTick task project into one Kata project. By
default, the daemon polls every five minutes. If you opt into
`--status-sync=two-way`, Kata sends explicit local completion events back to
TickTick and pulls upstream status changes. Titles, descriptions, priorities,
and assignments stay incoming-only. This feature requires a build that includes
TickTick sync; it is not part of older releases.

## Configure the daemon

1. Get a personal API token from TickTick's **Settings → Account → API Token**,
   or prepare an OAuth access token with `tasks:read` and, for completion
   writeback, `tasks:write`. Follow the
   [official public API documentation](https://developer.ticktick.com/docs/index.html)
   for token setup. Kata uses an existing access token and does not run an OAuth
   login or refresh flow. The selected account must be able to read the selected
   project, and two-way mode also requires write access. Kata rejects an
   explicit read or comment project permission when you enable two-way mode.
2. Set `KATA_TICKTICK_TOKEN` in the daemon's service environment. To read from a
   different environment variable, add this block to `<KATA_HOME>/config.toml`:

   ```toml
   [ticktick_sync]
   token_env = "EXAMPLE_TICKTICK_TOKEN"
   ```

3. Restart the daemon after changing its config or service environment.

Each run captures the token once, and later runs pick up a rotated token. The
daemon sends requests only to `https://api.ticktick.com` and rejects redirects.
Tokens never enter binding config, exports, logs, or CLI output. When you use a
remote Kata client, configure the selected remote daemon. If polling must
continue without connected clients, set `autostart_idle_timeout = "0"`. Embedded
services use `Config.TickTickSync`.

## Enable and operate

Find the TickTick project ID from the public API or from the project segment of
its TickTick web link. Pick a task project rather than a notes project or an
archived project, then run:

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

Use Kata's global `--project` flag to select the native Kata project.
Re-enabling without options keeps your source, title presentation, interval,
mode, and private content checkpoint. Each native Kata project can have only one
external binding, even after you disable it; use another Kata project for
another source. Federation spokes receive imports through their hub, and you
enable sync on the hub. Disabling sync pauses polling while keeping your
mappings and pending intent.

## Field ownership and status

| TickTick value | Kata issue |
| --- | --- |
| Title | `[TickTick] Original title`, or the original title with prefixes disabled; blanks become `(untitled)`. |
| Content, description, checklist | Body with checked/unchecked items and a TickTick attribution link. |
| Start/due date, timezone, recurrence rule | Body text; no local schedule or recurrence is created. |
| Priority | `5` → P1, `3` → P2, `1` → P3, `0` → unassigned. |
| Assignee username | Owner `ticktick:<username>`; absent means unassigned. |
| Status `0` / `2` | Open / closed with reason `done`. |

Imported issues use `ticktick-unknown` as their author, and Kata does not map
external usernames to local users automatically. Kata skips note tasks and
abandoned tasks with status `-1`, while keeping any previously imported issues
intact. Unknown statuses, invalid identities, or invalid content fail
preparation before imports begin.

TickTick's API does not provide a task content revision, and Kata stores a
fingerprint and observation time for each task instead. Identical content keeps
its original version across retries, restarts, and reordered responses, while a
changed fingerprint gets a new observed version. The newest observed content can
replace older local scalar edits, and unchanged content keeps your local edits.
The observation time records when Kata fetched the task list, or when it read a
missing task on its own; it is not the original TickTick creation or edit time.
Status changes do not advance the content version or replace local scalar edits,
and changing only the status direction also keeps unchanged content at its saved
version. In one-way mode, Kata records the current TickTick status on every run.
Seeing the same status again changes nothing, so a local close or reopen stays
in place until TickTick's status changes.

Only explicit local close and reopen events accepted while two-way mode is
active queue outbound writes; opting in does not send existing local states in
bulk. Pending local intent wins until a fresh readback acknowledges its exact
event. For an ordinary close, Kata rechecks the project and task, calls the
public completion endpoint, and verifies the resulting status. Tasks that are
already completed need no write. Imported status changes do not produce an
outbound echo, and same-state reads keep local closure evidence.

- **Outbound reopen is blocked.** The public API does not document a safe reopen
  operation or guarantee status-only partial updates. Reopen the task in
  TickTick and retry; fresh readback then settles the retained local intent.
- **Recurring-task completion writeback is also blocked.** Kata blocks it
  because completing an occurrence can advance or create another occurrence.
  Complete recurring tasks in TickTick. Kata can still observe incoming
  recurring status.

Kata never sends full task objects to change status, so provider-owned fields
stay intact.

Run `kata sync ticktick status` to check pending intent and sanitized errors.
Blocked or ambiguous writes keep their intent for later verification. If a
timeout or server failure happens after a `POST`, Kata does not retry blindly;
the next run reads the task before deciding whether to send another completion.
Switching back to one-way mode cancels pending writes. An ordinary JSONL restore
clears private sync observations, disables polling, and requires an operator to
re-enable the binding.

## Recovery and limits

TickTick documents its current-project data endpoint as an unpaginated
collection of open tasks, and Kata reads it in full on every run. Kata does not
use the capped completed-task history endpoint as a complete archive. When a
mapped task is missing from that collection, Kata reads it individually only
while its Kata issue is open, up to 100 task IDs per run in a durable, sorted
rotation. A task whose first import failed is read the same way until it imports
or TickTick no longer has it. This recovers completion without rereading
completed history on every run.

If you closed the Kata issue locally while TickTick still showed the task as
open, Kata reads the task once more to record its completion. After that, Kata
stops reading the task and forgets its fingerprint. If the task comes back, for
example after a reopen in either tool, Kata starts from the source version saved
on its mapping. Unchanged content cannot replace newer local edits, and a
TickTick reopen still reopens the issue. TickTick edits made while the task was
untracked import with the next TickTick edit. New historical completed tasks are
not guaranteed to be backfilled. Absence and task `404` responses never delete
or close local issues. Moved, deleted, abandoned, and inaccessible tasks stay
local, and Kata skips any task returned under another project. Explicit auth
failures stop preparation, and archived or closed projects stop sync while
keeping local issues.

Manual and scheduled runs share a durable claim, a progress tracker, and a
one-request-per-second client. Two-way status reads cost one task request each.
The status pass checks project access once per run, and every completion write
rechecks it. HTTP 429 responses set a shared `Retry-After` cooldown capped at 15
minutes, and absent or invalid delays default to ten seconds. Reads make at most
three attempts for rate limits or server errors. Authentication failures are not
retried. Completion `POST`s are attempted once, with fresh verification before
any later attempt. Individual requests time out after 30 seconds, runs time out
after 20 minutes, and enable validation has a two-minute deadline. Claims can
recover after a 30-minute stale horizon, and disabling or changing configuration
fences an older run's writes.

Sync enforces these limits as failure guards rather than supported-size
promises: 10,000 tracked tasks, counting open tasks plus missing tasks with open
Kata issues, 8 MiB per JSON response, 1 MiB per task text field, 64 MiB per
serialized import, and 2 MiB for the private fingerprint checkpoint. TickTick does
not document a numeric rate quota, and Kata's pacing is conservative rather than
a quota guarantee. Large projects or prolonged cooldowns can exceed a deadline.
Bindings run serially, so one slow source can delay another.

Preparation validates the full fetched data and stages the fingerprint
checkpoint before guarded import chunks run. If a later chunk fails, retrying
keeps the original observed versions and replays safely, while successful chunks
and their events stay committed. The shared status worker rotates through
existing mappings independently of content imports, so a content failure does
not hide a status transition. When a run fails, check your daemon credentials,
project access, pending warnings, and size limits, then retry with
`kata sync ticktick once`.
