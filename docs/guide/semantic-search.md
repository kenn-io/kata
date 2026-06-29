# Semantic search

By default `kata search` is lexical: it matches the words you type against issue
titles, bodies, and comments. That misses issues that describe the same problem
in different words — the case that matters most when an agent searches before
creating a duplicate. Semantic search adds a vector (embedding) leg so a query
like "auth redirect duplicates" can surface an issue titled "Login callback
double-submits on Safari" even though they share no keywords.

Semantic search is **opt-in**. With no embedding endpoint configured, `kata
search` behaves exactly as before and the daemon makes no network calls. This
page explains how to turn it on and how it behaves; for the exact config fields
see [Configuration](../reference/configuration.md#semantic-search), and for the
command flags see the [CLI reference](../reference/cli.md).

## How it works

When you configure an embedding endpoint, the daemon embeds each issue's title
and body into a vector and stores it alongside the issue. A search then runs two
legs and fuses them:

- the **lexical leg** — the existing full-text search, unchanged; and
- the **vector leg** — embeds your query and finds issues whose vectors are
  closest to it.

Results are merged with reciprocal rank fusion, so an issue that ranks well in
either leg surfaces, and an issue that ranks well in both rises to the top.

Embeddings are produced by an **OpenAI-compatible `/embeddings` endpoint** that
you point kata at. That can be a local runtime (Ollama, LM Studio, or a
llama.cpp server) or a hosted provider (OpenAI, Voyage, and others) — kata only
speaks the wire format and never bundles a model.

## Enabling it

1. Run an embeddings endpoint. For a fully local setup, install
   [Ollama](https://ollama.com) and pull an embedding model:

   ```sh
   ollama pull nomic-embed-text
   ```

2. Add a `[search.embeddings]` block to `<KATA_HOME>/config.toml`:

   ```toml
   [search.embeddings]
   base_url = "http://localhost:11434/v1"   # any OpenAI-compatible /embeddings
   model    = "nomic-embed-text"
   ```

   `base_url` and `model` are both required once the section exists. A hosted
   provider also needs a key — set `api_key` or, better, `api_key_env` pointing
   at an environment variable. See
   [Configuration](../reference/configuration.md#semantic-search) for every
   field (`dims`, `batch_size`, `timeout_seconds`, `fingerprint_salt`,
   `trust_private_network`).

3. Restart the daemon (or start it — `kata` will pick up the config). It begins
   embedding existing issues in the background immediately.

4. Confirm it is live and watch the backfill drain. The reconciler state is in
   the `embeddings` object of the JSON health response:

   ```sh
   kata health --json   # see the "embeddings" object: backlog, last_success_at
   ```

That is the whole setup. New and edited issues are embedded automatically from
then on.

## Search modes

`kata search` takes three mutually exclusive flags; with none of them it runs in
**auto** mode:

| Mode | Flag | Behavior |
| --- | --- | --- |
| auto (default) | *(none)* | Hybrid when embeddings are configured, lexical otherwise. |
| lexical | `--lexical` | Full-text search only — today's behavior. |
| hybrid | `--hybrid` | Fuse the lexical and vector legs. |
| semantic | `--semantic` | Vector results only. |

Auto is the right default for almost everything, including agents: it transparently
improves recall when embeddings are on and is unchanged when they are off. Reach
for `--lexical` when you want an exact keyword/identifier lookup, or `--semantic`
when you want pure concept matching regardless of wording.

The effective mode is reported back on every response. In `--json` and `--agent`
output it appears as a `mode` field (`lexical`, `hybrid`, or `semantic`); see the
[agent output reference](../reference/agent-output.md) for the exact shape.

## Freshness: lexical is instant, semantic is eventual

Lexical search is always up to date the moment you write an issue — creating an
issue and immediately searching for it works exactly as before, which keeps the
agent search-before-create flow reliable.

The vector index is **eventually consistent**. A background reconciler embeds new
and edited issues a few seconds after they change (sooner against a fast local
endpoint, longer against a busy cloud API). A brand-new issue is not in the
vector leg until its first embedding lands — it is still found lexically, so
nothing becomes unsearchable. An *edited* issue keeps serving its previous
vector (so the vector leg may rank it on the old text) until the reconciler
re-embeds it; the lexical leg already reflects the new text in the same results.
Either way the staleness is brief and bounded by reconciler lag.

You can watch the reconciler in `kata health --json` under `embeddings`:

- `configured` — whether an endpoint is set;
- `backlog` — how many issues are waiting to be (re-)embedded; trends to 0;
- `last_success_at` — when the reconciler last completed a batch;
- `last_error` — the most recent embedding failure, if any.

## When the endpoint is unavailable

If the embedding endpoint is unreachable for a query, behavior depends on the
mode:

- **auto** degrades to lexical results and labels the response so the fallback is
  never silent: a `# mode=lexical degraded: …` note in human output, `degraded`
  and `degraded_reason` fields in `--json`, and a `degraded=<reason>` field in
  `--agent`.
- **explicit `--hybrid` / `--semantic`** do not degrade — they return an error
  (HTTP 503) so a caller that asked for semantic results knows it did not get
  them. They return 400 when embeddings are not configured at all.

A persistent endpoint problem shows up as a growing `backlog` and a `last_error`
in health; the reconciler backs off and retries, and a misconfiguration (bad
key, wrong model) is reported there rather than silently looping.

## Changing the model

Each stored vector records a fingerprint of the model, dimensionality, and text
recipe it was produced under. If you switch `model`, kata notices the
fingerprint changed and re-embeds every issue under the new model in the
background; until a given issue is re-embedded it is carried by the lexical leg
and never compared across models. If you keep the same model name but its weights
changed underneath you (for example a re-pulled Ollama tag), bump
`fingerprint_salt` to force the same re-embed.

## Privacy and federation

Configuring an endpoint sends issue titles and bodies to it on every embed —
that is the consent boundary. For sensitive projects, prefer a local endpoint
(such as Ollama on loopback) so issue text never leaves the host. The embedding
API key is only ever sent to the configured `base_url` origin.

Embeddings are local derived state and **do not federate**: each daemon embeds
only what it stores, and no vectors are sent to or pulled from federated hubs.
They are included in JSONL [backup/export](../operations/backup-restore.md) so a
backup or schema upgrade does not force a full re-embed.

## Scope and limits

- Only issue **title and body** are embedded today; comments are searchable
  lexically but a paraphrase that lives only in a comment will not vector-match.
- The PostgreSQL backend stores the same embeddings, but accelerated vector
  search there (pgvector) is not yet implemented; semantic search currently runs
  on the SQLite backend.

For the design rationale and internals, see the
[semantic search design note](../design/semantic-search.md).
