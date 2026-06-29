# Semantic search technical notes

Status: design note for unshipped work. Remove this line when the feature
lands and fold any drift back into this document.

kata's search is lexical: SQLite FTS5 with BM25 ranking over title, body, and
comments (`internal/db/sqlitestore/queries_search.go`), with the PostgreSQL
equivalent stubbed pending its tsvector implementation. Lexical search misses
issues that describe the same problem in different words — the exact case
that matters for agents running search-before-create. This note describes the
design for semantic search: embedding-based vector retrieval fused with the
existing lexical search, opt-in by configuration, degrading to today's
behavior whenever embeddings cannot help.

## Goals and non-goals

Goals:

- `kata search` finds paraphrased and conceptually-related issues, not just
  token matches, when an embedding endpoint is configured.
- Zero new requirements for deployments that do not opt in: no extensions,
  no network calls, no behavior change to ranking or scores.
- Index freshness is owned by the daemon, never by user discipline. Stale or
  missing embeddings degrade recall gracefully; they never break search.
- Both storage backends reach the same observable contract, by different
  execution strategies where that is the better engineering trade.

Non-goals for v1 (future levers, in rough order of expected value):

- Embedding-assisted duplicate detection. The create-time look-alike
  soft-block keeps its lexical retrieval + Jaccard gate
  (`internal/similarity/`); a follow-up can add a cosine gate over the same
  vectors this design stores.
- Comment and chunk-level vectors (see "Text recipe" for the implications).
- LLM reranking and query expansion (qmd-style stages; they need a
  generation model, which this design deliberately does not require).
- sqlite-vec. The current driver (`modernc.org/sqlite`, pure Go) cannot load
  native extensions. If the driver ever changes for other reasons, a
  sqlite-vec fast path can slot in behind `SearchVector` without contract
  changes; brute force is sufficient at issue-tracker scale (see "SQLite
  vector execution").
- Federating embeddings. They are local derived state; each daemon embeds
  what it stores.
- TUI affordances beyond what arrives transparently through the API.

## Trust, privacy, and credentials

Configuring an embedding endpoint sends issue titles and bodies to that
endpoint. That is the consent boundary: the operator who writes the config
section is authorizing that data flow. The docs recommend a local endpoint
(for example Ollama on loopback) for sensitive projects.

Credential handling follows the existing bearer-token trust model:

- The embedding API key is attached only to requests whose origin exactly
  matches the configured `base_url` origin. The client reuses the
  origin-pinned transport machinery from `internal/config/bearer.go`
  (`bearerTransport`, `CheckBearerTargetSafeURLWithTrust`): cross-origin
  redirects are refused rather than followed, so a key configured for one
  origin can never leak to another.
- Plaintext HTTP targets follow the same safety ladder as other bearer
  targets: HTTPS always allowed; HTTP to loopback allowed; HTTP to literal
  non-public IPs only with `trust_private_network = true` in the embeddings
  section; anything else (public IPs, DNS hostnames) requires HTTPS. v1 has
  no public-plaintext escape hatch — point such targets at an HTTPS endpoint
  or an SSH tunnel.

## Architecture

```
write path:  mutation commit ──nudge──▶ reconciler ──▶ ListEmbedTargets
                                           │              (dirty rows)
                                           ▼
                                     embedding client ──▶ UpsertIssueEmbedding
                                     (batched HTTP)

query path:  search q ──┬─▶ FTS leg (existing SearchFTS) ──┐
                        │                                   ├─▶ RRF merge ─▶ results
                        └─▶ embed(q) ─▶ SearchVector ──────┘
                              │ (3s timeout)
                              └─ failure ─▶ lexical-only, degraded:true
```

Components:

- `internal/embedding` — the OpenAI-compatible embeddings client, the text
  recipe, and the fingerprint. Storage-free: it does not import `internal/db`
  and operates on plain strings (`EmbedText(title, body string)`). Named
  `embedding`, not `embed`, to avoid colliding with the Go stdlib package.
- `internal/db` interface additions — `UpsertIssueEmbedding`,
  `ListEmbedTargets`, `SearchVector`, implemented per backend. Storage never
  infers model state from the daemon: the active fingerprint is a parameter
  to `ListEmbedTargets` and `SearchVector`.
- `internal/daemon` — the reconciler goroutine (started only when
  configured) and the hybrid orchestrator inside the search handler. The RRF
  merge is a pure function.

## Configuration

```toml
[search.embeddings]
base_url = "http://localhost:11434/v1"  # any OpenAI-compatible /embeddings
model    = "nomic-embed-text"
# api_key      = "..."          # or api_key_env = "SOME_VAR"; mutually exclusive
# fingerprint_salt = ""         # bump to force re-embed when weights change
# trust_private_network = false # plaintext HTTP to literal non-public IPs
# timeout_seconds, batch_size, dims  # defaulted; override rarely
```

- Presence of the section enables the feature. `base_url` and `model` are
  both required once the section exists: partial configuration is a startup
  error, not a silent disable.
- `api_key` / `api_key_env` are mutually exclusive, mirroring the daemon
  catalog's `token` / `token_env` pair
  (`internal/config/daemon_config.go`). There is no additional hardcoded
  environment variable; `api_key_env = "KATA_EMBED_API_KEY"` is the
  conventional spelling if an env var is wanted.
- The client POSTs `{model, input: [...]}` to `{base_url}/embeddings` and
  L2-normalizes returned vectors at the boundary, so similarity is a dot
  product everywhere downstream.

## Embedding pipeline

### Text recipe and fingerprint

v1 embeds one vector per issue over `title + "\n\n" + body`, truncated to a
fixed character cap (~8k chars). Comments are deliberately not embedded:

- A new comment is findable immediately through the FTS comments column; it
  never creates embedding staleness or an embedding API call.
- The named gap: comment-only *conceptual* matches (paraphrase that lives
  solely in comments) will not vector-match. Lexical comment matches still
  hit. Chunked comment vectors are the v2 lever if this gap matters in
  practice.

`embed_fingerprint = sha256(model, dims, recipe_version, fingerprint_salt)`.
Any component change marks every row stale and triggers gradual re-embedding.
The salt is the operator's lever for "same model name, different weights"
(for example a re-pulled Ollama model). The endpoint URL is deliberately not
in the fingerprint: moving a port or switching localhost to a tunnel must not
force a full re-embed.

### Reconciler

One goroutine, started only when embeddings are configured. The dirty
predicate over the tables is the queue — there is no separate durable queue
to maintain or to lose.

Dirty means any of:

1. no `issue_embeddings` row (never embedded);
2. `embed_fingerprint != current` (model/recipe/salt changed);
3. `issues.content_revision != issue_embeddings.embedded_content_revision`
   (the embedded source text changed).

The third predicate needs a counter that moves *exactly* when embeddable
content changes. The existing `issues.revision` is unsuitable on both ends:
it is the metadata If-Match counter (`internal/db/sqlitestore/store_metadata.go`),
and title/body edits do not touch it — `editIssue` bumps only `updated_at`
(`internal/db/sqlitestore/queries.go`). Reusing `revision` would therefore
miss the very edits that matter, and broadening it would silently change
metadata optimistic-concurrency semantics. So this design adds a dedicated
`issues.content_revision`, monotonic, bumped by every writer that actually
changes title or body and by nothing else. Today those writers are
`EditIssue`, `EditIssueAtomic` (the active PATCH route,
`internal/daemon/handlers_issues.go`), and issue import
(`updateImportedIssue`, `internal/db/sqlitestore/imports.go`). The two
interactive paths already funnel field changes through
`issueFieldUpdatePlan`, which is the natural bump site — it must distinguish
a title/body change from an owner-only edit, since owner does not bump —
while import bumps in its direct `UPDATE`. Owner, priority, status,
comments, links, and metadata all leave `content_revision` unchanged, and
`revision` is left untouched. Timestamps are not used either: `updated_at`
also moves on non-content mutations (status flips), and its millisecond
precision can collapse same-instant edits.

Soft-deleted issues are excluded from targets while deleted; their rows are
kept so restore costs nothing. Purge removes rows via `ON DELETE CASCADE`.

Cycle: wake on a debounced (~1–2s) post-commit nudge, on startup, and on a
periodic safety sweep (~5m) that recovers anything missed across restarts.
Take up to `batch_size` (default 64) dirty targets, embed them in one batched
request, and upsert rows independently — each carrying the current
`embed_fingerprint` and the issue's `content_revision` — so partial progress
survives. Because `content_revision` moves only on real content change,
status flips and comment adds never enter the queue, so there is no
hash-recompute or no-op-touch step. The one inefficiency is a title/body edit
that reverts to a prior value (`A→B→A`): the counter advances both times, so
the unchanged content is re-embedded once. That is rare and harmless, and
buys a simpler, exact dirty signal over carrying a content hash.

Failure classes:

- 401 / 403 / 404 / 400 model-not-found — definitive misconfiguration: pin
  backoff at the maximum immediately (no hot loop) and surface the error in
  health.
- 429 — honor `Retry-After` when present, otherwise normal backoff.
- 5xx / timeouts / connection errors — exponential backoff, 1s doubling to a
  5m cap.

Reconciler health — `{configured, last_success_at, last_error, backlog}` —
joins the `/health` payload (following the `api_schema_version` reporting
precedent) and is the operator's view of index freshness.

### Freshness contract

- **Lexical: same-commit.** FTS triggers fire in the writing transaction;
  create-then-search (including the agent duplicate-check pattern) behaves
  exactly as today.
- **Semantic: eventual, seconds.** Nudge debounce plus one batch call —
  typically under ~5s against a local endpoint, somewhat more against a
  cloud API, unbounded only while the embedder is down (visible in
  `/health`). Until a row is (re-)embedded it contributes nothing to the
  vector leg and is carried by the FTS leg; an edited issue serves its old
  vector for the lag window while FTS already ranks the new text.

An issue is therefore never unsearchable because of embedding lag; only its
semantic recall lags.

## Storage

### Canonical tables (both backends)

```sql
CREATE TABLE issue_embeddings (
  issue_id                  INTEGER/BIGINT PRIMARY KEY
                            REFERENCES issues(id) ON DELETE CASCADE,
  embedded_content_revision INTEGER NOT NULL,  -- issues.content_revision at embed time
  embed_fingerprint         TEXT NOT NULL CHECK (length(embed_fingerprint) = 64),
  dims                      INTEGER NOT NULL CHECK (dims > 0),
  vector_bytes              BLOB/BYTEA NOT NULL,  -- dims × float32 LE, L2-normalized
  updated_at                TEXT/timestamptz NOT NULL,
  CHECK (length(vector_bytes) = dims * 4)   -- octet_length() on PostgreSQL
);
```

The column is `vector_bytes`, not `vector`, so nothing reads as pgvector's
`vector(N)` type. The table is canonical schema on both backends and rides a
schema-version bump that also adds the `issues.content_revision` column
(default 0) and the title/body writer bumps above: SQLite upgrades via the
usual JSONL cutover; PostgreSQL follows the existing operator-migration
policy (`internal/db/pgstore/open.go` refuses version mismatches).

pgvector is deliberately absent from the canonical schema. PostgreSQL
deployments that never enable embeddings carry no extension requirement.

### JSONL export/import

`issue_embeddings` rows are exported (vectors base64-encoded) and keyed by
`issue_uid`, with the numeric id treated as a local detail resolved at
import. Embeddings are expensive derived state — re-embedding a large
project costs real API calls and time — so they earn export the way cheap,
trigger-rebuilt FTS state does not. Live-only exports emit an embedding row
only when its parent issue is in the export set.

For the export to actually save work, the issue's `content_revision` must
travel with it. `IssueExport` (`internal/db/export_types.go`) carries
`content_revision` alongside the existing `revision`, and import restores
it. Otherwise an imported issue defaults to `content_revision = 0` while its
embedding row carries the revision it was embedded at, so the dirty
predicate would mark every imported-and-previously-edited issue stale and
re-embed it on the first sweep — exactly the cost export is meant to avoid.
Embedding import also validates `embedded_content_revision <=
issues.content_revision`; a row that fails (a corrupt or hand-edited dump)
is dropped and left for the reconciler rather than trusted.

### SQLite vector execution: brute force behind a cache

At issue-tracker scale (1–10k issues per project at 768 dims ≈ 3–30MB,
~1–5ms to scan; 50k ≈ ~30ms) exhaustive cosine is sufficient and avoids any
driver or extension dependency.

The cache is keyed by `(project_id, embed_fingerprint)` and holds
`(issue_id, vector)` for rows at that fingerprint only — a model swap creates
a fresh entry under the new fingerprint and abandons the old one, so
stale-fingerprint vectors can never be reused. It is consulted under one
invariant: **the cache supplies candidates and similarities, never
visibility or row data.** Every
`SearchVector` resolves candidate ids against the live `issues` table —
project scope, `deleted_at` filter, and all returned fields come from that
final query. Soft delete, restore, and purge therefore cannot make the cache
return a wrong result set; a stale entry either fails the join (purged) or
is filtered (deleted). What cache staleness *can* do is rank an edited issue
on slightly-old text until the reconciler catches up — bounded by the
freshness contract, and the FTS leg ranks the fresh text in the same merge.

Exact top-k under filtering: the linear scan ranks the entire cached set
anyway, so the query walks the ranked list, joining live rows in batches,
until k results are found or candidates are exhausted — filtered-out ids
never silently shrink the result set.

Cache freshness is verified per query with one cheap probe —
`(count, max(updated_at))` over the project's rows *at the active
fingerprint* (`WHERE embed_fingerprint = ?`), matching the cache key — rather
than trusting in-process invalidation alone, which also keeps multi-daemon
shared-database setups correct.

### PostgreSQL vector execution: pgvector as best-effort acceleration

When available, acceleration uses a derived table —
`issue_embeddings_vec(issue_id PK REFERENCES issues(id) ON DELETE CASCADE,
vec vector(N))` with an HNSW cosine index — maintained in the same
transaction as the canonical row, rebuildable from `vector_bytes` at any
time, and dropped/recreated on fingerprint change (N can change with the
model).

"Available" is probed, not assumed: the extension must be present in
`pg_extension` *and* the idempotent ensure-DDL must succeed under
`pg_advisory_xact_lock`. Any failure — missing extension, insufficient
privileges, concurrent DDL — logs and falls back to the same brute-force
path SQLite uses (BYTEA scan with the cache invariant above). Acceleration
is a capability, not a mode: search behaves identically either way, modulo
latency at scales SQLite deployments do not reach.

Queries against the HNSW index apply the visibility join inside the query
and rely on pgvector ≥ 0.8 iterative index scans, with oversample-and-refill
as the fallback, because HNSW post-filtering can under-return.

Daemons sharing one PostgreSQL database must share embedding configuration.
The advisory lock prevents DDL corruption; the shared-config requirement
prevents two daemons with different fingerprints from thrashing the derived
table through drop/recreate cycles. Duplicate reconciler work across daemons
is harmless (last-write-wins upserts of identical vectors).

## Query pipeline

### Modes

`mode = auto | lexical | hybrid | semantic` (CLI: `--lexical` / `--hybrid` /
`--semantic`; mutually exclusive flags).

- `auto` (default): hybrid when embeddings are configured, lexical
  otherwise. Degradation within auto is silent-but-labeled.
- `lexical`: exactly today's FTS path.
- `hybrid`, `semantic` (explicit): deterministic or an honest error — 400
  when embeddings are unconfigured, 503 when the vector leg cannot run
  (query embed failure or vector store failure). Never a silent downgrade.
- `semantic` against an empty current-fingerprint index (backfill not yet
  complete) is 200 with empty results: the request was served correctly;
  `/health` backlog explains the emptiness.

### Execution

Both legs start concurrently — the FTS leg never waits on the embedder.
Per-leg fetch depth is `max(limit*3, 50)`, capped at 200. The vector leg
embeds the query (3s timeout; query text truncated to the recipe cap for
embedding only — the lexical leg and the response's `query` echo always use
the original), runs `SearchVector` with the current fingerprint, and drops
hits below cosine 0.3 so weak vectors do not pad results. The fingerprint
parameter makes cross-model comparison structurally impossible: rows
embedded under a previous model are invisible to the vector leg until
re-embedded, and the FTS leg carries them meanwhile.

Fusion is reciprocal rank fusion with k=60 and equal leg weights:
`score(d) = Σ_legs 1/(60 + rank_leg(d))`. Ties break deterministically
(`updated_at` desc, then id). `matched_in` keeps its FTS column values and
gains `"semantic"` when the vector leg contributed the document.

There is no BM25-probe shortcut (qmd's optimization): the probe exists to
dodge expensive LLM query-expansion stages this design does not have, and
with legs running in parallel a probe saves nothing.

`degraded` is narrowly defined: the vector leg could not run or complete
*for this query* (query embed failure, dims mismatch with the configured
model, vector store failure) in a mode that wanted it. A nonzero reconciler
backlog is health state, not per-query degradation.

`include_deleted=true` lets the vector leg rank soft-deleted issues on
their last-known vectors (rows are not re-embedded while deleted) —
acceptable for an explicitly archaeological flag.

## API and CLI contract

`SearchResponse` gains `mode` (always present), `degraded` and
`degraded_reason` (omitempty). `score` semantics are mode-scoped, documented
at `SearchHit` (`internal/api/types.go`): lexical → negated BM25 exactly as
today; hybrid → RRF score; semantic → cosine similarity.

This is explicit API evolution, not strict invisibility: `api_schema_version`
takes a minor bump, the compatibility doc records that *ranking and score
semantics of unconfigured search are unchanged* while the response gains
`mode: "lexical"` and `/health` gains reconciler state. The alternative —
omitting fields unless configured — was rejected because clients could not
distinguish an old daemon from an unconfigured one, undermining the purpose
of version reporting. An always-present `mode` also tells agents whether
semantic search is even on. A CLI/API compatibility test pins the
unconfigured-default response shape and score semantics.

The CLI has three output surfaces, each handled to a precise shape:

- `--json`: the response envelope is passed through unchanged
  (`printSearchResults` JSON branch, `cmd/kata/search.go`), so `mode`,
  `degraded`, and `degraded_reason` appear automatically.
- Agent mode (`cmd/kata/search.go:116`) is a machine surface governed by
  `docs/reference/agent-output.md`, where field order is part of the
  contract and new fields may be *appended* without an `agent_format`
  version bump. So `mode` is appended after the existing header fields —
  exact order `OK search count=<n> query=<q> mode=<mode>` — and
  `degraded=<reason>` follows only when the query degraded (nullable fields
  are omitted when absent, per the contract). Result rows gain `semantic` in
  their comma-separated `matched=` list when the vector leg contributed.
  Because `count` and `query` keep their names, positions, and meanings,
  this is purely additive and `agent_format` stays `1`; the HTTP API is what
  takes the `api_schema_version` bump, since its JSON envelope always carries
  `mode`. The compatibility test pins the appended field order, `mode=lexical`
  on an unconfigured daemon, and unchanged lexical score semantics.
- Human mode (`%-8s  %.2f  %-8s  %s  (%s)` per row, `cmd/kata/search.go:142`)
  is an ergonomic surface. The header rule is keyed on whether the output is
  the plain baseline, not on the effective mode alone: a leading
  `# mode=<mode>` line (carrying a `degraded: <reason>` note when present)
  prints whenever `mode` is hybrid/semantic **or** `degraded` is true.
  Baseline lexical — `mode=lexical` and not degraded, which covers an
  unconfigured daemon, `auto` with embeddings off, and explicit `--lexical`
  — stays byte-identical to today (no header, `%.2f`) and is pinned by the
  compatibility test. This split is what keeps `auto` degradation
  "silent-but-labeled" rather than silent: when `auto` falls back because the
  embedder is down, the effective mode is `lexical` but `degraded` is true,
  so a `# mode=lexical degraded: <reason>` line still tells the human that
  semantic results are missing. Scores render with `%.4f` only for hybrid and
  semantic (RRF/cosine values cluster around 0.01–0.03, which `%.2f` would
  flatten); degraded-lexical results are ordinary BM25 and keep `%.2f`.

## Failure modes

| Failure | Behavior |
| --- | --- |
| Embeddings unconfigured | `auto` → lexical, `mode:"lexical"`, not degraded (it is the baseline); explicit `hybrid`/`semantic` → 400 |
| Embedder unreachable at query | `auto` → lexical + `degraded:true` + reason; explicit `hybrid`/`semantic` → 503 |
| Query embed dims ≠ configured dims | Treated as embed failure (degraded / 503) + health error |
| Embedder unreachable in reconciler | Backoff per failure class; backlog grows; search unaffected (FTS carries) |
| Definitive 4xx in reconciler | Max backoff immediately + health error; no hot loop |
| Rows not yet embedded / model swap in progress | Invisible to vector leg (fingerprint filter), carried by FTS leg |
| pgvector DDL/index failure | Log + brute-force fallback; feature works without acceleration |
| Cache staleness | Ranking-only effect; visibility always resolved against live `issues` |

## Testing

TDD throughout (red, green, refactor). Highlights, not an exhaustive list:

- `internal/embedding` against an `httptest` fake: wire shape, key attached
  only to the pinned origin, cross-origin redirect refusal (mirroring the
  `bearer.go` tests), batching, timeout, 429-with-Retry-After versus 401
  classification, truncation, fingerprint composition (each component
  independently changes it), L2 normalization.
- RRF as a pure function: overlapping/disjoint/empty legs, similarity floor,
  determinism and tie-breaks.
- Storage conformance suite shared by both backends (pgstore joins in
  Phase 2): upsert round-trips including CHECK violations (dims, vector
  length, fingerprint length); the three dirty predicates (missing,
  fingerprint mismatch, `content_revision` mismatch); that `content_revision`
  bumps via every title/body writer (`EditIssue`, `EditIssueAtomic`, import)
  but not on owner-only edits, status flips, or comment adds; fingerprint
  filtering; visibility joins under soft-delete/restore/purge; exact top-k
  refill with deleted-heavy caches; the fingerprint-scoped cache key and
  freshness probe; JSONL round-trip carrying `content_revision` so a
  previously-edited issue's imported embedding is not falsely stale, plus the
  `embedded_content_revision <= content_revision` validation dropping bad
  rows; live-only exclusion of unexported parents.
- Daemon: reconciler against a fake embedder (backfill, nudge debounce,
  model-swap gradual migration, backoff classes, health fields); handler
  mode-resolution matrix (hung embedder returns FTS results degraded;
  explicit-mode 400/503; semantic-on-empty-index 200).
- CLI/API compatibility test pinning the unconfigured-default search across
  surfaces: JSON envelope and agent status line carry `mode:"lexical"` /
  `mode=lexical`; human output is byte-identical to today; lexical score
  semantics (negated BM25) unchanged. A companion human-rendering test pins
  that degraded `auto` (embedder down, effective `mode=lexical`,
  `degraded:true`) *does* print the `# mode=lexical degraded: <reason>` note
  — distinguishing baseline lexical from degraded lexical.
- e2e with a deterministic fixture-map embedder and neutral placeholder
  data: create → instant lexical hit; after reconcile → paraphrase found
  semantically; embedder killed → degraded lexical.
- Explicitly untested: real model quality; no network in tests.

## Phasing

- **Phase 1 — SQLite end-to-end plus all backend-neutral machinery**: the
  `internal/embedding` client, config section and validation, storage
  interface methods with the sqlitestore implementation, reconciler, RRF
  merge and mode resolution, API/CLI surface, JSONL export/import, health
  reporting.
- **Phase 2 — PostgreSQL parity**: implement `SearchFTS`/`SearchFTSAny`
  over the existing tsvector machinery (currently stubbed in
  `internal/db/pgstore/stubs_gen.go`), splitting or weighting the tsvector
  so `matched_in` keeps parity with SQLite; the pgvector acceleration path
  with probe + advisory locking; pgstore joins the storage conformance
  suite.

## Future work

In rough order of expected value: embedding-assisted duplicate detection
(cosine gate beside Jaccard in the look-alike soft-block); comment/chunk
vectors; TUI degraded-state affordance in the search bar; LLM rerank and
query expansion; sqlite-vec fast path if the driver story changes;
quantization (int8) if vector storage size ever matters.
