---
title: Plane sync
description: Mirror a Plane project into native Kata issues with daemon-owned API credentials and optional two-way status sync.
last_edited: 2026-11-20
---

# Plane sync

Plane sync mirrors one Plane project into one Kata project. By default, the
daemon reads Plane and imports native issues every five minutes, while local
issue edits stay in Kata. To send explicit Kata close and reopen actions back to
Plane, enable `--status-sync=two-way`; other fields remain incoming-only.

## Configure the daemon

Create a Plane API key that can read the selected project, its states, and its
work items. Scoped keys need `projects:read`, `projects.states:read`, and
`projects.work_items:read`. Two-way status also needs
`projects.work_items:write`. Expose the key as `KATA_PLANE_TOKEN` in the
daemon's service environment. Your client workstation needs only its normal Kata
daemon credentials. See Plane's
[API authentication guide](https://developers.plane.so/api-reference/introduction).

Plane Cloud defaults to `https://api.plane.so` for API requests and
`https://app.plane.so` for attribution links. For self-hosted installations, set
root origins and an optional custom environment variable in
`<KATA_HOME>/config.toml`:

```toml
[plane_sync]
api_origin = "https://plane.example"
web_origin = "https://plane.example"
token_env = "EXAMPLE_PLANE_TOKEN"
```

If you omit `web_origin`, Kata uses the API origin, except that the Cloud
default uses `https://app.plane.so`. Origins must use HTTPS; only literal
loopback IPs may use HTTP. Kata rejects path-prefixed deployments, embedded
credentials, query strings, and fragments. It sends API keys only to the
configured API origin in `X-API-Key` headers and rejects redirects. Links and
images in descriptions are converted locally without fetching their targets, and
API keys never enter binding config, exports, or command output. Restart the
daemon after changing its configuration or service environment. Each run
captures its key once, and later runs see rotated keys.

For remote-client use, configure Plane on the selected remote daemon. If polling
must continue without connected clients, set `autostart_idle_timeout = "0"`.
Embedded services configure the same values through `Config.PlaneSync`.

## Enable and operate

From a bound Kata workspace, enable sync and inspect its status:

```sh
kata sync plane enable \
  --plane-workspace example-workspace \
  --plane-project 11111111-1111-4111-8111-111111111111
kata sync plane once
kata sync plane status
kata --agent sync plane status
kata --json sync plane status
kata sync plane disable
```

Initial enablement requires the Plane workspace slug and project UUID. Kata
normalizes UUIDs with or without hyphens to lowercase hyphenated IDs, and
workspace slugs are case-sensitive URL-safe names. Kata's global `--workspace`
selects the local workspace directory, while global `--project` selects the
native Kata project. Before saving the binding, the daemon validates upstream
project access and state groups. Plane sync requires the current `work-items/`
API and does not use legacy `issues/` endpoints.

| Enable option | Behavior |
| --- | --- |
| `--plane-workspace` | Plane workspace slug; required initially, then immutable. |
| `--plane-project` | Plane project UUID; required initially, then immutable. |
| `--interval` | Duration or integer seconds, at least one second; initially `5m`. |
| `--since` | Exclusive updated-after UTC date or whole-second RFC3339 timestamp. |
| `--status-sync` | `one-way` (initial default) or `two-way`; omission preserves the saved mode. |
| `--closed-state` | Optional completed-state UUID; empty clears the override. |
| `--open-state` | Optional unstarted-state UUID; empty clears the override. |
| `--title-prefix=false` | Keep source titles and add the source-managed `plane` label. |

On re-enable, you can omit every option to reuse your saved settings. Pass an
empty `--since ''` to clear the cutoff, or `--title-prefix=true` to restore
title prefixes. Dates mean midnight UTC, timestamp offsets normalize to UTC, and
Kata rejects fractional seconds. Config changes reset the stored cursor, while
interval-only changes and status-only changes (`--status-sync`,
`--closed-state`, `--open-state`) preserve it. The cursor records successful run
starts for diagnostics. Every run traverses the whole collection and applies the
cutoff locally, so workflow schema changes and items missed during concurrent
pagination can arrive on a later pass. This is eventual polling rather than an
atomic Plane snapshot.

Each native project has at most one external issue-sync binding. GitHub, Notion,
and Plane bindings cannot replace one another, even after you disable them. A
Plane binding's API origin, workspace, and project identity stay fixed. Changing
the daemon's API origin causes existing bindings to fail before credentials are
resolved; use a separate Kata project for another source. Re-enabling can
refresh the binding's web origin for links. Federation spokes receive imports
through their hub. Enable Plane sync on the hub. Existing external-root
ownership protections apply to managed issues.

## Mapping and local edits

| Plane value | Kata issue |
| --- | --- |
| Name and sequence | `[Plane EX-12] Original title`; blank names become `(untitled)`. |
| Description HTML | Markdown with a Plane attribution link; tables, lists, links, and images are retained when convertible. Unsupported or malformed link targets are removed while link text is kept. |
| Mention tags (`<mention-component>`) | Labels stored in tag attributes are omitted; literal text and surrounding description text are retained. |
| Priority | `urgent` → P0, `high` → P1, `medium` → P2, `low` → P3, `none` → unassigned. |
| `backlog`, `unstarted`, `started`, `triage` state groups | `open`. |
| `completed` group | `closed`, reason `done`. |
| `cancelled` group | `closed`, reason `wontfix`. |
| First assignee UUID | Owner `plane:<uuid>`; no assignees means unassigned. |
| Creator UUID | Author `plane:<uuid>`, or `plane-unknown` when absent. |
| Source timestamps | Created/updated timestamps normalized to persisted milliseconds; status observations use `completed_at` for completion when supplied, otherwise updated time. Cancellation uses updated time. |

Kata does not map external UUID identities to local users automatically. State
names, colors, and ordering do not affect completion, and unknown state groups
or references fail preparation. Upstream dates, labels, comments, relationships,
cycles, and modules are outside this import. Priority follows Plane on strictly
newer imports, and an explicit `none` clears it.

Source scalar edits apply only when their updated timestamp is strictly newer
than the native issue. Newer local edits stay in place until Plane has a later
source edit, while equal or older observations preserve title, body, owner, and
priority. A state-group edit can leave a work item's timestamp unchanged, and
status alone reconciles through independent workflow observations. Those
observations do not depend on the content cutoff or the native issue update
time, and they preserve local title, body, owner, and priority edits.

Changing title-prefix presentation refreshes source-owned titles at the same
source version while keeping your local title edits intact. Kata manages the
`plane` label separately and can refresh it after local scalar edits without
touching local labels. Archived, deleted, or unavailable work items stay in
Kata; absence never closes or deletes an imported issue. Disabling keeps the
binding and mappings, and JSONL restore leaves polling disabled until a local
operator re-enables it.

## Two-way status

```sh
kata sync plane enable --status-sync=two-way
kata close abc4 --done --message "Completed the imported task" --commit <sha>
kata reopen abc4
kata sync plane once
kata sync plane status
```

Closing an issue picks the first state in the `completed` group ordered by
workflow `sequence`, and reopening picks the first `unstarted` state, with UUID
order breaking ties. To pin a specific target in either group, pass
`--closed-state UUID` or `--open-state UUID`. Enabling two-way status requires
both groups; a one-way project can omit them, even when it holds a saved
override that has disappeared or moved. When Plane already matches the open or
closed state, Kata leaves the current Plane substate intact, including `started`
and `cancelled`. Renaming or reordering workflows needs no mapping rebuild,
while missing or moved overrides block writes until you correct them.

Only explicit close and reopen events accepted while two-way mode is configured
queue outbound writes; initial opt-in sends no bulk changes. Pending local
intent wins until verified readback acknowledges that exact event. In two-way
mode, status polling checks existing mappings independently of `--since` and
content conversion, so a content failure cannot suppress a status transition.
One-way mode takes status from content imports. Incoming cancellation closes
with `wontfix`, and completion closes with `done`. Same-state observations
preserve native closure evidence. Unavailable, archived, deleted, and moved
items stay local.

Status output shows the saved mode, pending count, and latest sanitized run
error, and `once` reports incoming status updates separately from content
imports. Failures retain pending intent for a later scan, and Kata never blindly
retries PATCH responses. Running `disable` pauses delivery while keeping
mappings and intent; switching back to `--status-sync=one-way` cancels pending
writes. Re-enabling preserves saved targets and mode. A mode-only return to
one-way succeeds even if a saved target is missing or outside its original
group, whereas two-way still requires valid live targets. Ordinary JSONL restore
clears private observations and pending intent and leaves the binding disabled
until you re-enable it.

## Progress, bounds, and recovery

Status shows the saved source, origins, interval, cutoff, last attempts,
success, errors, historical successful-run counts, and optional live progress
(`project`, `states`, `work-items`, `content`, `importing`, and `finalizing`). A
zero total means unknown. Manual and scheduled runs share a durable claim,
pacing, and progress tracker, and the daemon returns a conflict if you start a
second run while one is active. Disable and re-enable fence the earlier binding
snapshot while keeping its claim until that worker retires or the 30-minute
stale-claim horizon allows recovery.

Each run has a 20-minute deadline, and each HTTP request has a 30-second
timeout. All bindings share a rate limit of at most one request per second per
daemon, with up to four attempts for HTTP 429 and server errors, exponential
delays, and `Retry-After` cooldowns capped at 20 minutes. Enable validation has
a two-minute deadline. Very large projects or prolonged rate limiting can exceed
a deadline and retry without advancing the cursor. Because bindings run
serially, a slow project delays others.

Preparation reads and validates the complete eligible collection before
importing anything. Hard bounds guard each run: 10,000 items or states, 1,000
responses per collection, 8 MiB per decoded response, 128 MiB of total raw
responses per collection, 1 MiB per HTML description and converted Markdown, a
nesting depth of 256, and 64 MiB of serialized import items. These are failure
guards rather than supported-size promises. Incomplete fields or content,
missing or repeated cursors, duplicate identities, and exceeded bounds fail
preparation instead of importing a partial collection. Work-item validation
errors include the source UUID. Link targets allow `http`, `https`, and
`mailto`, plus relative URLs resolved against the work item; Kata strips other
schemes, malformed URLs, and URLs with user information without stopping the
import. A final `next_page_results=false` ends traversal even if Plane returns a
nonempty final cursor. States can arrive as paginated results or as a complete
array.

After a failure, check the error, service environment, API permissions, source
identity, and response limits, then retry with `once`. Narrowing `--since` can
reduce eligible content, though it does not backfill older work. Failed runs
leave the cursor unchanged. Imports run in durable guarded chunks: if a later
chunk fails, already committed chunks and events remain durable, and retry
replays them safely. Disabling fences future chunks even if an upstream read
continues after `disable` returns.
