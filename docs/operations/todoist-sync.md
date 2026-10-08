---
title: Todoist sync
description: Import one Todoist project and optionally synchronize standalone task completion and reopening.
last_edited: 2026-10-08
---

# Todoist sync

Mirror one Todoist project into one Kata project. The daemon imports active
tasks and completion history every five minutes by default. Enable two-way
status sync to send explicit Kata close/reopen changes back to standalone,
non-recurring Todoist tasks. Other fields remain incoming-only.

## Configure the daemon

Give the daemon a personal API token from Todoist's integration settings, or an
operator-obtained OAuth token. OAuth requires `data:read` for import and
`data:read_write` for two-way status. Kata does not need deletion scopes or run
an OAuth authorization flow. The client workstation needs only its normal Kata
credentials. See the [official Todoist API](https://developer.todoist.com/api/v1/)
for authentication, permissions and endpoint limits.

Set `KATA_TODOIST_TOKEN` in the daemon's service environment. To select another
variable, configure `<KATA_HOME>/config.toml`:

```toml
[todoist_sync]
token_env = "EXAMPLE_TODOIST_TOKEN"
```

Requests use the public `https://api.todoist.com` origin. The token is captured
once per run and never enters binding config, exports, command output or
provider error messages. Redirects are rejected. Token rotation must keep the
same Todoist account; a different account blocks the saved binding. Restart the
daemon after changing its service environment or configuration. Embedded
services use `Config.TodoistSync`. For continuous remote polling, set
`autostart_idle_timeout = "0"` on the selected daemon.

## Enable and operate

From a bound Kata workspace:

```sh
kata sync todoist enable --todoist-project project123
kata sync todoist once
kata sync todoist status
kata --agent sync todoist status
kata --json sync todoist status
kata sync todoist enable --status-sync two-way
kata sync todoist disable
```

`--todoist-project` selects the opaque Todoist project ID. Kata's global
`--project` selects the native Kata project. Enable resolves the credential's
account identity and validates the selected active project. Shared projects
are supported when that account can access them; task ownership does not
restrict imports to tasks owned by the credential holder.

Re-enable preserves omitted mode, interval and title presentation. Use
`--interval 2m` to change polling or `--title-prefix=false` to retain original
titles and add the `todoist` label. New bindings default to one-way mode and
`[Todoist] ` title prefixes. Disable stops delivery and polling while retaining
native issues, bindings and pending status intent.

The completion-history floor defaults to thirty days before initial enable.
Use `--history-since 2026-09-01` or a whole-second RFC3339 instant to choose it
initially. All active tasks are imported regardless of that floor. Account,
project and history floor are immutable; use a separate Kata project for a
different scope. This prevents an omitted option or credential change from
silently broadening an existing binding.

## Field ownership and completion

| Field | Ownership |
|---|---|
| Title, description, labels, priority, author and assignee | Imported from Todoist under Kata's existing import conflict rules; local edits are never sent upstream. |
| Open/closed status | Incoming in one-way mode; explicit Kata close/reopen events also deliver in two-way mode. |
| Due dates, deadlines, recurrence, reminders, comments, sections and parent relationships | Remain in Todoist; not projected or written back. |
| Task creation, deletion and project membership | Remain in Todoist; Kata never creates, deletes or moves provider tasks. |

Todoist priority 4 maps to Kata priority 1; Todoist priority 1 maps to Kata
priority 4. Labels use Kata's canonical import spelling: lowercase names,
whitespace and unsupported punctuation converted to hyphens, and bounded names
with duplicates removed. Original labels stay in Todoist. Imported bodies include a link to the task. A completed task maps
to closed/done. A local close reason does not change Todoist's completion type.

Pending Kata intent wins until a fresh provider read verifies it. Otherwise,
fresh provider observations determine status. Repeated imports and verified
converged writes do not echo changes back. A newer local close/reopen event
replaces older queued intent. The existing claim and admission checks fence
disabled bindings, changed settings and superseded intent before dispatch.

Recurring tasks remain importable. Their outbound completion is blocked because
Todoist's close operation advances the next occurrence. Complete them in
Todoist. An incomplete response that omits recurrence or hierarchy evidence also blocks
writeback. Kata also blocks closing tasks with active subtasks and reopening
child tasks because Todoist cascades those operations. Reopening a task in a
section requires a fresh read proving that section is active in the selected
project; an archived or unavailable section blocks delivery. Todoist offers no
atomic conditional status operation, so provider edits between validation and
dispatch remain an API concurrency limitation.

## Recovery and limits

Each run reads every active task with project-filtered cursor pagination. It
reads completion history in contiguous thirty-day windows, because Todoist
allows at most three months per completion-date request:

- The first run, and the first run after a title-presentation change, read
  history from the floor.
- Later runs read completions since the last successful run's start, with a
  two-minute overlap. A failed run does not advance that point.

Each run's reads are bounded to 1,000 pages, 10,000 task observations, 8 MiB
per response and 64 MiB total. Exceeding a limit or failing a page prevents
partial content import. Status scans have their own durable checkpoints and can
progress independently of content failures.

Two-way status scans read each mapped task's active state. A task whose
completion Kata already verified is not looked up in history again while it
stays inactive. A task last seen open is searched in history from that
observation onward; other lookups start at the configured floor. Status scans
search recent thirty-day windows first and stop when they find the exact task.
They share successful history reads across mappings and stay within the
10,000-observation limit per lookup. Reopen checks and write verification read
history fresh.

Unknown (`null`) creation or update times take the task's other known
timestamps; a task with none blocks the run. When `updated_at` is unknown, a
fresh status change still advances its checkpoint past the prior observation.
One-way imports may reconcile that status while the stored Todoist version
still owns the issue. The status update preserves other issue fields, and newer
local edits remain authoritative.

An active task wins over historical completions of the same ID, including old
recurring occurrences. Conflicting observations at the same version fail the
run. A missing task or `404` is never treated as completion or deletion. Kata
accepts a completion only when the exact task is found in scoped history and
an active reread does not find a reopened task. Deleted, moved, inaccessible or
archived data blocks delivery and preserves native issues. Tasks completed
before the configured history floor cannot be resolved through that history.

Reads retry transient failures with bounded backoff. Requests share pacing and
`Retry-After` cooldowns. Authentication and permission failures stop credential
fallback. A status POST is sent once and verified through fresh reads. A lost
or unverified response retains pending intent; the next run reads the provider
before deciding whether another write is needed. Check `status` for pending
changes and blocked errors. Repair credentials or provider state, then run
`kata sync todoist once` or wait for the next scheduled attempt.
