---
title: Semantic search
description: Configure hybrid semantic search and understand embeddings, ranking, fallbacks, and operations.
last_edited: 2026-11-12
---

# Semantic search

By default `kata search` is lexical: it matches the words you type against issue
titles, bodies, and comments. That misses issues that describe the same problem
in different words, which is the case that matters most when an agent searches
before creating a duplicate. Semantic search adds a vector (embedding) leg so a query
like "auth redirect duplicates" can surface an issue titled "Login callback
double-submits on Safari" even though they share no keywords.

Semantic search is **opt-in**. Without an embedding endpoint, `kata search`
matches words. A local daemon with search by meaning off adds a stderr hint
pointing to Settings and the CLI setup command. `--quiet` and `--agent` suppress
it; stdout stays unchanged.

This page explains how to turn it on and how it behaves; for the exact config fields
see [Configuration](../reference/configuration.md#semantic-search), and for the
command flags see the [CLI reference](../reference/cli.md).

## How it works

When you configure an embedding endpoint, the daemon embeds each issue's title
and body, chunking long text instead of truncating it so long issues get full
coverage, and stores the resulting vectors in backend-native derived storage.
A search then runs two legs and fuses them:

- the **lexical leg**: the existing full-text search, unchanged; and
- the **vector leg**: embeds your query and finds issues whose vectors are
  closest to it.

Results are merged with reciprocal rank fusion, so an issue that ranks well in
either leg surfaces, and an issue that ranks well in both rises to the top.

Kata can run a shared local embeddings server or use an OpenAI-compatible
`/embeddings` endpoint, such as Ollama or a hosted provider.

## Enabling it

Open `kata ui`, click the gear in the top bar, and choose **Turn on** under **Search by meaning**. Settings applies to every project on the daemon. Turn on downloads what is missing and saves the setting. When offered, choose **Restart daemon** and confirm. Every project and client on the daemon disconnects briefly. At a stable browser address, a direct local session reconnects; token-login users log in again. If the address changes, run `kata ui` to reopen it. If the daemon uses a custom home, set `KATA_HOME` first. A local owner session or a daemon access administrator can use the action. Other viewers, including viewers connected through the daemon switcher, see the status.

If Settings cannot restart the daemon, run `kata daemon restart` on its host, or use its service manager. Reopen `kata ui` to see indexing progress and errors while it retries.

The built-in server choice is shared with supported apps on this computer. When another supported app enables it, a SQLite daemon starts indexing on its next start unless Kata already has a configured provider. PostgreSQL requires explicit setup in Kata. A saved built-in setting also starts with installed files. If those files are missing, SQLite accepts a shared choice to download them. Otherwise, run `kata daemon local-embeddings` to approve the download.

Kata records the shared choice after setup succeeds. If Kata cannot confirm the choice was saved, setup still succeeds and Kata logs the failure. Other apps may miss the choice if it wasn't saved. The CLI writes the warning to stderr. Settings writes it to the detached daemon's `daemon.log`, or to stderr when the daemon runs in the foreground.

PostgreSQL needs pgvector and Kata’s semantic-search tables before setup. Follow the [PostgreSQL setup guide](../operations/postgres.md) to prepare the database, then try Turn on again.

You can also enable it from the CLI:

```sh
kata daemon local-embeddings
```

For a home without embeddings configured, the command uses a running Ollama
without downloading anything. Otherwise, it shows the missing download size,
about 290 MB on a fresh install, and asks before downloading the built-in
server. The files are shared by all Kenn apps. Use `--yes`
for unattended setup. It restarts a running daemon, which embeds existing
issues in the background. A stopped daemon picks up the setting on its next
start. Watch progress with `kata health --json`.

Run the command again to switch a configured Ollama home to the built-in
server. It asks before starting the server, even when its files are installed.
With no running Ollama and the shared files installed, a fresh home enables
the built-in server without a prompt.

Switching from Ollama's 768-dimensional `nomic-embed-text` keeps your index and
federation recipe. Smaller widths are kept only with `request_dimensions = true`.
An external provider's nonempty `fingerprint_salt` forces a rebuild with the
built-in model's weights and is cleared during the switch.
Other recipes rebuild the index after a warning. If this daemon shares
embeddings, replicas need the same recipe before they can reuse its vectors.

The command keeps comments and preserves formatting outside the rewritten
settings. It checks that running daemons support restart before writing, then
waits for them to restart.
Run it again to retry a failed restart. Older Kata releases reject the `local`
config key; use the updated binary after enabling it.

A `base_url` from any source takes precedence over `local = true`. The setup
command refuses while `KATA_SEARCH_EMBEDDINGS_BASE_URL` is set; unset it and
rerun. A service with that variable still uses its configured server after
restart. API key settings are ignored while the built-in server is selected.
If a daemon refuses its restart, setup restores the configuration file.

### Using Ollama or another provider

1. Run an embeddings endpoint. For a fully local setup, install
   [Ollama](https://ollama.com) and pull an embedding model:

   ```sh
   ollama pull nomic-embed-text
   ```

2. Add a `[search.embeddings]` block to `<KATA_HOME>/config.toml`, or replace
   the existing block, setting `local = false`:

   ```toml
   [search.embeddings]
   base_url = "http://localhost:11434/v1"   # any OpenAI-compatible /embeddings
   model    = "nomic-embed-text"
   ```

   `base_url` and `model` are both required for an external provider. With no
   credential source configured, Kata sends requests without an `Authorization`
   header; local Ollama needs no placeholder key. For a provider that requires
   a key, use `api_key_file = "~/.config/kata/embedding.key"` with an owner-only
   file (`chmod 600` on Unix), or `api_key_env` supplied to the daemon's launch
   environment. Keep literal keys outside `config.toml`. Use an absolute file path
   or a path starting with `~/`; symlinks are accepted. See the
   [configuration reference](../reference/configuration.md#semantic-search).
   `api_key_env` works when the variable is set in the daemon environment.
   A daemon autostarted by a CLI command inherits that command’s environment;
   it does not pick up keys exported later in another shell. A key file is more
   reliable across autostart and service launches. See
   [first-run embeddings setup](../get-started/quickstart.md#optional-first-run-embeddings-setup)
   for secure hosted-provider examples and verification commands.
   Credential precedence is inline > file > env. An unavailable selected source
   does not fall back to another source. Run `kata daemon reload` after replacing
   the key file; model or endpoint changes require a restart. See
   [Configuration](../reference/configuration.md#semantic-search) for every
   field (`dims`, `batch_size`, `model_context_tokens`, `max_batch_tokens`,
   `timeout_seconds`, `fingerprint_salt`, `trust_private_network`). If the
   provider limits total input tokens per request, set both token fields to
   keep background embedding batches below that limit. Leave them unset for
   the existing count-only batching behavior.

3. Restart the daemon (or start it; `kata` will pick up the config). It begins
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
| lexical | `--lexical` | Full-text search only; today's behavior. |
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

Lexical search is always up to date the moment you write an issue: creating an
issue and immediately searching for it works exactly as before, which keeps the
agent search-before-create flow reliable.

The vector index is **eventually consistent**. A background reconciler embeds new
and edited issues a few seconds after they change (sooner against a fast local
endpoint, longer against a busy cloud API). A brand-new issue is not in the
vector leg until its first embedding lands, but it is still found lexically, so
nothing becomes unsearchable. An *edited* issue keeps serving its previous
vector (so the vector leg may rank it on the old text) until the reconciler
re-embeds it; the lexical leg already reflects the new text in the same results.
Either way the staleness is brief and bounded by reconciler lag.

You can watch the reconciler in `kata health --json` under `embeddings`:

- `configured`: whether an endpoint is set;
- `embedded`: how many issues are embedded at their current revision for the
  configured generation;
- `skipped`: how many issues were deliberately stamped without vectors after
  the configured model rejected their content;
- `backlog`: how many issues are waiting to be (re-)embedded; decreases as
  each issue is persisted and trends to 0;
- `rate_per_second` and `eta_seconds`: a smoothed measured rate and estimated
  time remaining; omitted until at least two positive progress samples exist;
- `started_at` and `last_progress_at`: when the current backfill began and
  when its most recent issue was persisted, useful for distinguishing slow
  progress from a stalled endpoint;
- `last_success_at`: when the reconciler last completed a successful cycle,
  including a cycle with no pending issues; query requests do not update it;
- `last_error_status`: the HTTP status of the most recent embedding-endpoint
  error response, if any. It is set only when the endpoint answered with an
  HTTP error; an unreachable endpoint (transport failure) leaves it unset.

## When the endpoint is unavailable

If the embedding endpoint is unreachable for a query, behavior depends on the
mode:

- **auto** degrades to lexical results and labels the response so the fallback is
  never silent: a `# mode=lexical degraded: …` note in human output, `degraded`
  and `degraded_reason` fields in `--json`, and a `degraded=<reason>` field in
  `--agent`.
- **explicit `--hybrid` / `--semantic`** do not degrade; they return an error
  (HTTP 503) so a caller that asked for semantic results knows it did not get
  them. They return 400 when embeddings are not configured at all.

Search accepts `--status open` or `--status closed` in every mode; omitting it
searches both statuses. Both retrieval legs filter before their result limits.
The vector leg checks the current issue status, so closing or reopening an
issue takes effect without waiting for embeddings to refresh. Comments remain
lexical-only matches.

Status- and label-filtered vector retrieval is bounded. If filtering exhausts that
candidate ceiling before filling the requested result limit, an auto search
returns the reachable results with `degraded` set. Explicit `--hybrid` and
`--semantic` searches return HTTP 503 instead, because they cannot guarantee a
complete result page. No degraded signal is emitted when the candidates fill
the requested limit.

A persistent endpoint problem shows up as a growing `backlog` and a stale
`last_success_at` in health; the reconciler backs off and retries. When the
endpoint answers with an HTTP error, `last_error_status` carries that status:
a misconfiguration (bad key, wrong model) is reported there rather than
silently looping. An endpoint that is unreachable outright (transport error,
no HTTP response) leaves `last_error_status` unset; the growing backlog and
stale `last_success_at` are the signal in that case.

## Changing the model

Each stored vector belongs to a generation keyed by a fingerprint of the
model, dimensionality, and text recipe it was produced under. If you switch
`model`, `dims`, `fingerprint_salt`, any literal document/query prefix or
suffix, or `request_dimensions`, kata builds a new generation and re-embeds
all issue text in the background. See the
[configuration reference](../reference/configuration.md#semantic-search)
for the exact controls. Empty affixes and `request_dimensions = false`
keep the existing generation. While that backfill runs, the vector leg is
**unavailable**: queries embedded under the new configuration cannot be scored against the old
generation's vectors, so auto mode serves labeled degraded lexical results
and explicit `--hybrid`/`--semantic` requests return 503. Lexical search is
unaffected throughout. When the new generation finishes filling, kata cuts
over to it automatically, semantic results resume, and the old generation's
storage is reclaimed. If you keep the same model name but its weights changed
underneath you (for example a re-pulled Ollama tag), bump `fingerprint_salt`
to force the same re-embed.

## Privacy and federation

Configuring an endpoint sends issue titles and bodies to it on every embed.
That is the consent boundary. For sensitive projects, prefer a local endpoint
(such as Ollama on loopback) so issue text never leaves the host. The embedding
API key is only ever sent to the configured `base_url` origin.

Embeddings are local derived state and **do not federate**: each daemon embeds
only what it stores, and no vectors are sent to or pulled from federated hubs.
SQLite keeps them in a sidecar database; PostgreSQL keeps them in pgvector
tables in its selected Kata schema when that optional extension is installed.
They are not included in JSONL
[backup/export](../operations/backup-restore.md); a restore or a storage-format
upgrade re-embeds from scratch rather than carrying vectors forward. Archives
exported by older kata versions that still contain
embedding records import cleanly: those records are skipped and the
reconciler rebuilds the vectors.

## Scope and limits

- Only issue **title and body** are embedded today; comments are searchable
  lexically but a paraphrase that lives only in a comment will not vector-match.
- PostgreSQL semantic search requires pgvector 0.7 or later, but core task and
  lexical-search storage does not. Its first implementation uses bounded exact
  cosine scans; SQLite uses sqlite-vec KNN. See
  [PostgreSQL operations](../operations/postgres.md) for setup.

For the design rationale and internals, see the
[semantic search design note](../design/semantic-search.md).

### Missing or rejected credentials

`kata health` reports `credential: missing`, `rejected`, or `ok` and the selected
`credential_source`, never the secret. A provider rejection includes a readable
`last_error` and `last_error_at`, cleared by the next successful provider call.
Human and agent health output warn on stderr; `--json` includes the diagnostic
fields without a stderr warning. Top-level health `ok` continues to describe
daemon/database health, so ordinary issue tracking and lexical search keep
working during an embedding credential problem.

If a selected key file or environment variable cannot supply a key, Kata sends
no provider requests and retains the backlog. The reason identifies the source
and the problem. Fix that source and run `kata daemon reload`; credential
resolution does not fall back to a lower-priority source.

With no credential source configured, requests omit `Authorization`. A provider
that responds with HTTP 401 then reports that an API key is required. HTTP 401
with a key reports a rejected key; HTTP 403 reports denied access, which does
not necessarily mean the key is invalid. Later queries and background embedding
attempts can retry these provider responses, and a successful request clears
the error.

Credential errors make `kata search --semantic` and `--hybrid` fail with a
readable validation error. Default search falls back to lexical results with
one `# mode=lexical degraded: …` header in human output and no extra stderr
warning. JSON and agent formats carry the degradation reason. An unconfigured
lexical-only daemon has no degradation label.
