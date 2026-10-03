---
title: "Plane project sync"
description: "Understand Plane source identity, pagination, state mapping, and two-way status delivery."
last_edited: 2026-10-01
---
# Plane project sync

**Status: implemented v1.** Plane uses the provider-neutral issue-sync runner
and existing import storage, with no persisted schema change. See the
[operator guide](../operations/plane-sync.md) for setup and limits.

`internal/planesync` owns canonical source config, origin-pinned API sessions,
collection pagination, state-group mapping, and local HTML-to-Markdown conversion.
Both daemon runtimes share one configured client, wake channel, and progress
tracker with manual requests. The shared runner owns durable claims, deadlines,
drain admission, imports, events, and success/error recording.

The source key is `plane:<api-origin>/<workspace>/<project-uuid>`. The remote ID
is `<workspace>/<project-uuid>` and item IDs are `work-item:<uuid>`. Web origins
are presentation settings and do not change source identity. Daemon origins and
token selectors cannot be supplied through binding requests. A saved API origin
must match the daemon configuration before resolving its key. Sessions capture
one key and reject retargeting and redirects.

The client traverses current Plane `projects`, `states`, and `work-items` APIs,
using `per_page=100` and `order_by=sequence_id` for work items. The documented
cursor envelope contains `results`, `next_page_results`, and `next_cursor`;
the boolean is authoritative at the final page. State schemas can also return
complete arrays. Every poll reads the collection, then applies an exclusive
updated-after cutoff locally. Offset pagination is eventual: a later full scan
recovers omissions caused by concurrent removals. Missing descriptions or
expanded identity objects are rejected before any import.

Plane state groups can change without advancing work-item updated timestamps.
The status worker reads existing mappings independently of the content cutoff
and native issue update time. These observations change status and closure only;
title, body, owner, and priority keep their ordinary strictly-newer source rules.
Presentation and label reconciliation retain their independent ownership rules.

Opt-in two-way status uses the shared worker and four nullable import-mapping
columns described in [issue status sync](issue-status-sync.md). Incoming workflow
reads run separately from content, and outbound writes change only `state` after
fresh admission. Exact-event acknowledgement follows verified identity/state
readback; Plane uses its existing item UUID and needs no extra locator.
Live target membership is required only for two-way bindings. Returning to
one-way cancels pending writes even when a saved target has disappeared or moved
to another group; the binding retains its canonical target UUIDs for later edits.
Disable and re-enable fence the earlier binding snapshot but retain its claim
until the worker retires or the 30-minute stale-claim horizon permits recovery.
Source deletions, comments, relationships, and webhook ingestion remain deferred.

API behavior was checked against [Plane's API documentation](https://developers.plane.so/api-reference/introduction)
and [Plane API source](https://github.com/makeplane/plane/tree/preview/apps/api/plane/api).
