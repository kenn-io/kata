# Plane project sync

**Status: implemented v1.** Plane uses the provider-neutral issue-sync runner
and existing import storage, with no persisted schema change. See the
[operator guide](../operations/plane-sync.md) for setup and limits.

`internal/planesync` owns canonical source config, GET-only API sessions,
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
The import policy therefore offers opt-in status-only reconciliation when all
three persisted-millisecond timestamps match: source observation, import mapping,
and native issue. This correction changes status/closure only and preserves other
scalars. Providers default to ordinary strictly-newer source rules. Existing
presentation and label reconciliation retain their independent ownership rules.

Source deletions, comments, relationships, webhook ingestion, and upstream status
writes are deferred. This adapter only reads Plane; write-back belongs to the
later two-way status-sync feature.

API behavior was checked against [Plane's API documentation](https://developers.plane.so/api-reference/introduction)
and [Plane API source](https://github.com/makeplane/plane/tree/preview/apps/api/plane/api).
