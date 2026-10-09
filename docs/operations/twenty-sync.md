---
title: Twenty sync
description: Mirror Twenty workspace tasks into native Kata issues with self-hosted support and optional two-way status sync.
last_edited: 2026-10-08
---

# Twenty sync

Twenty sync mirrors the built-in tasks in one Twenty workspace into one Kata
project. The daemon polls every five minutes by default. Imports remain native
issues: local comments, evidence, relationships, and edits use ordinary Kata
commands. Status is incoming-only until you enable `--status-sync=two-way`.

## Credentials and instances

Create an API key in the source Twenty workspace. It must read workspace and
object metadata and built-in task records. Two-way mode also requires permission
to update task status. Supply the key to the daemon process, for example through
its service environment as `KATA_TWENTY_TOKEN`. Client workstations need no Twenty
key. The key value is never stored in project configuration or sync bindings.

Cloud defaults are `https://api.twenty.com` for API calls and
`https://app.twenty.com` for attribution links. For a self-hosted instance, put
this in the daemon's `<KATA_HOME>/config.toml` and restart the daemon:

```toml
[twenty_sync]
api_origin = "https://twenty.example"
web_origin = "https://twenty.example"
token_env = "EXAMPLE_TWENTY_TOKEN"
```

A self-hosted API origin is also the default web origin when `web_origin` is
omitted. Origins must be root HTTPS origins; HTTP is accepted only for literal
loopback IPs. Paths, credentials, queries, and fragments are rejected. Every API
request uses the configured API origin and a credential captured for that run;
redirects are rejected. Binding requests cannot supply keys or change origins.

## Enable and operate

```sh
kata sync twenty enable
kata sync twenty once
kata sync twenty status
kata --agent sync twenty status
kata --json sync twenty status
kata sync twenty disable
```

Enable discovers the workspace UUID from the API key and validates the task
schema before saving. Global `--workspace` and `--project` select the native
Kata workspace/project. There is no source workspace flag: the authenticated
Twenty workspace is the source. A binding's API origin and workspace UUID are
immutable. Rotating the key to another workspace fails before import or status
writes, even when both workspaces use the same origin.

One external issue-sync binding owns each native project. Twenty, Plane,
Notion, and GitHub bindings cannot replace each other, including after disable.
Use another Kata project for another source. Federation spokes receive imports
through their hub; enable Twenty on the hub.

| Enable option | Behavior |
| --- | --- |
| `--interval` | Duration or integer seconds, at least one second; initially `5m`. |
| `--since` | Exclusive updated-after UTC date or whole-second RFC3339 timestamp; explicit empty clears the cutoff. |
| `--title-prefix=false` | Keep source titles and add the source-managed `twenty` label. |
| `--status-sync` | `one-way` initially, or `two-way` for explicit close/reopen delivery. |
| `--closed-status` | API option value classified closed and selected for close writes; initially `DONE`. |
| `--open-status` | API option value selected for reopen writes; initially `TODO`. Must be classified open. |
| `--open-statuses` | Comma-separated API option values classified open; initially `TODO,IN_PROGRESS`. |

Re-enable preserves omitted options. Dates mean midnight UTC, timestamp offsets
normalize to UTC, and fractional seconds are rejected. Custom statuses use API
option values rather than display labels:

```sh
kata sync twenty enable --status-sync=two-way \
  --closed-status COMPLETE --open-status READY --open-statuses READY,ACTIVE
```

Every live status option must have an unambiguous classification. The closed
value cannot also be open, and open values must be distinct and nonempty. Null
status maps to open. Unknown values fail the run rather than guessing a closure.
Two-way enable requires both write targets in the live schema. One-way mode may
retain removed targets as long as every current option remains classified.

Changes to `closed_status`, `open_statuses`, cutoff, or title presentation reset
the content cursor. Changing only interval, status direction, or `open_status`
preserves it. The cursor records successful run starts for diagnostics. Each run
traverses the complete collection and skips tasks updated at or before the
cutoff before checking their content, so an older task with unsupported rich
text cannot block newer imports. Pagination is eventual polling rather than an
atomic Twenty snapshot.

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

External member identities are not mapped automatically to local users. Title,
body, and owner update only when source timestamps are strictly newer than the
native issue. Equal or older imports preserve local scalar edits. Imported
priority is unset, so a newer content import clears local priority. Title-prefix
changes refresh source-owned titles at the same source version while preserving
local title edits. The source label can refresh independently; local labels stay
intact. Ordinary external-root ownership protections also apply.

Two-way status observations are independent of content timestamps and cutoff,
so they preserve local title/body/owner/priority edits. One-way status arrives
with content imports. Same-state observations preserve native closure evidence.
Deleted or unavailable tasks stay local; absence never closes or deletes an
imported issue. Dates, comments, relationships, attachments, other objects, and
outbound fields other than status are outside this integration.

The task object must expose active `title` TEXT, `bodyV2` RICH_TEXT, and `status`
SELECT fields. Legacy body-only schemas are unsupported. A null body is empty;
nonempty blocknote content without Markdown on a task after the cutoff fails
content preparation instead of silently dropping it. Status-only reads can still
deliver pending changes when content conversion fails.

## Two-way status and recovery

Only explicit Kata close/reopen events accepted while two-way is configured
queue writes. Initial opt-in produces no bulk outbound changes. A close selects
`closed_status`; reopen selects `open_status`. An already matching open substate,
such as `IN_PROGRESS`, is preserved. Pending local intent wins until a fresh
readback verifies delivery for that exact event. Writes change only task status.

PATCH is attempted once and never blindly retried. A lost reply or failed
readback retains pending intent; a later scan checks the current state before
writing again. Missing/deleted tasks, mismatched workspace identity, unsupported
metadata, or removed targets block delivery and retain intent. Status shows the
mode, pending count, and sanitized error; `once` reports incoming status updates
separately from content import counts.

Disable pauses polling and delivery while retaining mappings and pending intent.
Returning to one-way cancels pending writes, including when a retained target has
been removed. JSONL restore clears private observations and pending intent and
leaves polling disabled until explicit re-enable.

## Progress and bounds

Live phases are `workspace`, `schema`, `tasks`, `content`, `importing`, and
`finalizing`. A zero total means unknown. Manual and scheduled runs share durable claims, pacing,
progress, and committed-event delivery. A second run conflicts while one is
active. Disable/re-enable fences the earlier binding snapshot; the existing
claim remains until the worker retires or the 30-minute stale-claim horizon
allows recovery.

Runs have a 20-minute deadline, requests a 30-second timeout, and enable
validation a two-minute deadline. All bindings share at most one request per
second per daemon. Each run checks workspace identity and loads task metadata
once, so a status read then costs one request and a status write three: read,
PATCH, and verified readback. Read requests allow four attempts for HTTP
429/server errors; `Retry-After` cooldowns are capped at 20 minutes. Bindings
run serially, so a slow workspace delays others.

Limits are 10,000 tasks/metadata records, 1,000 pages per collection, 8 MiB per
response, 128 MiB raw responses per collection, 1 MiB task Markdown, and 64 MiB
serialized import items. These are failure guards rather than supported-size
promises. Incomplete fields, duplicate identities, repeated/missing cursors, and
exceeded bounds fail preparation before any content import. Metadata object and
field collections paginate separately.

For credential errors, check the selected environment variable in the daemon's
service environment. For mapping errors, inspect API option values and re-enable
with a complete classification. For instance compatibility errors, check the
required metadata and core task fields above. Failed runs retain native issues,
pending intent, and the last successful cursor for the next attempt.
