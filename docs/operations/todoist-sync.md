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
Todoist's close operation moves them to their next date. Complete them in
Todoist. Kata also blocks closing tasks with active subtasks, reopening
subtasks, and reopening tasks in an archived section, because Todoist applies
those operations to the subtasks, parent tasks or section too. Todoist offers
no atomic conditional status operation, so provider edits between these checks
and the write remain an API concurrency limitation.

## Recovery and limits

Each run reads every active task with project-filtered cursor pagination. It
reads completion history in windows of at most three months, the limit Todoist
allows per completion-date request:

- The first run, and the first run after a title-presentation change, read
  history from the floor.
- Later runs read completions since the last successful run's start, with a
  two-minute overlap. A failed run does not advance that point.

A failed read prevents partial content import. Status scans have their own
durable checkpoints and can progress independently of content failures.

Two-way status scans read each mapped task's active state. A task whose
completion Kata already verified is not looked up in history again while it
stays inactive. A task last seen open is searched in history from that
observation onward; other lookups start at the configured floor.

Todoist reports unknown (`null`) creation or update times. Kata uses the task's
other known timestamps instead, or the history floor when none are known.
Two-way status scans never move a task's status version backwards, so a reopen
in Todoist still reaches Kata. One-way imports cannot order such a task against
Kata's stored version. For those, a reopen in Todoist and a `--title-prefix`
change reach the task only after it changes again in Todoist.

An active task wins over historical completions of the same ID, including old
recurring occurrences. A missing task is never treated as completion or
deletion: Kata accepts a completion only when the exact task is found in scoped
history. Deleted, moved or inaccessible tasks block delivery and preserve native
issues. Tasks completed before the configured history floor cannot be resolved
through that history.

Reads retry `429` and `5xx` responses with exponential backoff, honoring
`Retry-After`. A successful response with an empty body fails that run without
advancing it, so no safety check or history read relies on missing data.
Authentication and permission failures block the binding. A close or reopen is sent once and verified with a fresh read. A lost or
unverified response retains pending intent; the next run reads the provider
before deciding whether another write is needed. Check `status` for pending
changes and blocked errors. Repair credentials or provider state, then run
`kata sync todoist once` or wait for the next scheduled attempt.
