# Adopt kit's vector layer for kata semantic search

Date: 2026-07-07
Status: draft, awaiting review

## Goal

Replace kata's in-house vector embedding management (single-vector-per-issue
BLOB storage, brute-force cosine search, hand-rolled fingerprint and
reconcile loop) with `go.kenn.io/kit/vector` + `go.kenn.io/kit/vector/sqlitevec`,
the same layer agentsview builds on. This maximizes shared code across kit
consumers and picks up three capability upgrades kata lacks today:

1. **Chunked embeddings** (`kitvec.Split`) instead of truncating issue text at
   8000 runes — long issues get full semantic coverage.
2. **Generation lifecycle** — a model/config change fills a new generation
   while the old one keeps serving searches, then cuts over. Today a
   fingerprint change drops semantic coverage to zero until re-embedding
   completes.
3. **sqlite-vec KNN** (per-generation `vec0` virtual tables, cosine metric)
   instead of brute-force dot product over an in-memory vector cache.

Decisions made during brainstorming:

- Full adoption (storage included), not just kit's flow types.
- Vectors become **rebuildable derived state**: migration re-embeds
  everything; no carry-over of existing `issue_embeddings` rows.
- JSONL export **drops** the `issue_embedding` kind; import tolerates it in
  old archives (skip + log, never error).
- Sidecar database (`vectors.db`) alongside `kata.db`, structurally parallel
  to agentsview's `vectors.db` mirror pattern — the canonical schema,
  backups, JSONL contract, and federation are untouched.

## Constraint: do not disrupt agentsview

kit is a shared dependency; agentsview ships on tagged `go.kenn.io/kit
v0.2.1`. Rules for this project:

- kata consumes kit via **tagged releases only** (`v0.2.1` or later). No
  `replace` directives or commit pins in committed `go.mod`.
- This design requires **zero kit API changes**. Where kit has gaps (noted
  below), kata works around them locally the same way agentsview does.
- Any future kit improvements motivated by kata (e.g. a generation
  reclamation API) land as **additive** kit PRs, released via tags, adopted
  by each consumer on its own schedule.

## Current state (what is replaced)

| kata today | becomes |
|---|---|
| `internal/embedding.Fingerprint(model, dims, salt)` | `kitvec.Generation{Model, Dimensions, Params}.Fingerprint()` |
| `internal/embedding.EmbedText` truncation (8000 runes) | `kitvec.Split` chunking (recipe keeps title+body join, drops truncation) |
| Reconciler `reconcileOnce`: `ListEmbedTargets` → `Embed` → `UpsertIssueEmbedding` | mirror refresh + `kitvec.Fill` |
| `issue_embeddings` table in `kata.db` | kit-managed tables + `vec0` in `vectors.db` |
| `SearchVector` brute-force + `vectorCache` | `store.QueryGeneration` KNN + `kitvec.RollupByDocument` |
| JSONL `issue_embedding` export/import | dropped (import skips old archives' records) |

**Kept as-is:**

- `internal/embedding/client.go` — the OpenAI-compatible HTTP client with
  SSRF/origin-pinning, batching, L2 normalization, and `APIError`
  (definitive/Retry-After classification). kit deliberately provides no
  provider client (`EncodeFunc` is the caller's job); this is the piece
  agentsview had to write from scratch. It gains a small adapter method
  returning a `kitvec.EncodeFunc`.
- The daemon reconciler's shell: `Wake()` on events, periodic sweep,
  exponential backoff honoring `Retry-After`, `Health()` wire shape.
- `hybridSearch` + `mergeRRF` (RRF fusion of lexical + vector legs). kit's
  `Merge` is for cross-generation unions, not FTS+vector hybrid.
- Config surface: `[search.embeddings]` keys unchanged (`base_url`, `model`,
  `api_key`/`api_key_env`, `fingerprint_salt`, `dims`, `batch_size`,
  `timeout_seconds`, `trust_private_network`).

**Deleted:** `internal/db/sqlitestore/queries_embeddings.go`,
`vector_cache.go`, `export_embeddings.go`; the `issue_embeddings` table
(schema migration drops it); `UpsertIssueEmbedding` / `ListEmbedTargets` /
`EmbeddingStats` / `SearchVector` from the `db.Storage` interface;
`internal/embedding/recipe.go`'s `Fingerprint` (recipe text-rendering
stays).

## Architecture

New package `internal/vector` (named to parallel agentsview's) owning:

- **Sidecar db management** — `<KATA_HOME>/vectors.db`, opened with
  `modernc.org/sqlite` + `sqlitevec.Register()`. Contains kata's mirror
  table plus kit's bookkeeping (`_generations`, `_chunks`, `_stamps`) and
  per-generation `vec0` virtual tables via
  `sqlitevec.New[string, string](ctx, db, schema)`.
- **Mirror table** — `issue_mirror(issue_uid TEXT PRIMARY KEY, project_uid
  TEXT, content TEXT, content_revision INTEGER)`. `content` is the rendered
  recipe (title + "\n\n" + body, untruncated; comments still excluded —
  RecipeVersion 2). A `vector_meta` version marker guards the mirror schema:
  version mismatch → drop and rebuild everything in the sidecar (safe,
  all derived).
- **Generation lifecycle** — desired generation derives from config:
  `kitvec.Generation{Model, Dimensions: dims, Params: {"recipe": "2",
  "salt": salt}}`; its fingerprint is the generation key (`G = string`).
  Doc key is the issue UID (`K = string`, ULID — matches kit's own docs:
  "kata uses UUIDs").
- **Fill orchestration and search hydration** (below).

Dependency changes: `go.kenn.io/kit v0.1.8 → v0.2.1` (or newer tag),
`modernc.org/sqlite v1.49.1 → v1.53.0` (kit's tested version).

kata is pure-Go/no-cgo via modernc; kit's `extension_modernc.go` build path
(`vec_f32(?)` literals, sqlite-vec registered at init) covers this — no
build-tag work needed unless kata ever adds a cgo build.

## Indexing pipeline

`Reconciler.reconcileOnce` becomes:

1. **Mirror refresh** — list live issues (uid, project_uid, title, body,
   content_revision) from the canonical store via one new paginated
   `db.Storage` read method; upsert rows whose `content_revision` differs
   from the mirror's (idempotent, cheap at kata scale — `content_revision`
   is per-issue, so there is no global cursor to resume from); delete
   mirror rows + `store.DeleteVectors(uid)` for issues that are gone or
   soft-deleted. Incremental optimization (e.g. an `updated_at` watermark)
   is a plan-level detail if full-list scans ever show up in profiles.
2. **Fill** — ensure the desired generation exists (`EnsureGeneration`,
   state building on first sight), then `kitvec.Fill(ctx, store, gen,
   encodeFunc, opts)`.

Error policy stays kata's (kit is explicit that retry/backoff is the
caller's):

- `FillOptions.OnEncodeError`: definitive `APIError` (400/401/403/404) on a
  document → skip it (poison-document handling); anything else → abort the
  fill, reconciler backs off (Retry-After honored, exponential to cap).
- The `EncodeFunc` adapter wraps calls in `recover` → error, because kit
  invokes encoders on its own worker goroutines where the reconciler's
  recovery can't reach (agentsview lesson, `manager.go`).

Cutover: when a fill completes for a non-active generation, activate it,
retire the previous one, and reclaim the retired generation's rows
(vec0 + chunk map + stamps) with local SQL against kit's tables — kit has
no reclamation API yet (known gap, agentsview has the same workaround).
While the new generation fills, the old active generation keeps serving
searches.

Backlog gauge for `ReconcilerHealth`: count of mirror rows not covered by
the active-or-building generation (one query joining the mirror and kit's
stamp table — accepted coupling to kit's table shape, same as agentsview's
coverage queries). Health wire shape unchanged; may gain additive fields
(generation fingerprint, fill progress).

## Search path

Only `runVectorLeg` changes inside `hybridSearch`:

1. Embed the query via the client (3s timeout, unchanged).
2. `store.QueryGeneration` against the **active generation only** —
   deliberately bypassing `kitvec.Search`, which queries all live
   generations with one query vector (dimensions may differ across
   generations; same reasoned divergence as agentsview `search.go`).
3. `kitvec.RollupByDocument` (best chunk per issue).
4. Hydrate: resolve each hit's issue UID against the live `issues` table in
   `kata.db`; drop soft-deleted issues and issues outside the requested
   project. This preserves today's guarantee that soft-delete/purge can
   never leak stale results, without depending on sidecar consistency.
5. Apply `cosineFloor = 0.3` (sqlite-vec cosine score `1 − distance` is on
   the same scale as today's dot product over L2-normalized vectors), feed
   the RRF merge. Mode semantics unchanged: explicit hybrid/semantic
   failure → 503; auto → labeled degrade to lexical. Missing `vectors.db`
   or mirror-version mismatch at read time counts as vector-leg
   unavailable.

**Project scoping / over-fetch:** the KNN index is daemon-global while
search is project-scoped, so the leg over-fetches (`k × 4`, capped at the
existing `fetchCap`) before rollup and project/liveness filtering. For a
daemon dominated by one project this matches today's behavior; a daemon
with many projects could under-fill `k` for small projects. Accepted for
now at kata's scale; escape hatches if it bites: raise the over-fetch
factor, or partition doc keys by project. Flagged as an open question
below.

## Migration and compatibility

- `kata.db` schema migration drops `issue_embeddings`.
- First start after upgrade: `vectors.db` created, mirror backfilled,
  reconciler re-embeds everything. Auto-mode search degrades to lexical
  (with reason) until coverage builds — existing behavior for cold
  embeddings.
- JSONL: export stops emitting `issue_embedding`; the kind constant stays
  recognized by import, which skips such records with a log note.
- pgstore embedding stubs unchanged (Phase 2/3); kit plans a pgvector
  sibling backend, which becomes kata's Phase-2 acceleration path.
- Docs: note that `vectors.db` is disposable derived state (safe to delete;
  excluded from backup guidance), and describe the re-embed-on-upgrade
  behavior in the release changelog.

## Testing

- `internal/embedding` client tests survive nearly as-is; recipe tests adapt
  to RecipeVersion 2 (no truncation).
- `internal/vector` tests run against a real sidecar db (temp file,
  modernc + sqlite-vec) with a deterministic fake `EncodeFunc`: fill and
  backfill, staleness (revision moves mid-fill), poison-document skip,
  generation cutover (old serves during fill; retired rows reclaimed),
  project filtering, soft-delete exclusion, mirror version mismatch
  (write path rebuilds, read path degrades), missing sidecar.
- Reconciler/hybrid-search/RRF tests keep their fakes (the `embedder` seam
  remains an interface).
- JSONL round-trip tests assert old archives containing `issue_embedding`
  records import cleanly (skipped, logged).
- E2e `semantic_search_test.go` keeps running against the stub embeddings
  server.
- TDD per project rules; no content-grep shell tests.

## Open questions / uncertainty (for reviewer)

1. **Project scoping over-fetch** (above): is `k × 4` capped at `fetchCap`
   acceptable, or should doc keys be partitioned by project from day one?
2. **Backlog query coupling**: computing the health backlog reads kit's
   stamp table directly. Alternative: report only mirror-side counts
   (embeddable issues vs. mirror rows) and drop per-generation precision.
3. **Reclamation SQL**: local SQL against kit's `_chunks`/`_stamps`/vec0
   tables mirrors agentsview `resetGeneration`. If kit grows a reclamation
   API, both consumers switch to it.
4. **Where lifecycle state lives**: this design keeps generation cutover
   fully automatic (config-driven, no CLI). agentsview exposes
   build/activate/retire commands; kata can add operator commands later if
   needed (YAGNI for now).
5. **Chunking constants**: `Split` max-runes/overlap start as package
   constants (values to be picked in the implementation plan, informed by
   agentsview's choices), not config keys.
