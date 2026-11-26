---
title: Semantic search
description: Configure hybrid semantic search and understand embeddings, ranking, fallbacks, and operations.
last_edited: 2026-11-26
---

# Semantic search

By default, `kata search` is lexical and matches the words you type against
issue titles, bodies, and comments. Word matching misses issues that describe
the same problem in different terms, which matters most when an agent searches
before creating a duplicate. Semantic search adds a vector (embedding) leg so
a query like "auth redirect duplicates" can find an issue titled "Login
callback double-submits on Safari" even though they share no keywords.

Semantic search is **opt-in**. Without an embedding endpoint, `kata search`
matches words. When search by meaning is off, a local daemon prints a stderr
hint pointing to **Settings** and the CLI setup command. `--quiet` and
`--agent` suppress that hint, and stdout stays unchanged.

For exact configuration fields, see
[Configuration](../reference/configuration.md#semantic-search). For command
flags, see the [CLI reference](../reference/cli.md).

## How it works

When you configure an embedding endpoint, the daemon embeds each issue's title
and body and stores the resulting vectors in backend-native derived storage.
Long text is chunked instead of truncated, so long issues get full coverage. A
search then runs two legs and merges their results:

- **Lexical leg.** The existing full-text search, unchanged.
- **Vector leg.** Embeds your query and finds issues whose vectors are closest
  to it.

Kata merges both lists with reciprocal rank fusion, so an issue that ranks high
in either leg surfaces, and an issue that ranks high in both rises to the top.

Kata can run a shared local embeddings server, use an OpenAI-compatible
`/embeddings` endpoint, or connect to [Vertex AI](../reference/configuration.md#semantic-search).

## Enabling it

You can turn on search by meaning from the web UI or from the CLI.

To enable it in the web UI, open `kata ui`, open **Settings** at the bottom of the sidebar,
and choose **Turn on** under **Search by meaning**. **Settings** applies to
every project on the daemon. **Turn on** downloads any missing files and saves
the setting. When prompted, choose **Restart daemon** and confirm.

Every project and client on the daemon disconnects briefly during the restart.
At a stable browser address, a direct local session reconnects automatically,
while token-login users log in again. If the address changes, run `kata ui` to
reopen it; if the daemon uses a custom home, set `KATA_HOME` first. Only a
local owner session or a daemon access administrator can use this action. Other
viewers, including viewers connected through the daemon switcher, see the
status.

If **Settings** cannot restart the daemon, run `kata daemon restart` on its
host or restart it through its service manager. Reopen `kata ui` to watch
indexing progress and any retry errors.

If you use PostgreSQL, prepare pgvector and Kata's semantic-search tables first
by following the [PostgreSQL setup guide](../operations/postgres.md), then
choose **Turn on** again.

The built-in server choice is shared with supported apps on the same computer.
When another supported app enables it, a SQLite daemon starts indexing on its
next start unless Kata already has a configured provider. PostgreSQL requires
explicit setup in Kata. A saved built-in setting also starts with installed
files; if those files are missing, SQLite accepts a shared choice to download
them, and otherwise you can run `kata daemon local-embeddings` to approve the
download.

Kata records the shared choice after setup succeeds. If Kata cannot confirm
that the choice was saved, setup still succeeds and Kata logs the failure,
though other apps may miss the choice if it was not saved. The CLI writes that
warning to stderr. **Settings** writes it to the detached daemon's
`daemon.log`, or to stderr when the daemon runs in the foreground.

You can also turn on search by meaning from the CLI:

```sh
kata daemon local-embeddings
```

For a home without embeddings configured, the command uses a running Ollama
without downloading anything. Otherwise, it shows the missing download size,
about 295 MB on a fresh install, and asks before downloading the built-in
server. All Kenn apps share those files. Pass `--yes` for unattended setup. The
command restarts a running daemon, which embeds existing issues in the
background, while a stopped daemon picks up the setting on its next start. You
can watch progress with `kata health --json`.

Run the command again to switch a configured Ollama home to the built-in
server. It asks before starting the server, even when the shared files are
already installed. If no Ollama is running and the shared files are installed,
a fresh home enables the built-in server without a prompt.

Switching from Ollama's 768-dimensional `nomic-embed-text` keeps your index and
federation recipe. Smaller widths are kept only with
`request_dimensions = true`. If an external provider had a nonempty
`fingerprint_salt`, switching forces a rebuild with the built-in model's
weights and clears `fingerprint_salt`. Other recipes rebuild the index after a
warning. If this daemon shares embeddings, replicas need the same recipe before
they can reuse its vectors.

The setup command preserves comments and formatting outside the rewritten
settings. It checks that running daemons support restart before writing, then
waits for them to restart. If a restart fails, run the command again to retry;
if a daemon refuses its restart, setup restores the configuration file. Older Kata releases reject the `local` config key. Use the updated binary after
enabling it.

A `base_url` from any source takes precedence over `local = true`. The setup
command refuses to run while `KATA_SEARCH_EMBEDDINGS_BASE_URL` is set; unset
that variable and rerun. A service with that variable set still uses its
configured server after restart. Kata ignores API key settings while the
built-in server is selected.

### Use Ollama or another provider

1. Start an embeddings endpoint. For a fully local setup, install
   [Ollama](https://ollama.com) and pull an embedding model:

   ```sh
   ollama pull nomic-embed-text
   ```

2. Add a `[search.embeddings]` block to `<KATA_HOME>/config.toml`, or replace
   the existing block, and set `local = false` in it:

   ```toml
   [search.embeddings]
   base_url = "http://localhost:11434/v1"   # any OpenAI-compatible /embeddings
   model    = "nomic-embed-text"
   ```

   An external provider requires both `base_url` and `model`. When no
   credential source is configured, Kata sends requests without an
   `Authorization` header. Local Ollama needs no placeholder key.

   For a provider that requires a key, use
   `api_key_file = "~/.config/kata/embedding.key"` with an owner-only file
   (`chmod 600` on Unix), or set `api_key_env` to the name of an environment
   variable supplied to the daemon's launch environment. Keep literal keys out
   of `config.toml`. Use an absolute file path or a path starting with `~/`;
   symlinks are accepted. See the
   [configuration reference](../reference/configuration.md#semantic-search).
   `api_key_env` works only when that variable is set in the daemon
   environment. A daemon autostarted by a CLI command inherits that command's
   environment. It does not pick up keys exported later in another shell. A
   key file is more reliable across autostart and service launches. See
   [first-run embeddings setup](../get-started/quickstart.md#optional-first-run-embeddings-setup)
   for secure hosted-provider examples and verification commands.

   Credential precedence is inline > file > env, and an unavailable selected
   source does not fall back to another source. After replacing a key file, run
   `kata daemon reload`; model or endpoint changes require a restart. See
   [Configuration](../reference/configuration.md#semantic-search) for every
   field (`dims`, `batch_size`, `model_context_tokens`, `max_batch_tokens`,
   `timeout_seconds`, `fingerprint_salt`, `trust_private_network`). If your
   provider limits total input tokens per request, set both token fields to
   keep background embedding batches below that limit; leave them unset for
   count-only batching.

3. Restart or start the daemon. Kata picks up the configuration and begins
   embedding existing issues in the background right away.

4. Confirm that the endpoint is live and watch the backfill drain by checking
   the `embeddings` object in the JSON health response:

   ```sh
   kata health --json   # see the "embeddings" object: backlog, last_success_at
   ```

After setup finishes, Kata embeds new and edited issues automatically.

## Search modes

`kata search` accepts three mutually exclusive mode flags. With none of them,
it runs in **auto** mode:

| Mode | Flag | Behavior |
| --- | --- | --- |
| auto (default) | *(none)* | Hybrid when embeddings are configured, lexical otherwise. |
| lexical | `--lexical` | Full-text search only; today's behavior. |
| hybrid | `--hybrid` | Fuse the lexical and vector legs. |
| semantic | `--semantic` | Vector results only. |

Auto mode is the right default for almost everything, including agents. It
improves recall when embeddings are on and stays unchanged when they are off.
Use `--lexical` for exact keyword or identifier lookups, and use `--semantic`
when you want pure concept matching regardless of wording.

Every response reports the effective mode. In `--json` and `--agent` output, it
appears in the `mode` field (`lexical`, `hybrid`, or `semantic`); see the
[agent output reference](../reference/agent-output.md) for the exact shape.

## Search by meaning in the TUI

`kata tui`'s `/` search filters the rows already loaded, by title and ID. To
find an issue by what it is about, press `/` in a project, then `ctrl+t` to ask
the daemon instead. The daemon ranks the project's issues in auto mode and
the TUI takes its best 50 hits, flat and in the daemon's order. It shows only
the hits among the issues it has loaded (up to 2,000 per project), and the
other list filters (status, labels, owner, date) narrow those. A note beside the
search line says whether the daemon ranked by words and meaning or by words
only, and, in a project with more issues than the TUI loads, how many of the 50
hits lie beyond them.

The choice lasts until you quit. Press `ctrl+t` again to filter the loaded rows
by text. Results come after you stop typing; a blank query shows the normal
list. `enter` keeps the results while the query stays; `esc` restores
the previous list, and `c` clears the filters and the results. Daemon search
needs one project, so in the all-projects view `ctrl+t` does nothing. It does
not set up embeddings: without them the daemon ranks by words, and the note
says so.

## Lexical and semantic freshness

Lexical search is always up to date the moment you write an issue. Creating an issue
and searching for it right away works as before, which keeps the agent
search-before-create flow reliable.

The vector index is **eventually consistent**. A background reconciler embeds
new and edited issues a few seconds after they change, faster against a fast local
endpoint and slower against a busy cloud API. A brand-new issue is missing from
the vector leg until its first embedding lands, though lexical search still
finds it. An edited issue keeps serving its previous vector until the
reconciler re-embeds it, so the vector leg can rank it on the old text while
the lexical leg already matches the new text in the same results. In both
cases, staleness is brief and bounded by reconciler lag.

You can monitor the reconciler in `kata health --json` under `embeddings`:

- `configured`. Whether an endpoint is set.
- `embedded`. How many issues are embedded at their current revision for the
  configured generation.
- `skipped`. How many issues were deliberately stamped without vectors after
  the configured model rejected their content.
- `backlog`. How many issues are waiting to be embedded or re-embedded; this
  decreases as each issue is persisted and trends toward 0.
- `rate_per_second` and `eta_seconds`. A smoothed measured rate and estimated
  time remaining, omitted until at least two positive progress samples exist.
- `started_at` and `last_progress_at`. When the current backfill began and when
  its most recent issue was persisted, which helps distinguish slow progress
  from a stalled endpoint.
- `last_success_at`. When the reconciler last completed a successful cycle,
  including a cycle with no pending issues; query requests do not update it.
- `last_error_status`. The HTTP status of the most recent embedding-endpoint
  error response, if any. It is set only when the endpoint answered with an
  HTTP error; an unreachable endpoint leaves it unset.

## When the endpoint is unavailable

If the embedding endpoint is unreachable during a query, behavior depends on
the search mode:

- **Auto mode.** Degrades to lexical results and labels the response so the
  fallback is never silent. Human output prints a `# mode=lexical degraded: …`
  note, `--json` includes `degraded` and `degraded_reason`, and `--agent`
  includes `degraded=<reason>`.
- **Explicit `--hybrid` or `--semantic`.** Does not degrade and returns HTTP
  503 so callers asking for semantic results know they did not get them. When
  embeddings are not configured at all, these modes return HTTP 400.

Search accepts `--status open` or `--status closed` in every mode, and omitting
the flag searches both statuses. Both retrieval legs filter before applying
their result limits. Because the vector leg checks the current issue status,
closing or reopening an issue takes effect without waiting for embeddings to
refresh. Comments remain lexical-only matches.

Status- and label-filtered vector retrieval is bounded. If filtering exhausts
that candidate ceiling before filling the requested result limit, an auto
search returns the reachable results with `degraded` set. Explicit `--hybrid`
and `--semantic` searches return HTTP 503 instead, because they cannot
guarantee a complete result page. When the candidates fill the requested limit,
Kata emits no degraded signal.

A persistent endpoint problem shows up in health output as a growing `backlog`
and a stale `last_success_at`, while the reconciler backs off and retries. When
the endpoint answers with an HTTP error, `last_error_status` records that
status. Misconfigurations such as a bad key or wrong model surface there
instead of looping silently. When the endpoint is unreachable outright and
returns no HTTP response, `last_error_status` stays unset, and the growing
`backlog` and stale `last_success_at` are your signals.

## Change the model

Each stored vector belongs to a generation keyed by a fingerprint of the model,
dimensionality, and text recipe used to produce it. If you change `model`,
`dims`, `fingerprint_salt`, any literal document or query prefix or suffix, or
`request_dimensions`, Kata builds a new generation and re-embeds all issue text
in the background. See the
[configuration reference](../reference/configuration.md#semantic-search) for
the exact controls. Empty affixes and `request_dimensions = false` keep the
existing generation.

While that backfill runs, the vector leg is **unavailable**. Queries embedded
under the new configuration cannot be scored against the old generation's
vectors, so auto mode serves labeled degraded lexical results and explicit
`--hybrid` and `--semantic` requests return HTTP 503. Lexical search keeps
working throughout. Once the new generation finishes filling, Kata cuts over to
it automatically, semantic results resume, and Kata reclaims the old
generation's storage. If you keep the same model name but its weights change
underneath you, for example after re-pulling an Ollama tag, bump
`fingerprint_salt` to force the same re-embed.

## Privacy and federation

Configuring an embedding endpoint sends issue titles and bodies to that
endpoint on every embed. That is the consent boundary. For sensitive projects,
prefer a local endpoint such as Ollama on loopback so issue text never leaves
the host. Kata sends the embedding API key only to the configured `base_url`
origin.

The search index is local derived state, and it is not copied to a direct
spoke. A direct spoke searches shared projects by meaning through its hub and
embeds only its local projects. The spoke sends the query text to the hub,
which sends it to its configured embedding provider. Each search costs the hub
one query embedding. If the hub is unavailable or too old, automatic search
returns word matches with a reason, and explicit semantic or hybrid search
returns an error. Relays can reuse vectors from one selected producer when the
recipe matches, and only that producer generates missing shared vectors; see
[Sharing embedding vectors](../operations/federation.md#sharing-embedding-vectors).
An all-project MCP semantic or hybrid search returns available results and
names local projects without embeddings.

SQLite stores the vectors in a sidecar database, and PostgreSQL stores them in
pgvector tables in its selected Kata schema when that optional extension is
installed. Both JSONL [backup/export](../operations/backup-restore.md) forms,
full backups and project exports, keep complete portable embedding artifacts,
and a project export includes only that project's artifacts. A restore
validates those artifacts and reuses an artifact's vectors without requesting
new embeddings only when the artifact's input and recipe match the daemon's
current embedding configuration. The daemon re-embeds issues without a matching
artifact. Legacy index embeddings are still excluded. Archives exported by
older kata versions that still contain embedding records import cleanly; Kata
skips those records and the reconciler rebuilds the vectors.

## Scope and limits

- **Title and body only.** Kata embeds only issue titles and bodies today.
  Comments remain searchable lexically, and a paraphrase that lives only in a
  comment does not vector-match.
- **PostgreSQL requirements.** PostgreSQL semantic search requires pgvector 0.7
  or later, while core task and lexical-search storage do not. Its first
  implementation uses bounded exact cosine scans, and SQLite uses sqlite-vec
  KNN. See [PostgreSQL operations](../operations/postgres.md) for setup.

For the design rationale and internals, see the
[semantic search design note](../design/semantic-search.md).

### Missing or rejected credentials

`kata health` reports `credential: missing`, `rejected`, or `ok` along with the
selected `credential_source`, and never prints the secret. When a provider
rejects a request, health output includes a readable `last_error` and
`last_error_at`, which clear on the next successful provider call. Human and
agent health output warn on stderr, while `--json` includes the diagnostic
fields without a stderr warning. Top-level health `ok` continues to describe
daemon and database health, so ordinary issue tracking and lexical search keep
working during an embedding credential problem.

If a selected key file or environment variable cannot supply a key, Kata sends
no provider requests and retains the backlog. The error reason identifies the
source and the problem. Fix that source and run `kata daemon reload`;
credential resolution does not fall back to a lower-priority source.

When no credential source is configured, requests omit `Authorization`. If a
provider responds with HTTP 401 in that case, Kata reports that an API key is
required. HTTP 401 with a key reports a rejected key, and HTTP 403 reports
denied access, which does not necessarily mean the key is invalid. Later
queries and background embedding attempts can retry after these provider
responses, and a successful request clears the error.

Credential errors cause `kata search --semantic` and `--hybrid` to fail with a
readable validation error. Default search falls back to lexical results with
one `# mode=lexical degraded: …` header in human output and no extra stderr
warning. JSON and agent formats carry the degradation reason. An unconfigured
lexical-only daemon has no degradation label.
