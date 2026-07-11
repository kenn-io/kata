# Semantic search technical notes

Status: implemented. This note captures the semantic search design
rationale; for current operator-facing behavior see the
[semantic search guide](../guide/semantic-search.md) and
[Configuration](../reference/configuration.md#semantic-search). The
"Storage" section reflects the kit-based sidecar vector index
(`kata.vectors.db`) that replaced the single-table brute-force design this
note originally described.

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
- Comment vectors (see "Text recipe" for the implications). Chunked
  title+body embeddings already ship (see "Storage"); comments remain
  untouched.
- LLM reranking and query expansion (qmd-style stages; they need a
  generation model, which this design deliberately does not require).
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
write path:  mutation commit ──nudge──▶ reconciler ──▶ RefreshMirror
                                           │              (issue_mirror in kata.vectors.db)
                                           ▼
                                     kitvec.Fill ──▶ embedding client (batched HTTP)
                                     (active-or-building generation)

query path:  search q ──┬─▶ FTS leg (existing SearchFTS) ──┐
                        │                                   ├─▶ RRF merge ─▶ results
                        └─▶ embed(q) ─▶ Index.Query ───────┘
                              │ (3s timeout)         (active generation only)
                              └─ failure ─▶ lexical-only, degraded:true
```

Components:

- `internal/embedding` — the OpenAI-compatible embeddings client and the text
  recipe. Storage-free: it does not import `internal/db` and operates on
  plain strings (`EmbedText(title, body string)`). Named `embedding`, not
  `embed`, to avoid colliding with the Go stdlib package.
- `internal/vector` — owns the `kata.vectors.db` sidecar (mirror table,
  generation lifecycle, fill, and query), built on `go.kenn.io/kit/vector`
  and `go.kenn.io/kit/vector/sqlitevec`. See "Storage" below. `internal/db`
  contributes one read method, `ListIssueContent`, that feeds the mirror; it
  has no per-vector write or search path of its own.
- `internal/daemon` — the reconciler goroutine (started only when
  configured), which refreshes the mirror and drives the generation fill,
  and the hybrid orchestrator inside the search handler. The RRF merge is a
  pure function.

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

kata embeds `title + "\n\n" + body` per issue, chunked via kit's
`vector.Split` (2000 runes per chunk, 200-rune overlap) instead of truncated
to a fixed cap, so long issues get full semantic coverage rather than losing
everything past the old ~8k-character cutoff. Comments are deliberately not
embedded:

- A new comment is findable immediately through the FTS comments column; it
  never creates embedding staleness or an embedding API call.
- The named gap: comment-only *conceptual* matches (paraphrase that lives
  solely in comments) will not vector-match. Lexical comment matches still
  hit.

The generation fingerprint is `kitvec.Generation{Model, Dimensions,
Params}.Fingerprint()` over `{model, dims, recipe_version,
fingerprint_salt}`. Any component change starts a *new generation* rather
than marking existing rows stale in place: the reconciler fills it in the
background while the previous generation keeps serving searches, then cuts
over automatically once the fill completes and reclaims the retired
generation's storage (see "Storage"). Mid-swap the vector leg is
**unavailable**: the active generation's fingerprint no longer matches the
configured embedder's, and scoring a new-model query vector against
old-model stored vectors would be meaningless (same dims) or an error (dims
change), so the search handler refuses the leg — auto degrades to labeled
lexical, explicit hybrid/semantic return 503 — until the cutover. Lexical
search carries throughout, and semantic results resume the moment the new
generation activates. The salt is the operator's lever for "same model name,
different weights" (for example a re-pulled Ollama model). The endpoint URL
is deliberately not in the fingerprint: moving a port or switching localhost
to a tunnel must not force a full re-embed.

### Reconciler

One goroutine, started only when embeddings are configured. Each cycle does
two things: refresh the sidecar mirror from the canonical store (upsert rows
whose `content_revision` or `project_uid` differs from the mirror's; remove
rows — and their vectors in every generation — for issues that left the
feed: purged, or their project deleted), then run kit's `Fill` for the
desired generation, which embeds
any mirror row that generation doesn't yet cover. There is no separate
durable queue: the mirror's `content_revision` column plus kit's own
per-generation coverage bookkeeping is the queue.

A mirror row needs (re-)embedding under the active-or-building generation
when either:

1. it has no coverage yet in that generation (never embedded, or the
   generation is new); or
2. its `content_revision` changed since the generation last covered it (the
   embedded source text changed).

A model/dims/salt change does not mark existing rows dirty in place — it
starts a new generation instead (see "Text recipe and fingerprint" above).

The `content_revision` counter needs to move *exactly* when embeddable
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

Soft-deleted issues are excluded from `ListIssueContent` (the mirror's
feed). This is a privacy contract, not a convenience: mirror content is what
gets sent to the configured embedding endpoint, and deleting an issue must
stop that outbound flow — otherwise every initial sidecar build or
generation rebuild would re-send historical deleted content to the provider.
The next refresh after a deletion removes the mirror row and its vectors in
every generation (the same path that handles purge and project deletion), so
an `include_deleted` search ranks soft-deleted issues lexically only; their
semantic recall returns when a restore puts them back in the feed and the
reconciler re-embeds them.

Cycle: wake on a debounced (~1–2s) post-commit nudge, on startup, and on a
periodic safety sweep (~5m) that recovers anything missed across restarts.
The embedding client still batches up to `batch_size` (default 64) inputs
per request; kit's `Fill` drives how many chunks go into a batch and upserts
each chunk's coverage independently, so partial progress survives a crash or
error mid-fill. Because `content_revision` moves only on real content
change, status flips and comment adds never enter the queue, so there is no
hash-recompute or no-op-touch step. The one inefficiency is a title/body edit
that reverts to a prior value (`A→B→A`): the counter advances both times, so
the unchanged content is re-embedded once. That is rare and harmless, and
buys a simpler, exact dirty signal over carrying a content hash.

Failure classes:

- 400 — ambiguous: the same status covers a request-level problem (bad model
  name, malformed request, oversized batch) and a document the model
  permanently rejects. The fill verifies which by replaying the failing
  document's exact request shape — same chunk count, per-chunk lengths, and
  batching — with benign text. If the replay succeeds, the 400 was
  content-specific: the document is stamped as skipped (it stops being
  pending and gains no semantic recall until its content changes) and the
  fill continues past it. If the replay also fails, the 400 is request-level
  and handled as definitive misconfiguration below — a systemic 400 must
  never stamp the corpus as skipped.
- 401 / 403 / 404 / request-level 400 — definitive misconfiguration: pin
  backoff at the maximum immediately (no hot loop) and surface the error in
  health.
- 429 — honor `Retry-After` when present, otherwise normal backoff.
- 5xx / timeouts / connection errors — exponential backoff, 1s doubling to a
  5m cap. The backlog gauge is published before each fill starts, decreases
  after every successfully persisted document, and is refreshed when a fill
  fails partway and after success. `/health` therefore exposes live progress
  during a long backfill without reporting stale zero across an outage.

Reconciler health includes current-generation embedded, skipped, and pending
coverage plus progress timing and ETA alongside provider status. It joins the
`/health` payload (following the `api_schema_version` reporting precedent) and
is the operator's view of index freshness.

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

### Sidecar database

Vectors live in a SQLite sidecar the daemon opens (creating it if absent)
next to the main database the first time `[search.embeddings]` is configured
— `internal/vector.Open`. Its name is derived from the database filename
(`kata.db` → `kata.vectors.db`) so two databases in one directory never
share sidecar state. It is derived state, rebuildable from
`kata.db` at any time: a structural mismatch in the mirror schema version
(`vector_meta`) deletes and recreates the file rather than migrating it in
place, and an operator can delete it outright — the reconciler rebuilds it
by re-embedding on the next cycle. It is therefore excluded from backup
guidance and not part of the JSONL export contract (see "JSONL export/import"
below).

kata owns two tables in the sidecar: `vector_meta` (the schema-version guard
above) and `issue_mirror(issue_uid TEXT PRIMARY KEY, project_uid TEXT,
content TEXT, content_revision INTEGER, embed_gen TEXT)` — `content` is the
rendered recipe (`title + "\n\n" + body`, untruncated), and `embed_gen` is
kit's nullable per-row generation stamp. kit's `sqlitevec.Store` owns the
rest, prefix-qualified from `vectorsPrefix = "issue_vectors"`:
`issue_vectors_generations`, `issue_vectors_chunks`, `issue_vectors_stamps`,
and one `vec0` virtual table per generation (`issue_vectors_v<ordinal>`,
cosine metric) created via `sqlitevec.New[string, string]`. Doc keys are
issue UIDs; kata never writes chunk rows directly — `kitvec.Fill` chunks
content (`vector.Split`, 2000 runes with 200-rune overlap) and writes chunks,
stamps, and vec0 rows together.

### Generation lifecycle and cutover

The desired generation is derived from config
(`kitvec.Generation{Model, Dimensions, Params: {"recipe", "salt"}}`); its
fingerprint is the generation key. `EnsureBuilding` creates a `building`
generation the first time a fingerprint is seen, without disturbing whichever
generation is `active`. The reconciler runs `kitvec.Fill` against the
building generation while the active one keeps answering searches
unchanged. When a fill completes for a non-active generation, `CutOver`
activates it and marks every other generation `retired`, then reclaims each
retired generation's storage — drops its `vec0` table, deletes its
`_chunks`/`_stamps` rows — with local SQL, since kit has no reclamation API
yet (a workaround other kit consumers share). Reclaim is unconditional and
idempotent, so a crash between retire and reclaim self-heals on the next
cutover. Cold start is the deliberate exception to build-then-cutover: when
no generation is active (fresh sidecar, or the first start after an upgrade
that reset the sidecar), the reconciler cuts the new generation over
immediately — before the fill — so search serves partial results during the
initial backfill and the health backlog explains the coverage. `Index.Query`
reads only the active generation — never a building one — so a model swap
never exposes a partially-filled index to queries.

### JSONL export/import

Vectors are not exported. JSONL export dropped the `issue_embedding` record
kind kata's earlier single-table embeddings design used to emit; the
sidecar is treated the same as SQLite's FTS index — cheap, trigger/
reconciler-rebuilt derived state that does not earn export cost. Import
still recognizes the `issue_embedding` kind for archives written by older
kata versions: it acknowledges and drops each such record without error, and
the reconciler re-embeds the affected issues on the next cycle from live
content. `content_revision` still travels on `IssueExport`
(`internal/db/export_types.go`) so an imported issue's mirror refresh is
driven by the same revision the source daemon had, independent of whether
the archive carried any embedding data.

### SQLite vector execution: sqlite-vec KNN per generation

Queries run kit's `sqlitevec` `vec0` KNN against the active generation's
table, not a brute-force scan — `kit/vector/sqlitevec` registers the
sqlite-vec extension through `modernc.org/sqlite`'s pure-Go `vec_f32(?)`
literal path, so kata's no-cgo build carries no native extension dependency.
`Index.Query` (`internal/vector/query.go`) issues the KNN query, then
`kitvec.RollupByDocument` collapses per-chunk hits to one score per issue
(best chunk wins). The search handler (`internal/daemon/hybrid_search.go`)
resolves each hit's issue UID against the live `issues` table before
returning it — project scope, `deleted_at` filtering, and all returned
fields come from that query, so sidecar staleness can affect ranking only,
never visibility: a stale hit either fails the join (purged) or is filtered
(deleted, wrong project). The KNN index is daemon-global while search is
project-scoped, so the vector leg over-fetches (`fetchCap`, 200) before
rollup and the visibility join, then applies `cosineFloor = 0.3` so weak
hits do not pad results. When the first batch comes back full but filters
down to fewer in-project hits than wanted (another project's higher-scoring
chunks crowded the batch), the leg retries once at `knnDeepLimit` (1000)
before giving up — one bounded retry, not a loop.

### PostgreSQL: not supported

Semantic search requires the SQLite backend today. A daemon started with
`[search.embeddings]` configured against a non-SQLite DSN fails at startup
with a clear configuration error (`internal/vector` needs a `kit/vector`
store backend that does not exist for PostgreSQL yet) rather than silently
running lexical-only. kit is expected to grow a pgvector sibling backend;
that becomes kata's PostgreSQL acceleration path when it lands, in place of
the brute-force-with-pgvector-acceleration design this note originally
sketched for PostgreSQL.

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
Per-leg output depth is `max(limit*3, 50)`, capped at 200. The vector leg
first checks that the active generation's fingerprint matches the configured
embedder's — on mismatch (model change mid-backfill) the leg is unavailable
(degraded / 503) rather than scoring a new-model query against old-model
vectors — then embeds the query (3s timeout, `embedding.EmbedText` —
unchunked, same as any query string), queries the active generation for
`fetchCap = 200` raw KNN hits (over-fetched ahead of the project/liveness
hydration in "Storage", with one bounded deep retry at `knnDeepLimit` when a
full batch filters down short), and drops hits below cosine 0.3 so weak
vectors do not pad results. Serving only the fingerprint-matching active
generation makes cross-model comparison structurally impossible: rows still
in a building generation are invisible to the vector leg until that
generation cuts over, and the FTS leg carries them meanwhile.

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

`include_deleted=true` ranks soft-deleted issues through the lexical leg
only: their mirror rows and vectors are removed at the first refresh after
deletion (see "Reconciler" — deleted content must not keep flowing to the
embedding endpoint), and hydration serves live issues only regardless of
`include_deleted`, so the contract holds per request even in the window
between a soft delete and the refresh that removes its stale vectors.

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
| Rows not yet embedded (reconciler backlog) | Invisible to vector leg (not yet stamped in the active generation), carried by FTS leg; not degradation |
| Model swap in progress (active generation fingerprint ≠ configured embedder) | Vector leg unavailable until cutover: `auto` → lexical + `degraded:true`; explicit `hybrid`/`semantic` → 503 |
| Non-SQLite backend with embeddings configured | Daemon fails to start with a configuration error |
| Missing or version-mismatched `kata.vectors.db` at query time | Treated as vector-leg failure (degraded / 503) |
| Mirror/fill lag (edited issue before next reconcile) | Ranking-only effect; visibility always resolved against live `issues` |

## Testing

TDD throughout (red, green, refactor). Highlights, not an exhaustive list;
the storage-conformance bullet below describes the original v1 test plan
against the single-table design — `internal/vector`'s actual suite covers
the sidecar equivalents instead: mirror refresh and staleness, fill and
backfill, generation cutover with reclaim, project filtering, soft-delete
retention (removal only on purge or project deletion), and mirror
version-mismatch handling.

- `internal/embedding` against an `httptest` fake: wire shape, key attached
  only to the pinned origin, cross-origin redirect refusal (mirroring the
  `bearer.go` tests), batching, timeout, 429-with-Retry-After versus 401
  classification, the no-truncation recipe, generation fingerprint
  composition (each component independently changes it), L2 normalization.
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
  so `matched_in` keeps parity with SQLite; semantic search stays
  SQLite-only (hard startup error otherwise, see "Storage") until kit ships
  a pgvector sibling backend for its vector store, at which point kata
  adopts it as the PostgreSQL acceleration path.

## Future work

In rough order of expected value: embedding-assisted duplicate detection
(cosine gate beside Jaccard in the look-alike soft-block); comment vectors;
TUI degraded-state affordance in the search bar; LLM rerank and query
expansion; quantization (int8) if vector storage size ever matters.
