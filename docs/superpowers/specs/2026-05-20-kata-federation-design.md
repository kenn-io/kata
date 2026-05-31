# kata federation — hub-and-spoke design

**Status:** Historical design spec. The implemented behavior is documented in
`docs/design/federation.md`; the per-phase implementation plans were consolidated and
removed after completion.
**Date:** 2026-05-20
**Topic:** Multi-master federation of shared kata projects across independent
daemons in a hub-and-spoke topology: low-latency upstream propagation,
periodic downstream pull, conflict-free convergence, a precise replayable
audit log, and hub-arbitrated claims so multiple agents never double-work an
issue.

Implementation note: the pre-merge implementation keeps the internal storage
and audit-event term `claim`, but the public CLI/API/docs call the concept a
federation **lease** to avoid colliding with user-facing "claim/take this task"
assignment workflows.

---

## 1. Context

kata today is local-first: one daemon, one SQLite database under `KATA_HOME`,
all reads and writes through an HTTP API, an append-only event log, and a TUI
over the same data. Federation between instances was **deliberately deferred**
before v1, but the schema was seeded for it:
every event carries a stable `uid` and an `origin_instance_uid`, event payloads
cross-reference other entities by UID (not local integer id), soft-delete +
purge tombstones exist, and `revision` columns exist as cache-coherency hints.

A separate note (`docs/superpowers/specs/2026-04-29-kata-shared-server-mode.md`)
describes a **centralized** shared server — thin clients talking to one
authoritative daemon. This spec describes something different and complementary:
**each user keeps their own daemon holding a replica of shared projects**, with
bidirectional sync to a hub. Where the two overlap (auth, project-vs-path
identity, server-derived actor), this design follows the shared-server note's
guardrails.

Users who require strong consistency for every read and write should talk
directly to a shared daemon instead of using federation. Federation deliberately
chooses weaker consistency for general edits — local writes, async sync, and
deterministic convergence — in exchange for local-first behavior, offline
tolerance, and a simpler hub-and-spoke architecture. Operations that need
exclusivity, such as claims, still use synchronous hub arbitration.

The prior federation groundwork that was removed (per-instance `origin_seq`
column + unique index + conflict-ordering rules) lives in commits `ee8b697`,
`30dabf1`, `7877121`. This design intentionally **does not** revive `origin_seq`
as the conflict clock (see §6.3); it is available later only as an optional
per-origin continuity check.

### 1.1 What today's code already gives us

| Capability | Where | Federation use |
|---|---|---|
| Event `uid` + `origin_instance_uid` on every row | `internal/db/schema.sql:200` | Global event identity + provenance |
| UID-keyed cross-references in payloads | event payloads | Stable across replays/merges |
| Monotonic event cursor (`after_id`) + `reset_required` | `internal/daemon/handlers_events.go` | The pull transport, unchanged |
| SSE broadcaster fan-out | `internal/daemon/broadcaster.go` | Push trigger on local commit |
| JSONL `event` record type + deterministic export | `internal/jsonl/` | The delta wire format |
| Bearer-token transport pinned to origin | `internal/client/auth.go` | Spoke→hub auth |
| Non-public address validation | `internal/daemon/endpoint.go` | Private-network boundary |
| Per-key metadata diffs (`{from,to}`) | `internal/db/store_metadata.go:113` | Path-level metadata merge |

### 1.2 The gaps federation must close

1. **Events are not replay-complete.** `issue.updated` is written with
   `Payload: "{}"` (`internal/db/queries_edit_atomic.go:183`); `issue.created`
   carries labels/links/owner/priority but **not `title` or `body`**
   (`buildCreatedPayload`, `internal/db/queries.go:582`). You cannot reconstruct
   an issue's state from its events today.
2. **No global identity for comments.** The `comments` table has no `uid`;
   labels and links rely on composite identity.
3. **No cross-node ordering primitive.** Ordering is local autoincrement `id`
   plus wall-clock-millisecond `created_at`. "Latest wins" across hosts needs a
   skew-tolerant logical clock.
4. **The daemon has never been an HTTP client of another daemon.** The client
   library and bearer transport exist, but no outbound sync loop does.

---

## 2. Goals and non-goals

### Goals
- A user's daemon holds a local replica of shared projects and serves all
  reads/writes locally (local-first preserved).
- Local changes to shared projects propagate upstream to the hub with
  sub-second latency when online, and queue durably when offline.
- The spoke periodically pulls hub changes and applies them.
- Conflicting concurrent edits **always converge** to the same state on every
  node, with **no irreconcilable conflict** by construction.
- The full event history is retained as a precise, replayable audit log.
- Agents can acquire an exclusive, hub-authoritative **claim** on a shared issue
  so two agents never silently work the same issue.

### Non-goals (v1 of federation)
- Full authenticated multi-tenant deployment (coarse per-spoke bearer tokens +
  private network for now; real auth tables are additive later).
- A global user registry (free-form actor strings remain, disambiguated by
  `origin_instance_uid`).
- Federated authoring of recurrences (see §11), cross-hub issue moves (§12), and
  arbitrary hard purge of federated history (§10).
- Strong consistency for all reads/writes across replicas. Users that need that
  should use the centralized shared-server mode and talk directly to one daemon.
- Changing the local single-user write path for non-shared projects.

---

## 3. Key decisions (locked)

| # | Decision | Rationale |
|---|---|---|
| D1 | **Availability:** general edits are async/offline-tolerant (LWW); acquiring a claim requires a synchronous hub round-trip. | Preserves local-first editing + low-latency sync while making claims genuinely exclusive when the hub is reachable. Offline claim requests are *pending*, never authoritative. |
| D2 | **State model:** for federated projects, the **event log is the source of truth**; issue/comment/label/link/metadata tables are a deterministic **fold** (projection). Non-federated projects keep today's direct-write path. | Solves audit/replay and conflict-resolution with one mechanism; reuses the existing cursor + JSONL event record; guarantees convergence. |
| D3 | **Write path for federated projects:** mutation = *append complete event, then apply the projection*, in one transaction. Never "mutate row, best-effort event after." | Prevents drift between the local write path and the hub/spoke replay path. |
| D4 | **Merge order:** `(hlc, origin_instance_uid, event_uid)`. Local `events.id` is a delivery cursor only, never merge order. | Skew-tolerant total order; deterministic tiebreak; convergence. |
| D5 | **Conflict resolution:** per-field LWW (scalars), per-element LWW with tombstones (labels/links), per-path LWW with structural-absence deletion (metadata), append-only by `comment_uid` (comments). | Field/element granularity avoids clobbering unrelated concurrent edits; tombstones prevent resurrection. |
| D6 | **Claims** are a separate concept from `owner`: hub-authoritative, exclusive, and stored in a distinct claim table. The default claim is hard (explicit release); optional timed claims add nullable `expires_at` + renew. `owner` stays a durable LWW field. | "Responsible for" (owner) and "actively working right now" (claim) are different questions. A time limit is an agent policy, not the base coordination model. |
| D7 | **Purge:** federated projects are soft-delete-only; hard purge is a hub-admin reset boundary. | Deleting replay history fights event-truth; reset/bootstrap is the safe, explicit escape hatch. |
| D8 | **Auth:** per-spoke bearer token over a private network; the hub binds each token to an expected `origin_instance_uid` and an explicit enrollment grant. In a single trusted deployment that grant may cover all federated projects on the hub; in mixed/untrusted deployments it must narrow to project scopes/capabilities. Real user/member auth tables are additive later. | Cheap to start while preventing origin spoofing. Scope granularity follows the trust model instead of forcing per-project admin overhead on trusted private networks. |
| D9 | **Actor identity:** free-form actor strings preserved as snapshots; nodes disambiguated by `origin_instance_uid`. No global user registry in v1. | Matches the shared-server note; keeps historical audit readable. |

---

## 4. Architecture

### 4.1 Topology and roles

The **hub** is not a special binary — it is an ordinary kata daemon designated
as the rendezvous and authority for a set of shared projects. **Spokes** are
kata daemons that hold a local replica of those projects and sync with the hub.
The hub may itself originate events (someone can work directly on it); "hub" is a
role and topology, not a different program.

The federation auth boundary is **spoke→hub**, not individual agent→hub. In the
standard deployment, ephemeral agents, CLIs, and TUIs are thin clients of one
long-lived spoke daemon using the existing local/remote-client trust model. The
spoke is enrolled with the hub once and becomes the federation principal. If a
future deployment wants each ephemeral agent to run its own spoke with a distinct
`instance_uid`, that is a separate bootstrap-enrollment feature, not the default
v1 path.

```
   spoke A (daemon + SQLite)              spoke B (daemon + SQLite)
        |   ^                                  |   ^
  push  |   | pull (poll / SSE tail)     push  |   | pull
 (on-commit)|                          (on-commit)|
        v   |                                  v   |
   +------------------------------ HUB daemon ------------------------------+
   |  POST .../events:ingest  (dedup by uid+hash; origin-token bound)       |
   |  GET  .../events?after_id=N  (existing stream contract)                |
   |  POST .../issues/{ref}/actions/claim|renew|release  (atomic claim)     |
   |  purge / reset authority for federated projects                       |
   +-----------------------------------------------------------------------+
```

### 4.2 Data flow

- **Pull (hub→spoke):** the spoke is an event-stream client of the hub. It uses
  the *existing* `after_id` cursor contract. Apply = insert new events (dedup by
  `uid` + content hash) → re-fold affected issues. No new hub machinery for the
  steady state.
- **Push (spoke→hub):** the spoke subscribes to its own broadcaster and forwards
  *locally-originated* shared-project events to the hub immediately (sub-second
  when online), through a transactional outbox/recovery scanner that drains on
  reconnect. The broadcaster is only a wake-up signal, never the durability
  boundary.
- **Bootstrap:** paged poll from the project's federation **replay horizon**
  (§7.2), folding as it goes, then switch to live tail.

### 4.3 Watermarks / cursors (answers the original "how do nodes know what to send")

Two independent cursors, both cheap; the hub keeps no per-puller state for the
pull path (same as SSE today):

| Cursor | Lives on | Meaning |
|---|---|---|
| **Pull cursor** (per shared project) | spoke | Highest hub `events.id` consumed. Hub purge below it → `reset_required` → re-bootstrap. |
| **Push cursor** (per shared project) | spoke | Highest local `events.id` of the spoke's *own* events the hub has acked. Hub ingest is idempotent (dedup by `uid` + content hash), so retries are safe. |

Loop avoidance is structural: a spoke only ever pushes events whose
`origin_instance_uid` equals its own instance UID; foreign events arrived *from*
the hub, so the hub already has them. `origin_seq` is **not** reintroduced as the
merge clock; if gap-detection is ever needed it can return as an optional
per-origin continuity counter, orthogonal to the HLC merge order.

---

## 5. Component boundaries

Each unit has one purpose, a defined interface, and is testable in isolation:

- **`fold` engine** (`internal/fold`, new): pure function
  `Fold(events []Event) -> Projection`. No I/O. The heart of replay and conflict
  resolution. Property-tested for order-independence.
- **HLC clock** (`internal/hlc`, new): stamps local events; advances on apply of
  foreign events. No I/O beyond a monotonic source.
- **Event emit-and-apply** (`internal/db`, extended): for federated projects,
  one transaction appends a complete event and applies its projection delta.
- **Federation sync** (`internal/federation`, new): the outbound pull loop, the
  push queue, bootstrap, reset handling. The only component that is an HTTP
  *client* of another daemon. Reuses `internal/client`.
- **Hub ingest + claim handlers** (`internal/daemon`, extended): new endpoints;
  reuse the existing per-request-transaction + broadcaster pattern.
- **Claim store** (`internal/db`, extended): the authoritative claim table and
  arbitration queries.

---

## 6. The event model and replay (Phase 0 foundation)

This section is independently valuable: it gives kata a precise, replayable audit
log and powers conflict resolution. It ships **before** any federation code.

### 6.1 Event completeness requirements

Every mutation event must carry enough to reconstruct the resulting state of what
it touched. Concretely:

- **`issue.created`** must carry: `uid`, `short_id`, `title`, `body`, `author`,
  `owner`, `priority`, `status`, `closed_reason`/`closed_at` (if created closed),
  `metadata`, initial `labels`, initial `links`, `created_at`, plus the existing
  `idempotency_key`/`idempotency_fingerprint`. (Today it omits `title`/`body`.)
- **`issue.updated`** must carry the changed scalar fields with their new values
  (and old values for non-`body` scalars, for human-readable audit). `body`
  stores the new value; an old-value/diff optimization for large bodies is noted
  but deferred. (Today the payload is `{}`.)
- **`issue.closed`/`reopened`/`assigned`/`unassigned`/`priority_*`** keep/extend
  current payloads so the resulting field value is unambiguous.
- **`issue.commented`** carries `comment_uid`, `author`, `body`, `created_at`.
- **`issue.labeled`/`unlabeled`** carry `issue_uid`, `label` (already present).
- **`issue.linked`/`unlinked`/`links_changed`** carry endpoint UIDs + type
  (already present).
- **`issue.metadata_updated`** keeps the per-key `{from,to}` diff (already
  present, and sufficient for path-level merge — §6.6).
- **`issue.soft_deleted`/`restored`** mark deletedness (LWW register).
- **`issue.moved`** carries old/new project UID (scope-limited — §12).

### 6.2 Event taxonomy and fold rules

| Event type | Folds into | Rule |
|---|---|---|
| `issue.created` | issue row (initial) | Establishes the entity; immutable fields (`uid`, `short_id`, `author`, `created_at`) set once. |
| `issue.updated` | title/body/owner | per-field LWW |
| `issue.assigned` / `issue.unassigned` | owner | per-field LWW (owner register) |
| `issue.priority_set` / `issue.priority_cleared` | priority | per-field LWW |
| `issue.closed` / `issue.reopened` | status/closed_reason/closed_at | per-field LWW (status register) |
| `issue.soft_deleted` / `issue.restored` | deleted_at | LWW register (last of the two wins) |
| `issue.commented` | comments | append-only set keyed by `comment_uid`; order by `(hlc, uid)` |
| `issue.labeled` / `issue.unlabeled` | issue_labels | per-element LWW with tombstone, key `(issue_uid, label)` |
| `issue.linked` / `issue.unlinked` / `issue.links_changed` | links | per-element LWW with tombstone; key per the link-identity note below |
| `issue.metadata_updated` | metadata | per-path LWW; delete = structural absence (§6.6) |
| `project.metadata_updated` | project metadata | per-path LWW; delete = structural absence (§6.6) |
| `issue.moved` | project_id | constrained; see §12 |
| `recurrence.*` | recurrence template/state | replicated read-only on spokes; materialization hub-only (§11) |
| `claim.acquired/released/expired/force_released` | (audit only) | claim *state* is authoritative from the hub API, not folded (§9.6) |
| `issue.snapshot` | issue row (baseline) | establishes folded state at the federation horizon (§7.2) |
| `close.throttled` | (audit only) | not a state change |
| `sync.reset_required` | (control) | transport signal, not stored |

**Link element identity (portability).** The fold key for `parent` and `blocks`
is directional: `(from_uid, to_uid, type)`. For symmetric `related` links the key
canonicalizes the endpoint UIDs lexicographically —
`(min(uidₐ, uid_b), max(uidₐ, uid_b), "related")` — because today's schema
canonicalizes `related` by *local integer id*
(`internal/db/schema.sql:101`: `CHECK (type <> 'related' OR from_issue_id <
to_issue_id)`), which is not portable across replicas with different local ids.
Materialization may reorder the local FK endpoints to satisfy that same-DB
constraint independently of the UID canonical order.

### 6.3 HLC and merge order

- Add `events.hlc` — a hybrid logical clock value: `(physical_ms, counter)`,
  stored sortable (two integer columns or one fixed-width encoded string).
- Add an immutable `events.content_hash` over the canonical event identity and
  contents: `uid`, `origin_instance_uid`, project identity, `type`, `actor`,
  `hlc`, `created_at`, and canonicalized payload. Local delivery fields such as
  autoincrement `events.id` and local projection state are excluded. A duplicate
  `uid` with the same hash is a retry/no-op; a duplicate `uid` with a different
  hash is an integrity error and must be rejected or quarantined before fold.
- On **local emit**: `hlc = max(last_hlc, now_ms·2^k) + 1` (standard HLC update),
  stamped in the same transaction as the event.
- On **apply of a foreign event**: advance the local HLC past the incoming
  event's HLC so causally-later local events sort after it.
- **Merge order is `(hlc, origin_instance_uid, event_uid)`** — total, deterministic,
  skew-tolerant. Wall-clock `created_at` remains for display/digest only.
- Optional guard: reject/flag events whose `physical_ms` is implausibly far in the
  future (clock-skew defense); the audit log retains everything for human repair.

### 6.4 The fold function and projection

`Fold(events) -> Projection` is pure and deterministic:

1. Sort events by `(hlc, origin_instance_uid, event_uid)`.
2. For each issue, fold its events into the projection applying the §6.2 rules.
3. Output rows for issues/comments/labels/links/metadata identical in shape to
   the current tables.

**Convergence guarantee:** the projection is a pure function of the event *set*.
Any two nodes holding the same events compute identical state, regardless of
arrival order — so there is no irreconcilable conflict by construction. This is
property-tested: shuffling event arrival order yields a byte-identical projection.

Materialization is incremental in the hot path: applying one event re-folds only
the affected issue (O(events-per-issue), negligible for kata-sized issues), not
the whole project.

### 6.5 Comment identity

- Add `comments.uid` (ULID). `issue.commented` references `comment_uid`.
- Existing comments are backfilled with deterministic UIDs (the same
  `FromStableSeed` approach used to backfill project/issue UIDs in the
  pre-v2→v2 JSONL cutover), so backfill is stable across nodes and re-runs.

### 6.6 Metadata: JSON-path-level merge

kata metadata is a per-key map. Reserved keys are registry-validated typed values
(date, bool, string, checklist array, IANA tz — `internal/metadata/registry.go`);
**unreserved keys are accepted opaquely and stored verbatim**
(`internal/metadata/validate.go:24`), so an opaque key's value may be an arbitrary
nested JSON object — including legitimate nested `null`s. The mutation API patches
**whole top-level keys** (set or clear); to change a sub-field of an opaque object
a client read-modify-writes the whole value. Path-level merge exists to salvage
*concurrent* read-modify-writes to different sub-fields of the same key.

Merge rules:

- The fold recursively **diffs** each `*.metadata_updated` event's per-key
  `{from, to}` to recover the JSON-Pointer paths that event changed. Scalar and
  array leaves are LWW registers keyed by `(entity_uid, json_pointer)`, resolved
  by `(hlc, origin, uid)`.
- **Deletion is keyed off structural absence, never off JSON `null`.** A value
  present in `from` but absent in `to` is a tombstone for that path. If that path
  is an object, the tombstone covers the whole subtree: a later clear of `/foo`
  hides older descendant writes such as `/foo/bar`, while a newer descendant write
  can recreate the subtree. A leaf present in `to` with value `null` is a *real*
  null data value and is preserved. JSON distinguishes `{"a":null}` from `{}`, so
  this is unambiguous.
- The only place `null` acts as a marker is the **top-level cleared-key
  convention**: the mutation API and `metadata.Diff` both treat a top-level `null`
  as "clear this key" (`validate.go:31`, `store_metadata.go:312`, `diff.go:36`),
  and a top-level key can never be *stored* as `null`. A cleared key therefore
  surfaces as the key being structurally absent in `to` — consistent with the
  nested rule above.
- **Leaf = scalar or array; arrays are atomic** (no element-level array CRDT in
  v1, so a checklist is replaced wholesale). Objects are interior nodes that
  recurse. For reserved keys (flat scalars + atomic checklist) the whole scheme
  degenerates to per-key LWW.
- Result: concurrent `foo/bar` and `foo/baz` writes both survive; concurrent
  writes to the *same* pointer resolve by clock.
- Type-conflict and deletion edge cases are prefix-aware: one event writing an
  object/scalar at `/foo`, clearing `/foo`, or writing a descendant like
  `/foo/bar` resolves by LWW at the nearest common pointer. Higher clock wins
  that subtree. Property-tested.

This lives entirely in the fold engine; the existing event payload format is
unchanged (it already records whole-value `{from,to}` per key).

### 6.7 The drift-guard invariant test

The full guarantee is **project-level**: for every non-federated project (which
still uses the direct-write path), assert
`direct_write_project_projection == Fold(project_events)`. Project scope is
required because links and project metadata are project-scoped — an event
attached to one issue (e.g. a link, or a cross-issue change) can affect another
issue's projection, so `events_for_one_issue` is too narrow. For test speed, back
the project-level identity with focused affected-entity assertions (re-fold and
compare only the entities a given mutation touched). This mechanically proves
event completeness and prevents the direct-write and replay paths from drifting —
the gate that lets us trust event-truth before federation turns on.

---

## 7. Federation transport

### 7.1 Pull — read-only replication (Phase 1)

The spoke runs an outbound sync loop per shared project:

1. `GET hub/api/v1/projects/{id}/events?after_id={pull_cursor}&limit=N`,
   following the existing batch + `reset_required` contract.
2. Insert new events (dedup by `uid` + `content_hash`; preserve `uid`,
   `origin_instance_uid`, `hlc`, `content_hash`; reassign only the local
   `events.id`). If a duplicate `uid` arrives with a different `content_hash`,
   reject/quarantine it as an integrity error and do not fold it.
3. Re-fold affected issues; update the projection.
4. Advance `pull_cursor`; repeat until caught up, then switch to SSE tail for
   low-latency live updates (poll remains the catch-up/fallback path).

At the end of Phase 1 a spoke can mirror a shared project read-only; no local
writes flow upstream yet. This isolates and proves the riskiest mechanics
(apply, fold, cursor, reset) before write-back exists.

### 7.2 Bootstrap, baseline snapshots, and the replay horizon

Pre-Phase-0 history is **not** replay-complete, so "fold from cursor 0" only
works for history created after Phase 0. To onboard existing projects:

- Enabling federation on a project emits a control event
  `project.federation_enabled`. It runs as a **single write-exclusive
  transaction** over one consistent DB view (the daemon is single-writer, so no
  mutation can interleave between snapshots) and records a **baseline epoch** — an
  HLC boundary `B`. Its `events.id` is the project's **replay horizon**.
- At the horizon, the hub emits a **baseline** from that one consistent view: one
  `issue.snapshot` per **non-purged issue, including soft-deleted rows** (carrying
  `deleted_at`, so a later `issue.restored` for an issue soft-deleted *before*
  federation has a base state to restore). Each snapshot captures complete current
  state — all scalar fields, labels, links, metadata, and the ordered comment
  history (each with its backfilled `comment_uid`). (For very large comment
  threads, comments may instead be re-emitted as `issue.commented` events after
  the snapshot; both representations fold identically.)
- All baseline artifacts — `issue.snapshot` events and any optionally re-emitted
  `issue.commented` events — sort at or below `B`, and the node's HLC is advanced
  past `B`, so **no post-baseline event can sort before the baseline boundary** — a
  fresh replica always folds a single consistent-time baseline, never a mix of
  pre- and post-snapshot states.
- Replicas (and the hub's own folded view of the project) **fold from the
  baseline forward**. Pre-horizon legacy events remain in the log as
  **audit-only** and are never used for fold.
- A fresh spoke bootstraps by paged-polling from the horizon, folding, then
  tailing. No separate snapshot-import path is required (it sidesteps the
  wipe-only limitation of `kata import`), though a snapshot fast-path for very
  large projects can be added later.

### 7.3 Push — bidirectional sync (Phase 2)

- New hub endpoint `POST /api/v1/projects/{id}/events:ingest` accepts a JSONL
  batch of foreign events, dedups by `uid` + `content_hash`, applies/folds, and
  re-broadcasts so other spokes receive them through the normal stream.
  Duplicate `uid` with different `content_hash` is rejected/quarantined before
  insert/fold.
- The push path uses a **transactional outbox or equivalent recovery scan**. The
  durable queue entry is written in the same DB transaction as the local event
  append, or the push loop derives pending work by scanning
  `events.origin_instance_uid == self AND events.id > push_cursor`. The
  broadcaster is only a low-latency wake-up after commit; correctness must survive
  a daemon crash between commit and broadcast handling. On daemon start and
  reconnect, the push loop scans from `push_cursor` before relying on live
  broadcaster wake-ups. Debounce/batch to coalesce bursts.

### 7.4 Loop avoidance, origin-token binding, and enrollment grants

- A spoke pushes **only** events with `origin_instance_uid == self`. Foreign
  events came from the hub, so they are never echoed back.
- The hub binds each bearer token to one expected `origin_instance_uid`
  (configured when the spoke is enrolled) and **rejects** an ingest batch
  containing events whose `origin_instance_uid` differs from the token's bound
  origin. This prevents a compromised/buggy spoke from forging another node's
  provenance, which would corrupt loop-avoidance and audit trust even on a
  private network.
- The hub also binds each token to an explicit enrollment grant. In the common
  trusted-network deployment, a grant may be coarse (for example, all federated
  projects on this hub). In mixed or untrusted deployments, the grant must narrow
  to specific projects and capabilities (`pull`, `push`, `claim`). Every
  project-scoped federation endpoint first checks that the authenticated token is
  authorized for the requested `{id}` before reading, ingesting, folding, or
  arbitrating anything for that project.
- Ingest also performs **content-level project validation** before insert/fold.
  Every event in a batch must belong to the requested project and authorized
  grant: project UID/id, issue UID, comment target, link endpoints, move
  source/target, metadata target, recurrence target, and any other UID reference
  in the payload must resolve inside the authorized project set. For a
  cross-project move allowed by §12, the token must be authorized for both source
  and target projects. Unknown or out-of-scope references are rejected before the
  event enters the log.

### 7.5 reset_required and re-bootstrap

If the hub purges history below a spoke's pull cursor (admin reset — §10), the
existing `sync.reset_required` frame tells the spoke to discard cached state for
that project and re-bootstrap from the current horizon. This reuses the
`purge_log.purge_reset_after_event_id` mechanism already in the schema.

---

## 8. Conflict resolution and convergence

Restating the guarantee precisely, since it is the property the user most wants:

- All mutable state is a CRDT: LWW-register map (scalars + metadata leaves),
  per-element LWW sets with tombstones (labels, links), and a grow-only log
  (comments).
- With a deterministic total order `(hlc, origin, uid)` over a *retained* event
  set, `Fold` is associative/commutative-after-sort. Therefore **every node that
  has seen the same events converges to identical state**, and a late-arriving
  event with a lower clock correctly *loses* to an already-applied higher-clock
  write for the same field/element — without being discarded (it stays in the
  audit log).
- There is no "irreconcilable conflict" state to resolve manually. Surprising
  outcomes (e.g., clock skew making an unexpected writer win) are visible and
  repairable through the retained log, never data loss.

---

## 9. Claims

### 9.1 Model

A claim answers "who is *actively working* this issue right now," distinct from
`owner` ("who is responsible"). It is hub-authoritative and exclusive per issue.
A claim is a coordination primitive, not a cryptographic or consensus proof of
global state.
Holding a claim gives temporary exclusivity against other non-comment work while
the claim is live. It does not replace durable ownership, does not serialize
comments, and is not required for ordinary unclaimed edits, which still converge
through the LWW fold.

The default is a **hard claim**: it has no expiry and stays active until the
holder releases it, the issue closes, or an admin/human force-releases it. For
autonomous agents, kata can also support a **timed claim**: "let me try working
on this for 30 minutes," with `expires_at` and renewal.

Claim row fields: `claim_uid`, `issue_uid`, `project_id`, `holder` (actor),
`holder_instance_uid`, `purpose`/`client_kind`, `claim_kind` (`hard` or
`timed`), `acquired_at`, nullable `expires_at`, `revision`.

Claim ownership is the tuple `(holder_instance_uid, holder)`. The
`holder_instance_uid` is server-authenticated: for spoke requests it comes from
the token-bound `origin_instance_uid`, and for hub-local requests it is the hub's
own `instance_uid`. The free-form actor string is an audit snapshot and is never
enough by itself to authorize claim mutation.

### 9.2 Arbitration API (hub, synchronous)

- `POST /api/v1/projects/{id}/issues/{ref}/actions/claim` — atomically grants if
  no live claim exists, else returns the current holder (denied). Single-writer
  SQLite transaction on the hub is the serialization point. The granted
  `holder_instance_uid` is derived from the authenticated origin, not from a
  caller-supplied payload field.
- `.../actions/renew` — extends `expires_at` for a timed claim if the caller is
  the same `(holder_instance_uid, holder)` tuple. Hard claims do not renew.
- `.../actions/release` — releases if the caller is the same
  `(holder_instance_uid, holder)` tuple.
- Admin override: `force_release` (and optional `steal`) emit audit events.
- Closing an issue releases any live claim as part of the same hub-authoritative
  state transition.

### 9.3 Timed claims, renewal, expiry

- Hard claims have `expires_at = NULL` and are not swept.
- Timed-claim holders renew at ~`ttl/3`.
- The hub expires stale timed claims via a small periodic sweeper **and**
  opportunistically during claim/poll requests (so expiry is observed even if the
  sweeper interval lags).
- Durable events: `claim.acquired`, `claim.released`, `claim.expired`,
  `claim.force_released`. Bare renewals just bump the claim row's `expires_at`
  (not event-sourced — see §9.6) to avoid log spam.

### 9.4 Enforcement (three layers, no data loss)

Because edits are async (D1), the hub cannot reject a mutation that already
happened offline. Enforcement therefore does not live in a synchronous write
path; it is layered:

1. **Claim exclusivity is mechanical** — the hub guarantees ≤1 live holder.
2. **Spoke live-claim gate** — before non-comment work mutations
   (edit/close/priority/links) on a shared issue, a spoke refreshes cached claim
   state when it can. If no live claim exists, the mutation proceeds locally and
   converges by LWW. If the acting agent holds the live claim, the mutation
   proceeds. If another holder has a live, unexpired claim, the spoke refuses the
   mutation. Timed claims stop blocking once expired. Pending offline claim
   requests are never authoritative and do not block ordinary edits. Comments
   bypass this gate because they are append-only.
3. **Hub-side audit annotation** — if a work event arrives while another holder
   has a live claim on the affected issue, the hub emits `claim.violated` and
   the origin spoke surfaces it on next sync. Ordinary unclaimed work is not a
   violation. Nothing is dropped.

A strictly synchronous hub-reject model is possible only by making agent work
mutations synchronous, which trades away offline editing; this design declines
that trade.

### 9.5 Offline pending-claim UX

Offline, a spoke records and displays a **pending** claim request. It must not
block edits or other actors, must not appear authoritative, and is retried on
reconnect. If the hub later denies it, the local daemon surfaces a *rejected
pending claim* and the agent should stop relying on exclusivity.

### 9.6 Claim state authority

Current claim state — especially `expires_at` after timed-claim renewals — is
authoritative from the **hub claim table/API**, not derived from the event log
(renewals are not events). Spokes may cache claim state for display and local
continuity, but must treat it as possibly stale and confirm against the hub before
relying on exclusivity. Offline mutations made while relying on a cached hard
claim are therefore explicitly outside the strict ≤1-holder guarantee; on ingest
the hub annotates work that conflicts with another currently live claim with
`claim.violated` instead of dropping data.

---

## 10. Purge and deletion policy (federated projects)

- Federated projects are **soft-delete only** in normal operation
  (`issue.soft_deleted` / `issue.restored`, folded as an LWW register).
- **Hard purge** of a federated issue is a **hub-admin-only** operation. It
  establishes a reset boundary: the hub records the purge in `purge_log` with a
  `purge_reset_after_event_id`, and spokes with a pull cursor below it receive
  `sync.reset_required` and re-bootstrap from the current horizon.
- Rationale: deleting replay history is fundamentally in tension with
  event-truth; the only safe story is an explicit, operator-acknowledged reset
  that all spokes honor.

---

## 11. Recurrences scope

Recurrence templates and their materialization state replicate as ordinary
folded entities (`recurrence.*` events), but on spokes they are **read-only** for
federated projects: **materialization is performed only by the hub.** Running the
recurrence scheduler on multiple spokes would each materialize duplicate
occurrences (the `(recurrence_id, occurrence_key)` uniqueness is per-database and
UIDs differ across nodes, so cross-node dedup would not catch it).

For the first federation phases, a federated project that uses recurrences is
supported with hub-only materialization; full federated *authoring* of
recurrences from spokes is deferred. The current operator-facing behavior and
limitations are documented in `docs/design/federation.md`.

---

## 12. Issue-move scope

`issue.moved` carries old/new project UID. In v1 federation:

- Moves **between two federated projects on the same hub** are representable and
  fold correctly.
- Moves that **cross a federation boundary** (federated↔local, or across
  different hubs) are **disallowed** initially — there is no coherent single
  event log spanning the boundary. A future design may model this as
  soft-delete-in-source + create-in-target with a linking reference.

---

## 13. Security / auth

- Transport: per-spoke bearer token (reusing `internal/client/auth.go`,
  including origin-pinning) over a private network; the hub's listen address is
  validated non-public exactly as today.
- **Origin-token binding (D8):** the hub maps each token to one
  `origin_instance_uid` and rejects ingest of events claiming a different origin.
- **Enrollment grant binding (D8):** the hub maps each token to an explicit
  project/capability grant and rejects unauthorized project-scoped requests
  before processing request contents. Trusted single-team hubs may use a coarse
  "all federated projects" grant; mixed or untrusted hubs must narrow grants per
  project/capability. Ingest still validates every event payload reference
  against the authorized project set before insert/fold.
- Future (additive, not v1): full `users` / `api_tokens` /
  `project_memberships` tables and project-scoped roles per the shared-server
  note; server-derived actor identity. The event log keeps actor *snapshots* so
  audit stays readable after renames/revocations.

---

## 14. Configuration and identity

- **Project federation binding:** a project is marked federated with an upstream
  hub URL. The hub URL + bearer token live in a gitignored credentials file under
  `KATA_HOME` (e.g. `credentials.toml`, as the shared-server note prescribes),
  **never** in the committed, secret-free `.kata.toml`.
- **Node identity:** the daemon's existing `instance_uid` (`meta` table) is the
  node identity used for `origin_instance_uid`, push loop-avoidance, claim
  `holder_instance_uid`, and token binding.
- **Actor:** unchanged precedence (`--as` > `KATA_AUTHOR` > git user > anonymous),
  stored as a snapshot on every event.

---

## 15. Schema changes (consolidated)

| Change | Table | Phase |
|---|---|---|
| `hlc` value column(s) + immutable `content_hash` | `events` | 0 |
| Complete payloads (esp. `issue.created` title/body, `issue.updated` field diffs) | `events` (payload) | 0 |
| `uid` column + deterministic backfill | `comments` | 0 |
| `federated` flag + upstream binding | `projects` (or a side table) | 1 |
| hub token enrollment: token hash, bound `origin_instance_uid`, project/capability grant | new auth/config table | 1 |
| `pull_cursor` / `push_cursor` per shared project | new sync-state table | 1 / 2 |
| transactional push outbox and/or push-cursor recovery scanner | new table / sync loop | 2 |
| claim table (§9.1) | new | 3 |
| new event types: `issue.snapshot`, `project.federation_enabled/_disabled`, `claim.*`, `claim.violated` | `events.type` | 1–3 |

All additive; non-federated single-user behavior is unchanged except that events
become complete (better audit) in Phase 0.

---

## 16. Original phased delivery outline

The completed phase plans were consolidated into `docs/design/federation.md`. This
section remains as historical context for how the implementation was staged.

### Phase 0 — Replay-complete events + fold engine + HLC (no federation)
- Complete event payloads (§6.1); add `comments.uid` + backfill (§6.5); add
  `events.hlc` and `events.content_hash` (§6.3); build the pure `Fold` engine
  (§6.4) and metadata path-merge (§6.6).
- **Exit:** the §6.7 invariant
  `direct_write_project_projection == Fold(project_events)` holds for every
  mutation type; `Fold` is property-tested order-independent. Audit/digest/replay
  improve immediately. *Ships value with zero federation surface.*

### Phase 1 — Read-only replication (pull)
- Federation config + binding (§14); `project.federation_enabled` + baseline
  snapshots + replay horizon (§7.2); enrollment-grant authorization for event
  pull (§7.4, §13); the pull loop (§7.1); `reset_required` re-bootstrap (§7.5).
- **Exit:** a spoke mirrors a shared project read-only and converges with the hub
  across restarts and resets; no upstream writes yet.

### Phase 2 — Bidirectional sync (push)
- Hub ingest endpoint + origin/enrollment-grant binding and content-level project
  validation (§7.3–7.4); transactional outbox or push-cursor recovery scanner;
  push cursor.
- **Exit:** concurrent edits on multiple spokes converge to identical state on
  all nodes (multi-master LWW), online and after offline reconnect.

### Phase 3 — Claims / optional timed claims
- Claim table + atomic hub endpoints (§9.2); timed-claim sweeper +
  opportunistic expiry; enrollment-grant authorization for claim actions
  (§7.4, §13); spoke live-claim conflict gate; pending-claim UX; `claim.*`
  events; lease CLI/status surface.
- **Exit:** two agents on different spokes cannot both hold a live claim; a
  crashed timed-claim holder's claim auto-expires and frees the issue.

### Phase 4 — Operational hardening
- Federated purge / reset authority (§10); auth hardening toward additive tables
  (§13) if needed; federation status/lag observability (`kata federation status`:
  cursors, queue depth, last sync, lease summary).
- **Exit:** an operator can observe and safely operate a federated deployment.

---

## 17. Open questions / deferred

- HLC physical-time skew guard thresholds and behavior (reject vs flag).
- Baseline comment representation for very large threads (snapshot-embedded vs
  re-emitted).
- Snapshot fast-path for bootstrapping very large projects (vs paged poll).
- Element-level array CRDT for metadata (v1 treats arrays as atomic leaves).
- Cross-boundary issue moves (§12).
- Federated recurrence authoring from spokes (§11).
- Whether `origin_seq` returns as an optional per-origin gap-detection counter.

---

## 18. References

- `docs/design/federation.md` — canonical documentation for the implemented behavior
  and consistency limits.
- `docs/superpowers/specs/2026-04-29-kata-shared-server-mode.md` — centralized
  shared-server guardrails this design follows for auth/identity.
- Prior federation groundwork (removed): commits `ee8b697`, `30dabf1`, `7877121`.
- Ground-truth anchors: `internal/db/schema.sql:200` (events),
  `internal/db/queries_edit_atomic.go:183` (`issue.updated` empty payload),
  `internal/db/queries.go:582` (`issue.created` payload), `store_metadata.go:113`
  (metadata diffs), `internal/daemon/handlers_events.go` (cursor/SSE),
  `internal/client/auth.go` (bearer pinning),
  `internal/daemon/endpoint.go` (non-public address validation).
