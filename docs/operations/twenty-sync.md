---
title: Twenty sync
description: Mirror Twenty workspace tasks into native Kata issues with self-hosted support and optional two-way status sync.
last_edited: 2026-11-20
---

# Twenty sync

Twenty sync mirrors the built-in tasks from one Twenty workspace into one Kata
project. By default, the daemon polls every five minutes. Imported tasks become
native Kata issues, and you can add comments, evidence, relationships, and edits
with ordinary Kata commands. Status updates flow only from Twenty into Kata
until you enable `--status-sync=two-way`.

## Credentials and instances

1. Create an API key in the source Twenty workspace. The key needs permission to
   read workspace metadata, object metadata, and built-in task records. If you
   plan to use two-way mode, also grant permission to update task status.
2. Provide the key to the daemon process, for example by setting
   `KATA_TWENTY_TOKEN` in its service environment. Client workstations do not
   need a Twenty key, and Kata never stores the key value in project
   configuration or sync bindings.

For Twenty Cloud, Kata defaults to `https://api.twenty.com` for API requests and
`https://app.twenty.com` for attribution links. For a self-hosted Twenty
instance, add this section to the daemon's `<KATA_HOME>/config.toml` and restart
the daemon:

```toml
[twenty_sync]
api_origin = "https://twenty.example"
web_origin = "https://twenty.example"
token_env = "EXAMPLE_TWENTY_TOKEN"
```

If you omit `web_origin`, Kata uses the self-hosted `api_origin` as the web
origin too. Both origins must be root HTTPS origins, with HTTP allowed only for
literal loopback IPs. Kata rejects paths, credentials, queries, and fragments in
origin values. Every API request uses the configured API origin and a credential
captured for that run, and Kata rejects redirects. Binding requests cannot
supply keys or change origins.

## Enable and operate

Run these commands to enable, inspect, or pause Twenty sync:

```sh
kata sync twenty enable
kata sync twenty once
kata sync twenty status
kata --agent sync twenty status
kata --json sync twenty status
kata sync twenty disable
```

When you run `enable`, Kata discovers the workspace UUID from the API key and
validates the task schema before saving the binding. Use the global
`--workspace` and `--project` flags to select the native Kata workspace and
project. There is no source workspace flag; the authenticated Twenty workspace
is the source. Once saved, a binding's API origin and workspace UUID cannot
change. If you rotate the key to a different workspace, sync fails before any
import or status write, even if both workspaces share the same origin.

Each native Kata project can have only one external issue-sync binding. Twenty,
Plane, Notion, and GitHub bindings cannot replace each other, even after you
disable one. To sync another source, use another Kata project. If you use
federation spokes, they receive imports from their hub, and you enable Twenty
sync on the hub.

| Enable option | Behavior |
| --- | --- |
| `--interval` | Duration or integer seconds, at least one second; initially `5m`. |
| `--since` | Exclusive updated-after UTC date or whole-second RFC3339 timestamp; explicit empty clears the cutoff. |
| `--title-prefix=false` | Keep source titles and add the source-managed `twenty` label. |
| `--status-sync` | `one-way` initially, or `two-way` for explicit close/reopen delivery. |
| `--closed-status` | API option value classified closed and selected for close writes; initially `DONE`. |
| `--open-status` | API option value selected for reopen writes; initially `TODO`. Must be classified open. |
| `--open-statuses` | Comma-separated API option values classified open; initially `TODO,IN_PROGRESS`. |

Re-enabling keeps any options you omit. Date values mean midnight UTC, timestamp
offsets normalize to UTC, and Kata rejects fractional seconds. For custom
statuses, pass the API option values rather than their display labels:

```sh
kata sync twenty enable --status-sync=two-way \
  --closed-status COMPLETE --open-status READY --open-statuses READY,ACTIVE
```

Every live status option in Twenty must have an unambiguous classification. The
closed value cannot also appear as an open value, and open values must be
distinct and nonempty. A null status maps to open. If Kata sees an unknown
status value, it fails the run instead of guessing whether the task is closed.
Enabling two-way mode requires both write targets to exist in the live schema.
In one-way mode, a binding may keep removed targets as long as every current
option stays classified.

Changing `closed_status`, `open_statuses`, the cutoff, or title presentation
resets the content cursor. Changing only the interval, status direction, or
`open_status` keeps the cursor intact. The cursor records successful run starts
for diagnostics. Each run reads the entire collection and skips tasks updated at
or before the cutoff before inspecting their content, so an older task with
unsupported rich text cannot block newer imports. Pagination uses eventual
polling rather than an atomic Twenty snapshot.

## Mapping and local edits

| Twenty value | Kata issue |
| --- | --- |
| Task UUID and title | `[Twenty <first-eight-UUID-characters>] Original title`; null or blank titles become `(untitled)`. |
| `bodyV2.markdown` | Preserved Markdown with an attribution link under the configured web origin. |
| Configured closed status | `closed`, reason `done`, using source update time for closure time. |
| Configured open statuses or null | `open`. |
| `assigneeId` | Owner `twenty:<UUID>`; null means unassigned. |
| `createdBy.workspaceMemberId` | Author `twenty:<UUID>`, or `twenty-unknown` when absent. |
| Source timestamps | UTC timestamps normalized to persisted milliseconds. |

Kata does not map external member identities to local users automatically.
Title, body, and owner update only when Twenty's timestamps are strictly newer
than the native issue, and equal or older imports keep your local scalar edits.
Imported priority is unset, so a newer content import clears any local priority.
Changing the title prefix refreshes source-owned titles at the same source
version while keeping local title edits. The source label can refresh on its
own, and local labels stay intact. Ordinary external-root ownership protections
also apply.

Two-way status observations do not depend on content timestamps or the cutoff,
so they keep your local title, body, owner, and priority edits. In one-way mode,
status arrives with content imports. Same-state observations keep native closure
evidence. Deleted or unavailable tasks remain in Kata, and absence never closes
or deletes an imported issue. Dates, comments, relationships, attachments, other
objects, and outbound fields other than status are outside this integration.

Your Twenty task object must expose active `title` TEXT, `bodyV2` RICH_TEXT, and
`status` SELECT fields. Legacy body-only schemas are not supported. A null body
imports as empty, while nonempty blocknote content without Markdown on a task
after the cutoff fails content preparation instead of dropping the content
silently. Status-only reads can still deliver pending changes when content
conversion fails.

## Two-way status and recovery

Only explicit Kata close and reopen events accepted while two-way mode is active
queue outbound writes. Opting into two-way mode does not send bulk changes for
existing issues. A close writes `closed_status`, and a reopen writes
`open_status`. If the task is already in a matching open substate, such as
`IN_PROGRESS`, Kata leaves it there. Pending local intent wins until a fresh
readback verifies delivery for that exact event. Outbound writes change only
task status.

Kata attempts each `PATCH` once and never retries it blindly. If a reply is lost
or readback fails, Kata keeps the pending intent, and a later scan checks the
current state before writing again. Missing or deleted tasks, a mismatched
workspace identity, unsupported metadata, or removed targets block delivery and
keep the pending intent. Run `kata sync twenty status` to see the mode, pending
count, and sanitized error. `kata sync twenty once` reports incoming status
updates separately from content import counts.

Disabling pauses polling and delivery while keeping mappings and pending intent.
Switching back to one-way mode cancels pending writes, even when a retained
target has been removed. A JSONL restore clears private observations and pending
intent and leaves polling disabled until you explicitly re-enable it.

## Progress and bounds

Live sync phases are `workspace`, `schema`, `tasks`, `content`, `importing`, and
`finalizing`. A total of zero means the count is unknown. Manual and scheduled
runs share durable claims, pacing, progress, and committed-event delivery, and a
second run conflicts while one is active. Disabling or re-enabling fences the
earlier binding snapshot, while the existing claim remains until the worker
retires or the 30-minute stale-claim horizon allows recovery.

Each run has a 20-minute deadline, each request has a 30-second timeout, and
enable validation has a two-minute deadline. All bindings on a daemon share a
limit of at most one request per second. Each run checks workspace identity and
loads task metadata once, so a status read then costs one request and a status
write costs three requests: read, `PATCH`, and verified readback. Read requests
allow four attempts for HTTP 429 and server errors, and `Retry-After` cooldowns
are capped at 20 minutes. Bindings run serially, so a slow workspace delays
others.

Each sync run enforces these limits as failure guards rather than supported-size
promises: 10,000 tasks or metadata records, 1,000 pages per collection, 8 MiB per
response, 128 MiB of raw responses per collection, 1 MiB of Markdown per task,
and 64 MiB of serialized import items. Incomplete fields, duplicate identities,
repeated or missing cursors, and exceeded bounds fail preparation before any
content import starts. Metadata object and field collections paginate
separately.

When troubleshooting a failed run, start with the error category:

- **Credential errors.** Check the selected environment variable in the daemon's
  service environment.
- **Mapping errors.** Inspect your API option values in Twenty and re-enable
  with a complete classification.
- **Instance compatibility errors.** Verify the required metadata and core task
  fields listed above.

Failed runs keep your native issues, pending intent, and the last successful
cursor for the next attempt.
