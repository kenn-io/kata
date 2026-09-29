---
title: Notion sync
description: Mirror a Notion task data source into Kata with read-only daemon credentials, explicit completion mappings, and bounded polling.
last_edited: 2026-09-28
---

# Notion sync

Notion sync mirrors one task data source into one Kata project. The daemon reads
Notion and imports native issues. Task completion and reopening arrive on the
next successful poll, subject to source and local edit timestamps. Local edits,
including closing an issue, never update Notion. Polling defaults to five minutes.

## Configure the daemon

Create a Notion internal connection with read-content access and share the task
database with it. Supply its token on the **daemon host**, through the daemon's
service environment. The default environment variable is `KATA_NOTION_TOKEN`.
To use another variable, set this in `<KATA_HOME>/config.toml`:

```toml
[notion_sync]
token_env = "EXAMPLE_NOTION_TOKEN"
```

Restart the daemon after changing its configuration or service environment.
Tokens are resolved for each run, shared across bindings, and never stored in
binding config, JSONL, or command output. All upstream requests go to fixed
`https://api.notion.com:443`, with `Notion-Version: 2026-03-11`; redirects are
rejected. The connection needs no upstream write or comment permissions.

In [remote-client mode](remote-daemon.md), configure the token on the remote
daemon and select it with `KATA_SERVER` or `--daemon`. The client workstation
needs its ordinary daemon authentication, but no Notion credential. For a daemon
that must keep polling without connected clients, disable idle shutdown with
`autostart_idle_timeout = "0"`.

## Enable a source

In a bound project workspace:

```sh
kata sync notion enable \
  --data-source 11111111-1111-4111-8111-111111111111 \
  --status-property 'Workflow' --assignee-property 'Responsible' \
  --done-status 'Delivered' --done-status 'Accepted' --interval 5m
kata sync notion once
kata sync notion status
```

Initial enable requires exactly one locator and at least one completed option.
The daemon validates access and the current schema before saving the binding.
Untitled data sources display as `Notion data source <id>`.
An enabled binding participates in daemon polling; `once` starts a run now and
waits for its result. Manual and scheduled runs share a durable claim. A second
run is rejected while that claim remains active. Re-enabling clears the claim,
so a new run may start while the earlier run still reads Notion; the earlier
run can no longer import or advance the cursor.

| Enable flag | Meaning |
| --- | --- |
| `--data-source UUID` | Explicit task data source UUID; preferred when a database has multiple sources. |
| `--database UUID-or-URL` | Convenience locator for a database containing exactly one data source. |
| `--status-property ID-or-name` | Status property; omitted selects the sole property of type `status`. |
| `--assignee-property ID-or-name` | A People property is required; omit the selector only when exactly one exists. |
| `--done-status ID-or-name` | Completed option of the selected status property; repeat for each option. |
| `--interval duration-or-seconds` | Polling interval of at least one second; initially defaults to `5m`. |
| `--title-prefix[=true-or-false]` | Initially true; false keeps source titles and adds the plain `notion` label. Omitted preserves the saved choice. |
| `--since date-or-timestamp` | Import pages edited after a UTC date or whole-second RFC3339 timestamp. |

Selectors use exact returned IDs or exact case-sensitive names. Ambiguous IDs,
names, or property discovery fail with choices from the daemon. Kata discovers
the unique title property. It never infers completion from English names,
colors, option order, or status groups. New options remain open unless their IDs
belong to the originally selected completed set.

Database UUIDs may have or omit hyphens. URLs must use HTTPS and `notion.so`,
`www.notion.so`, `notion.com`, `www.notion.com`, or `app.notion.com`, with the UUID
in the final path segment. Hostnames are case-insensitive and a trailing slash
is accepted. The CLI extracts the UUID locally; it never requests
or follows that URL. Query strings and fragments do not select a view or source.
Custom domains, credentials in URLs, nondefault ports, ambiguous paths, and short
task links are rejected. Explicit UUIDs remain reliable if URL formats change.

## Saved mappings and cutoffs

A project has one external issue-sync binding, so GitHub and Notion cannot own
it simultaneously. Notion source, title/status/people property IDs, and completed
option IDs are immutable after binding, including after disable. Renaming a
property or option works by ID. Removing a selected property or option, or
changing a selected property type, fails the run without advancing its cursor.
Retargeting or remapping imported issues is outside v1.

The binding display name follows the Notion data-source title. If the source has
no title, Kata displays `Notion data source <data_source_id>` until Notion
returns a name.

Re-enable can reuse the saved source and selections:

```sh
kata sync notion enable --interval 10m
kata sync notion enable --since 2026-01-01
kata sync notion enable --since ''
```

An omitted interval, cutoff, or title-prefix choice preserves its saved value
on re-enable. An explicit empty `--since` clears the cutoff. Dates mean midnight UTC; whole-second RFC3339
offsets normalize to UTC. Fractional seconds are rejected. A config change resets
the cursor; changing only the interval preserves it. Clearing a cutoff may make
older pages eligible, but equal source timestamps do not replace local scalar
edits. Successful runs retain their run-start cursor, with a two-minute overlap
for incremental queries; this is eventual polling, not an atomic Notion snapshot.
Kata reads title and people twice to detect changes between those reads, but
reads markdown only once per attempt. These checks do not guarantee a consistent
page body or detect later edits that reuse an already imported source timestamp.

## Imported issues and local work

Titles receive `[Notion] ` by default; an empty title becomes `[Notion] (untitled)`.
Pass `--title-prefix=false` to retain original titles and add the plain `notion`
label; empty titles then become `(untitled)`. Re-enable with `--title-prefix=true`
to restore prefixing. Changing this setting resets the cursor and refreshes
source-owned titles even at an unchanged source timestamp. Local title edits
remain intact. Kata adds/removes its source-managed presentation tag without
losing local labels; a pre-existing local `notion` label remains local. The body
contains complete page markdown and an attribution link. A selected completed
option maps to `closed` with reason `done`; all other statuses, including a null
status, map to `open`. Native in-progress states are not inferred.

The first user in the complete selected people property becomes owner
`notion:<user_uuid>`; groups are skipped and no user means unassigned. The creator
becomes author with the same stable ID format, or `notion-unknown` when absent.
These external identities are not automatically mapped to local users.

Notion controls imported title, body, state, and owner when its source timestamp
is strictly newer than the stored issue timestamp. Local edits with a newer
timestamp remain until Notion is edited again later. Equal or older source
observations preserve local scalar edits. Notion supplies nil priority, so a
strictly newer import can clear a local priority. Native due dates, comments,
upstream labels, relations, and completion write-back are not imported or sent
upstream. The `notion` presentation label is the only source-managed label.
It can refresh at the latest observed source timestamp even after local scalar
edits; older replays leave it alone. If you remove a source-managed `notion`
label by hand, the next fetch of that page restores it with an `issue.labeled`
event, even if the page has not changed.

Deleting, archiving, trashing, or moving a Notion page does not delete or close its
existing Kata issue. Unavailable/moved pages are skipped when safely observed;
a page becoming unavailable during content reads fails that preparation. Imports
are retained rather than reconciled by absence. Disabling sync preserves issues,
binding, and import mappings. JSONL restore keeps bindings disabled until an
operator re-enables them. Federation and external-root ownership rules still apply.

## Status and recovery

```sh
kata sync notion status
kata --agent sync notion status
kata --json sync notion status
kata sync notion disable
```

Human and agent output show the source, resolved database/property/completion
IDs, interval, saved cutoff, timestamps, last error, and optional live progress.
A zero progress total means the total is unknown. `last_created`, `last_updated`,
and `last_unchanged` are **last successful run** counts; they remain historical
while a later run is active or has failed. JSON returns the daemon response with
non-secret resolved config. `not_enabled` means no binding exists; `disabled`
means the saved binding is paused. `once` requires an enabled binding.

After a failure, check the last error and source access, token/service environment,
and selected schema IDs. Content-read and mapping errors include the page UUID
so you can locate the failing page. Fix the cause, then run `once` or let polling
retry.
A failed run does not advance its cursor. Preparation completes before imports
start; committed import chunks and their events remain durable if a later chunk
fails, and retry safely replays them. Disabling stops polling and transactionally
fences further imports. Already committed chunks remain; active upstream reads
may continue after disable returns.

A run has a 25-minute deadline and each request a 30-second timeout. All Notion
bindings share pacing of at most three requests per second. Each eligible page
needs at least seven requests: title, people, markdown, and page metadata,
then a second read of title, people, and page metadata.
That makes roughly **640 changed pages per run an optimistic ceiling**, not a
supported database size. Schema/query reads, latency, property pagination,
re-reads, retries, and concurrent enable requests lower it. Initial sync reads
all eligible pages; the same ceiling applies to a backlog after an outage.

Preparation reads every eligible page before importing any. If that work exceeds
the deadline, nothing is imported and the cursor stays unchanged, so repeated
polls can time out indefinitely. Notion bindings run serially; one oversized
binding can delay the others by 25 minutes each polling pass. Disable a stalled
binding while reducing its source or narrowing `--since`. A later cutoff skips
older tasks; it does not backfill them. Resumable content batches are outside v1.

The separate hard read limits are 10,000 unique pages, 1,000 query response pages,
100 response pages or 10,000 values per property, 8 MiB decoded response, 1 MiB
markdown per page, and 64 MiB serialized import items per run. The 10,000-page
limit is a guard against excessive reads, not a promise that a run can complete
that many pages. Incomplete query results, repeated/missing pagination cursors,
markdown with `truncated=true` or nonempty `unknown_block_ids`, changed content
during both read attempts, and exceeded limits fail rather than silently
importing partial content. Unsupported blocks can remain as visible placeholders
when those failure signals are absent.

Transient failures retry up to five attempts with exponential backoff, jitter,
and `Retry-After`. Retry-After cooldowns are capped at 25 minutes. If a shared
cooldown cannot finish before the caller's deadline, the error reports the
upstream failure, such as HTTP 429 `rate_limited`, and the remaining retry delay.
Webhook sync is outside v1.
