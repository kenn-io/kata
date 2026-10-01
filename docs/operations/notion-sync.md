---
title: Notion sync
description: Mirror a Notion task data source into Kata with workflow groups, optional two-way status, and bounded polling.
last_edited: 2026-09-29
---

# Notion sync

Notion sync mirrors one task data source into one Kata project. The daemon reads
Notion and imports native issues. Status sync defaults to one-way; opt into
two-way status to send explicit Kata close and reopen actions back to the task.
Other local edits remain local. Polling defaults to five minutes.

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
rejected. One-way sync requires read-content access. Two-way status also needs
update-content access; Kata writes only the selected status property.

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

Initial enable requires exactly one locator. Without `--done-status`, completion
uses the selected status property's live Complete group.
The daemon validates access and the current schema before saving the binding.
Untitled data sources display as `Notion data source <id>`.
An enabled binding participates in daemon polling; `once` starts a run now and
waits for its result. Manual and scheduled runs share a durable claim. A second
run is rejected while that claim remains active. Disable and re-enable fence
the earlier binding snapshot but retain its claim until that worker retires,
so a replacement worker cannot overlap an admitted status write.

| Enable flag | Meaning |
| --- | --- |
| `--data-source UUID` | Explicit task data source UUID; preferred when a database has multiple sources. |
| `--database UUID-or-URL` | Convenience locator for a database containing exactly one data source. |
| `--status-property ID-or-name` | Status property; omitted selects the sole property of type `status`. |
| `--assignee-property ID-or-name` | A People property is required; omit the selector only when exactly one exists. |
| `--status-sync one-way-or-two-way` | Status direction; omitted preserves the saved mode and new bindings default to one-way. |
| `--complete-group ID-or-name` | Complete workflow group; defaults to exact name `Complete`. |
| `--todo-group ID-or-name` | To-do workflow group; defaults to exact name `To-do` and is required for two-way status. |
| `--closed-status ID-or-name` | Optional closed write target within Complete; empty clears the override. |
| `--open-status ID-or-name` | Optional open write target within To-do; empty clears the override. |
| `--done-status ID-or-name` | Legacy one-way completed option set; repeat for each option. Rejected with two-way status. |
| `--interval duration-or-seconds` | Polling interval of at least one second; initially defaults to `5m`. |
| `--title-prefix[=true-or-false]` | Initially true; false keeps source titles and adds the plain `notion` label. Omitted preserves the saved choice. |
| `--since date-or-timestamp` | Import pages edited after a UTC date or whole-second RFC3339 timestamp. |

Selectors use exact returned IDs or exact case-sensitive names. Ambiguous IDs,
names, or property discovery fail with choices from the daemon. Kata discovers
the unique title property. Completion follows live membership of the saved Complete group. Display renames
preserve that group identity, and adding or moving options into the group changes
completion on the next observation. Explicit `--done-status` retains the legacy
one-way option-set mode: only the selected IDs count as complete.

Database UUIDs may have or omit hyphens. URLs must use HTTPS and `notion.so`,
`www.notion.so`, `notion.com`, `www.notion.com`, or `app.notion.com`, with the UUID
in the final path segment. Hostnames are case-insensitive and a trailing slash
is accepted. The CLI extracts the UUID locally; it never requests
or follows that URL. Query strings and fragments do not select a view or source.
Custom domains, credentials in URLs, nondefault ports, ambiguous paths, and short
task links are rejected. Explicit UUIDs remain reliable if URL formats change.

## Saved mappings and cutoffs

A project has one external issue-sync binding, so GitHub and Notion cannot own
it simultaneously. Notion source, title/status/people property IDs, selected workflow group IDs,
and legacy completed option IDs stay fixed after binding, including after
disable. Omitted selectors preserve the saved IDs. Renaming a
property or option works by ID. Removing a selected property or option, or
changing a selected property type, fails the run without advancing its cursor.
Removing a selected workflow group also fails until the schema is restored.
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

## Two-way status

```sh
kata sync notion enable \
  --data-source 11111111-1111-4111-8111-111111111111 \
  --status-sync=two-way
```

The CLI checks the daemon's `issue_status_sync` capability before sending a new
mode or group/target selector. Upgrade an older daemon when it lacks support.
Existing legacy option-set bindings keep that mapping until explicit two-way
opt-in switches completion to workflow groups. Source and property IDs stay fixed.

Close writes an option from Complete; reopen writes one from To-do. An omitted
write target uses the group's first option in the current returned `option_ids`
order. Re-enable preserves explicit target overrides; `--closed-status=` or
`--open-status=` clears one and resumes the live default. Targets must belong
to their selected groups. Matching binary state causes no write, preserving
Notion's in-progress substates and null open statuses. Enabling two-way does not
send historical local closures. Unrelated page edits do not undo a local closure;
status observations have their own baseline.

`disable` pauses imports and status writes while retaining pending intent.
Explicit Kata close/reopen while paused still updates that intent. Re-enable
resumes it. Switching to `--status-sync=one-way` cancels pending outbound intent
and keeps group-based completion. A later two-way enable does not recreate old
local actions. Pending intent survives provider failures until verified delivery
or a newer local action; inspect the latest error and fix access or schema issues.

## Imported issues and local work

Titles receive `[Notion] ` by default; an empty title becomes `[Notion] (untitled)`.
Pass `--title-prefix=false` to retain original titles and add the plain `notion`
label; empty titles then become `(untitled)`. Re-enable with `--title-prefix=true`
to restore prefixing. Changing this setting resets the cursor and refreshes
source-owned titles even at an unchanged source timestamp. Local title edits
remain intact. Kata adds/removes its source-managed presentation tag without
losing local labels; a pre-existing local `notion` label remains local. The body
contains complete page markdown and an attribution link. A selected completed
option or live Complete-group member maps to `closed` with reason `done`; all other statuses, including a null
status, map to `open`. Native in-progress states are not inferred.

The first user in the complete selected people property becomes owner
`notion:<user_uuid>`; groups are skipped and no user means unassigned. The creator
becomes author with the same stable ID format, or `notion-unknown` when absent.
These external identities are not automatically mapped to local users.

Notion controls imported title, body, and owner when its source timestamp
is strictly newer than the stored issue timestamp. Local edits with a newer
timestamp remain until Notion is edited again later. Equal or older source
observations preserve local scalar edits. Notion supplies nil priority, so a
strictly newer import can clear a local priority. Native due dates, comments, upstream labels, and relations are not imported
or sent upstream. Two-way mode handles status independently of content timestamps
and preserves pending explicit local close/reopen actions until delivery. The `notion` presentation label is the only source-managed label.
It can refresh at the latest observed source timestamp even after local scalar
edits; older replays leave it alone. If you remove a source-managed `notion`
label by hand, the next fetch of that page restores it with an `issue.labeled`
event, even if the page has not changed.

Deleting, archiving, trashing, or moving a Notion page does not delete or close its
existing Kata issue. Unavailable/moved pages are skipped when safely observed;
a page becoming unavailable during content reads fails that preparation. Imports
are retained rather than reconciled by absence. Disabling sync preserves issues,
binding, and import mappings. JSONL restore keeps bindings disabled until an
operator re-enables them. Ordinary restore also clears provider-status
observations, pending outbound status intent, and additional API locators.
Automatic database cutover preserves that state. Federation and external-root
ownership rules still apply.

## Status and recovery

```sh
kata sync notion status
kata --agent sync notion status
kata --json sync notion status
kata sync notion disable
```

Human and agent output show the source, resolved database/property/group IDs,
explicit targets or `live group default`, status mode, pending status count,
interval, saved cutoff, timestamps, last error, and optional live progress.
Status does not request the upstream schema; group membership and default
targets are resolved live when the worker observes or writes status.
A zero progress total means the total is unknown. `last_created`, `last_updated`,
and `last_unchanged` are **last successful run** counts; they remain historical
while a later run is active or has failed. JSON returns the daemon response with
non-secret resolved config. `not_enabled` means no binding exists; `disabled`
means the saved binding is paused. `once` requires an enabled binding and reports
`status_updated` for inward open/closed transitions committed by its independent
status pass. Content import counters remain separate.

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

Page bodies come directly from Notion's
[Markdown endpoint](https://developers.notion.com/reference/retrieve-page-markdown).
Sync preserves the returned Markdown and adds source attribution; it does not
fetch or convert block JSON. Notion's
[enhanced Markdown](https://developers.notion.com/guides/data-apis/enhanced-markdown)
can include tags for callouts, toggles, columns, and mentions. Those tags remain
in the imported body. Unsupported content may appear as visible placeholders,
and media URLs supplied by Notion can expire.

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
