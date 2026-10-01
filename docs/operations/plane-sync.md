---
title: Plane sync
description: Mirror a Plane project into native Kata issues with daemon-owned read-only API credentials.
last_edited: 2026-09-30
---

# Plane sync

Plane sync mirrors one Plane project into one Kata project. The daemon reads
Plane and imports native issues every five minutes by default. Local issue edits,
including completion, stay in Kata. Status write-back will be a separate feature.

## Configure the daemon

Create a Plane API key that can read the selected project, its states, and its
work items. Scoped keys need `projects:read`, `projects.states:read`, and
`projects.work_items:read`. Supply the key as `KATA_PLANE_TOKEN` in the daemon's
service environment. The client workstation needs only its normal Kata daemon
credentials. See Plane's [API authentication guide](https://developers.plane.so/api-reference/introduction).

Cloud defaults to `https://api.plane.so` for API requests and
`https://app.plane.so` for attribution links. Self-hosted installations can set
root origins and a different environment variable in `<KATA_HOME>/config.toml`:

```toml
[plane_sync]
api_origin = "https://plane.example"
web_origin = "https://plane.example"
token_env = "EXAMPLE_PLANE_TOKEN"
```

When `web_origin` is omitted it follows the API origin, except the Cloud default
uses `https://app.plane.so`. Origins require HTTPS; literal loopback IPs may use
HTTP. Path-prefixed deployments, credentials, query strings, and fragments are
rejected. API keys go only to the configured API origin in `X-API-Key` headers;
redirects are rejected. Links and images in descriptions are converted locally
without fetching their targets. Keys never enter binding config, exports, or
command output. Restart the daemon after changing its configuration or service
environment. Each run captures its key once; later runs see rotated keys.

For remote-client use, configure Plane on the selected remote daemon. Set
`autostart_idle_timeout = "0"` when polling must continue without clients.
Embedded services configure the same values through `Config.PlaneSync`.

## Enable and operate

From a bound Kata workspace:

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

Initial enable requires the Plane workspace slug and project UUID. UUIDs with or
without hyphens normalize to lowercase hyphenated IDs. Workspace slugs are
case-sensitive URL-safe names. Kata's global `--workspace` selects the local
workspace directory; global `--project` selects the native Kata project.
The daemon validates upstream project access and state groups before saving.
The current `work-items/` API is required; legacy `issues/` endpoints are not used.

| Enable option | Behavior |
| --- | --- |
| `--plane-workspace` | Plane workspace slug; required initially, then immutable. |
| `--plane-project` | Plane project UUID; required initially, then immutable. |
| `--interval` | Duration or integer seconds, at least one second; initially `5m`. |
| `--since` | Exclusive updated-after UTC date or whole-second RFC3339 timestamp. |
| `--title-prefix=false` | Keep source titles and add the source-managed `plane` label. |

Re-enable may omit every option to reuse saved settings. Explicit empty
`--since ''` clears the cutoff; explicit true restores title prefixes. Dates
mean midnight UTC and timestamp offsets normalize to UTC. Fractional seconds
are rejected. Config changes reset the stored cursor; interval-only changes
preserve it. The cursor records successful run starts for diagnostics. Every
run traverses the whole collection and applies the cutoff locally, so workflow
schema changes and items missed during concurrent pagination can arrive later.
This is eventual polling, not an atomic Plane snapshot.

One external issue-sync binding owns each native project. GitHub, Notion, and
Plane bindings cannot replace one another, even after disable. A Plane binding's
API origin, workspace, and project identity cannot change. Changing the daemon's
API origin makes old bindings fail before credentials are resolved; use another
Kata project for another source. Re-enable can refresh its web origin for links.
Federation spokes receive imports through their hub; enable Plane on the hub.
Existing external-root ownership protections apply to managed issues.

## Mapping and local edits

| Plane value | Kata issue |
| --- | --- |
| Name and sequence | `[Plane EX-12] Original title`; blank names become `(untitled)`. |
| Description HTML | Markdown with a Plane attribution link; tables, lists, links, and images are retained when convertible. Unsupported or malformed link targets are removed while link text is kept. |
| Mention tags (`<mention-component>`) | Labels stored in tag attributes are omitted; literal text and surrounding description text are retained. |
| Priority | `urgent` → P0, `high` → P1, `medium` → P2, `low` → P3, `none` → unassigned. |
| `backlog`, `unstarted`, `started` state groups | `open`. |
| `completed` group | `closed`, reason `done`. |
| `cancelled` group | `closed`, reason `wontfix`. |
| First assignee UUID | Owner `plane:<uuid>`; no assignees means unassigned. |
| Creator UUID | Author `plane:<uuid>`, or `plane-unknown` when absent. |
| Source timestamps | Created/updated timestamps normalized to persisted milliseconds; closure uses source updated time. |

External UUID identities are not mapped automatically to local users. State
names, colors, and ordering do not affect completion. Unknown state groups or
references fail preparation. Upstream dates, labels, comments, relationships,
cycles, and modules are outside this import. Priority follows Plane on strictly
newer imports; an explicit `none` clears it.

Source scalar edits apply only when their updated timestamp is strictly newer
than the native issue. Newer local edits survive until Plane has a later source
edit. Equal or older observations preserve title, body, owner, and priority.
A state-group edit may leave the work item's timestamp unchanged: status alone
can reconcile when the observed timestamp, latest mapping timestamp, and native
issue timestamp all match. A newer local edit prevents this correction.

Changing title-prefix presentation refreshes source-owned titles at the same
source version while preserving local title edits. The `plane` label is managed
separately and can refresh after local scalar edits; local labels remain intact.
Archived/deleted/unavailable work items are retained locally. Absence never
closes or deletes an imported issue. Disable retains the binding and mappings.
JSONL restore disables polling until a local operator re-enables it.

## Progress, bounds, and recovery

Status shows the saved source, origins, interval, cutoff, last attempts, success,
errors, historical successful-run counts, and optional live progress (`project`,
`states`, `work-items`, `content`, and `import`). A zero total means unknown.
Manual and scheduled runs share a durable claim, pacing, and progress tracker.
A second run receives a conflict while one is active. Re-enable clears the old
claim and fences the earlier run from importing or advancing its cursor.

Each run has a 20-minute deadline; each HTTP request has a 30-second timeout.
All bindings share at most one request per second per daemon, with up to four
attempts for HTTP 429 and server errors, exponential delays, and `Retry-After`
cooldowns capped at 20 minutes.
Enable validation has a two-minute deadline. Very large projects or prolonged
rate limiting can exceed a deadline and retry without advancing the cursor.
Bindings run serially, so a slow project delays others.

Preparation reads and validates the complete eligible collection before any
import. Hard bounds are 10,000 items or states, 1,000 responses per collection,
8 MiB per decoded response, 128 MiB total raw responses per collection, 1 MiB
per HTML description and converted Markdown, nesting depth 256, and 64 MiB
serialized import items. These are failure guards, not supported-size promises.
Incomplete fields/content, missing/repeated cursors, duplicate identities, and
exceeded bounds fail preparation rather than importing a partial collection.
Work-item validation errors include the source UUID. Link targets allow `http`,
`https`, and `mailto`, plus relative URLs resolved against the work item. Other
schemes, malformed URLs, and URLs with user information are removed without
stopping the import. A final `next_page_results=false` ends traversal even if Plane returns
a nonempty final cursor. States may use paginated results or a complete array.

After failure, check the error, service environment, API permissions, source
identity, and response limits, then retry with `once`. Narrowing `--since` can
reduce eligible content; it does not backfill older work. Failed runs keep the
cursor unchanged. Imports use durable guarded chunks: a later failure retains
already committed chunks and events, and retry replays them safely. Disable
fences future chunks even if an upstream read continues after it returns.
