---
title: Notion sync
description: Mirror a Notion task data source into Kata with workflow groups, optional two-way status, and bounded polling.
last_edited: 2026-11-20
---

# Notion sync

Notion sync mirrors one Notion task data source into one Kata project. The
daemon reads Notion and imports native issues on a five-minute polling interval
by default. Status sync defaults to one-way, and you can opt into two-way status
to send your explicit Kata close and reopen actions back to the Notion task.
Other local edits stay in Kata.

## Configure the daemon

Create a Notion internal connection with read-content access and share the task
database with it. Provide its token on the **daemon host** through the daemon's
service environment. Kata reads `KATA_NOTION_TOKEN` by default; to use a
different environment variable, configure it in `<KATA_HOME>/config.toml`:

```toml
[notion_sync]
token_env = "EXAMPLE_NOTION_TOKEN"
```

After you change the configuration or service environment, restart the daemon.
Kata resolves the token for each run, shares it across bindings, and never
stores it in binding config, JSONL exports, or command output. All upstream
requests go to fixed `https://api.notion.com:443` with `Notion-Version: 2026-03-11`,
and Kata rejects redirects. One-way sync requires read-content access; two-way
status also requires update-content access, and Kata writes only to the selected
status property.

In [remote-client mode](remote-daemon.md), configure the token on the remote
daemon and target it with `KATA_SERVER` or `--daemon`. Your client workstation
needs only its ordinary daemon authentication, with no Notion credential. If the
daemon must keep polling without connected clients, disable idle shutdown by
setting `autostart_idle_timeout = "0"`.

## Enable a source

From a bound project workspace, enable sync and run a first check:

```sh
kata sync notion enable \
  --data-source 11111111-1111-4111-8111-111111111111 \
  --status-property 'Workflow' --assignee-property 'Responsible' \
  --done-status 'Delivered' --done-status 'Accepted' --interval 5m
kata sync notion once
kata sync notion status
```

Initial enablement requires exactly one locator. If you omit `--done-status`,
completion follows the selected status property's live Complete group. Before
saving the binding, the daemon checks access and the current schema. Untitled
data sources appear as `Notion data source <id>`.

An enabled binding joins the daemon's polling schedule, while `once` starts a
run immediately and waits for the result. Manual and scheduled runs share a
durable claim, and the daemon rejects a second run while that claim is active.
Running disable and re-enable fences the earlier binding snapshot while keeping
its claim until that worker retires, so a replacement worker cannot overlap an
admitted status write.

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

Selectors match exact returned IDs or exact case-sensitive names. If an ID,
name, or property discovery is ambiguous, the daemon fails and lists the
available choices. Kata discovers the unique title property automatically.
Completion tracks live membership of the saved Complete group: renaming the
group keeps that group identity intact, and adding or moving options into the
group changes completion on the next observation. Passing `--done-status`
keeps the legacy one-way option-set mode, where only the selected IDs count as
complete.

You can pass database UUIDs with or without hyphens. Database URLs must use
HTTPS on `notion.so`, `www.notion.so`, `notion.com`, `www.notion.com`, or
`app.notion.com`, with the UUID in the final path segment. Hostnames are
case-insensitive, and a trailing slash is accepted. The CLI extracts the UUID
locally and never requests or follows the URL. Query strings and fragments do
not select a view or data source. Kata rejects custom domains, credentials in
URLs, nondefault ports, ambiguous paths, and short task links. Passing explicit
UUIDs stays reliable if Notion changes its URL formats.

## Saved mappings and cutoffs

Each Kata project holds at most one external issue-sync binding, so GitHub and
Notion cannot own the same project at the same time. Once bound, the Notion
source, title, status, and people property IDs, selected workflow group IDs, and
legacy completed option IDs stay fixed, even after you disable the binding.
Omitting selectors on re-enable keeps those saved IDs. Renaming a property or
option continues to work by ID, whereas removing a selected property or option
or changing a selected property type fails the run without advancing the cursor.
Removing a selected workflow group also fails runs until you restore the schema.
Retargeting or remapping imported issues is outside v1.

The binding's display name tracks the Notion data-source title. If the source
has no title, Kata displays `Notion data source <data_source_id>` until Notion
returns one.

You can re-enable a binding using its saved source and selections:

```sh
kata sync notion enable --interval 10m
kata sync notion enable --since 2026-01-01
kata sync notion enable --since ''
```

When you re-enable, omitting the interval, cutoff, or title-prefix flag keeps
your saved value. Passing an empty `--since` clears the cutoff. Dates mean
midnight UTC, whole-second RFC3339 offsets normalize to UTC, and Kata rejects
fractional seconds. Changing the config resets the cursor, while changing only
the interval preserves it. Clearing a cutoff can make older pages eligible
again, though equal source timestamps never overwrite local scalar edits.
Successful runs record their run-start cursor with a two-minute overlap for
incremental queries. This is eventual polling rather than an atomic Notion
snapshot. Kata reads title and people twice to detect changes between those
reads, and reads markdown only once per attempt. Those checks do not guarantee a
consistent page body or catch later edits that reuse an already imported source
timestamp.

## Two-way status

```sh
kata sync notion enable \
  --data-source 11111111-1111-4111-8111-111111111111 \
  --status-sync=two-way
```

Before sending a new status mode or a group or target selector, the CLI checks
the daemon's `issue_status_sync` capability. Upgrade an older daemon if it lacks
that support. Existing legacy option-set bindings keep their option mapping
until you explicitly opt into two-way status, which switches completion to
workflow groups while keeping the source and property IDs fixed.

Closing a Kata issue writes an option from the Complete group, and reopening
writes one from the To-do group. If you omit an explicit write target, Kata uses
the group's first option in the current returned `option_ids` order. Re-enabling
keeps your explicit target overrides; passing `--closed-status=` or
`--open-status=` clears the override and returns to the live group default.
Explicit targets must belong to their selected groups. When Notion already
matches the open or closed state, Kata skips the write, preserving Notion's
in-progress substates and null open statuses. Enabling two-way status does not
push historical local closures. Unrelated page edits do not undo a local
closure, and status observations have their own baseline.

Running `disable` pauses imports and status writes while keeping pending intent.
Explicit Kata close and reopen actions while paused still update that intent,
and re-enabling resumes delivery. Switching to `--status-sync=one-way` cancels
pending outbound intent while keeping group-based completion; turning two-way
back on later does not recreate old local actions. Pending intent survives
provider failures until verified delivery or a newer local action. Inspect the
latest error and fix any access or schema problem.

## Imported issues and local work

By default, Kata prefixes imported titles with `[Notion] `, turning an empty
title into `[Notion] (untitled)`. To keep original titles and add the plain
`notion` label instead, pass `--title-prefix=false`; empty titles then become
`(untitled)`. Re-enable with `--title-prefix=true` to restore prefixing.
Changing this setting resets the cursor and refreshes source-owned titles even
when the source timestamp has not changed, while keeping your local title edits
intact. Kata adds or removes its source-managed presentation tag without losing
local labels, and a pre-existing local `notion` label stays local.

Each imported issue body contains the complete page Markdown followed by an
attribution link. A selected completed option or a live Complete-group member
maps to `closed` with reason `done`, while all other statuses (including a null
status) map to `open`. Kata does not infer native in-progress states.

The first user in the complete selected people property becomes the issue owner
as `notion:<user_uuid>`. Kata skips groups and leaves the issue unassigned if no
user is listed. The page creator becomes the author in the same stable ID
format, or `notion-unknown` when absent. Kata does not automatically map these
external identities to local users.

Notion controls an imported issue's title, body, and owner whenever its source
timestamp is strictly newer than the stored issue timestamp. Local edits with a
newer timestamp stay in place until Notion is edited again later, and equal or
older source observations preserve local scalar edits. Because Notion supplies
nil priority, a strictly newer import can clear a local priority. Kata does not
import or send upstream native due dates, comments, upstream labels, or
relations.

Two-way mode manages status independently of content timestamps and preserves
pending explicit local close and reopen actions until delivery. The `notion`
presentation label is the only source-managed label. It can refresh at the
latest observed source timestamp even after local scalar edits, while older
replays leave it alone. If you remove a source-managed `notion` label by hand,
the next fetch of that page restores it with an `issue.labeled` event, even if
the page itself has not changed.

Deleting, archiving, trashing, or moving a Notion page never deletes or closes
its Kata issue. Kata skips unavailable or moved pages when it observes them
safely, while a page that becomes unavailable during content reads fails that
preparation pass. Kata retains imported issues rather than reconciling them by
absence. Disabling sync keeps your issues, binding, and import mappings. JSONL
restore keeps bindings disabled until an operator re-enables them. Ordinary
restore also clears provider-status observations, pending outbound status
intent, and additional API locators, while automatic database cutover preserves
that state. Federation and external-root ownership rules still apply.

## Status and recovery

```sh
kata sync notion status
kata --agent sync notion status
kata --json sync notion status
kata sync notion disable
```

Human and agent output show the source, resolved database, property, and group
IDs, explicit targets or `live group default`, status mode, pending status
count, interval, saved cutoff, timestamps, last error, and optional live
progress. Running status does not query the upstream schema; the worker resolves
group membership and default targets live when it observes or writes status. A
zero progress total means the total is unknown. `last_created`, `last_updated`,
and `last_unchanged` are **last successful run** counts and remain historical
while a later run is active or has failed. `--json` returns the daemon response
with non-secret resolved config. `not_enabled` means no binding exists, while
`disabled` means the saved binding is paused. `once` requires an enabled binding
and reports `status_updated` for inward open and closed transitions committed by
its independent status pass, separate from content import counters.

After a failure, check the last error along with source access, the token and
service environment, and the selected schema IDs. Content-read and mapping
errors include the page UUID so you can locate the failing page. Once you fix
the cause, run `once` or wait for the next scheduled poll. A failed run does not
advance its cursor. Preparation finishes before any import begins; if a later
chunk fails, already committed import chunks and their events stay durable, and
a retry safely replays them. Disabling stops polling and transactionally fences
further imports. Committed chunks stay in place, though active upstream reads
can continue after `disable` returns.

Each run has a 25-minute deadline, and each request has a 30-second timeout. All
Notion bindings share a rate limit of at most three requests per second. Every
eligible page needs at least seven requests (title, people, markdown, and page
metadata, followed by a second read of title, people, and page metadata). That
puts an optimistic ceiling at roughly **640 changed pages per run**, which is
not a supported database size. Schema and query reads, latency, property
pagination, re-reads, retries, and concurrent enable requests reduce that number
further. Initial sync reads every eligible page, and a backlog after an outage
hits the same ceiling.

Preparation reads every eligible page before importing any of them. If that work
exceeds the deadline, Kata imports nothing and leaves the cursor unchanged, so
repeated polls can time out indefinitely. Notion bindings run serially, and one
oversized binding can delay the others by 25 minutes on each polling pass. If a
binding stalls, disable it while you shrink its source or narrow `--since`. A
later cutoff skips older tasks without backfilling them. Resumable content
batches are outside v1.

Page bodies come directly from Notion's
[Markdown endpoint](https://developers.notion.com/reference/retrieve-page-markdown).
Sync keeps the returned Markdown and appends source attribution instead of
fetching or converting block JSON. Notion's
[enhanced Markdown](https://developers.notion.com/guides/data-apis/enhanced-markdown)
can include tags for callouts, toggles, columns, and mentions, and those tags
stay in the imported body. Unsupported content can appear as visible
placeholders, and media URLs supplied by Notion can expire.

Separate hard read limits guard each run: 10,000 unique pages, 1,000 query
response pages, 100 response pages or 10,000 values per property, 8 MiB per
decoded response, 1 MiB of markdown per page, and 64 MiB of serialized import
items per run. The 10,000-page cap guards against excessive reads rather than
promising that a run can finish that many pages. Rather than silently importing
partial content, Kata fails a run on incomplete query results, repeated or
missing pagination cursors, markdown with `truncated=true` or nonempty
`unknown_block_ids`, content that changes during both read attempts, or exceeded
limits. When none of those failure signals appear, unsupported blocks can remain
as visible placeholders.

Transient failures retry up to five attempts with exponential backoff, jitter,
and `Retry-After` handling, with `Retry-After` cooldowns capped at 25 minutes.
If a shared cooldown cannot finish before the caller's deadline, the error
reports the upstream failure, for example HTTP 429 `rate_limited`, along with
the remaining retry delay. Webhook sync is outside v1.
