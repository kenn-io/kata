---
title: HTTP API schema
description: Generate clients and inspect Kata's versioned OpenAPI schema, compatibility rules, and authentication.
last_edited: 2026-11-27
---

# HTTP API schema

The daemon exposes an HTTP API that the CLI, TUI, and remote clients use. Kata
publishes its shape as an OpenAPI 3.1 document so out-of-process clients can
generate typed clients instead of copying wire structs by hand.

To find the endpoint and transport with the same selection rules as the CLI, use
[`kata daemon locate`](daemon-discovery.md). If you write in Go, you can use the
generated [Go client](go-client.md), which includes discovery. For job and
workflow resources, see [Cron](cron.md).

## Search configuration header

When the answering daemon has an embeddings provider, search responses include
`X-Kata-Embeddings-Configured: true`, even if the index is empty or read-only
mode forces lexical search. When no provider is configured, the header is
`false`. The response body does not change.

## Project lookup by name

Call `GET /api/v1/projects/resolve?name=...` to look up a project by its exact
name. You need read access to that project, and this route does not resolve
aliases or filesystem paths.

## Local embeddings setup

`GET /api/v1/embeddings/local` returns the configured provider, missing download
bytes, Ollama availability, setup job, indexing progress, `can_change`, and
`can_restart`. While the daemon keeps retrying after an indexing error,
`indexing.failed` reports that error. Only daemon owners can see aggregate
indexing counts, time estimates, and detailed setup errors; other viewers get
progress and failure state without those details. Issue-scoped credentials can
read this status.

To start setup, call `POST /api/v1/embeddings/local`. It returns `202` with the
same status payload, runs the same download and config checks as the CLI, saves
the setting, and reports the `restart` job state until the daemon restarts. When
`can_restart` is true, a daemon administrator can call
`POST /api/v1/embeddings/local/restart`. That route returns `202` with an empty
body, keeps the job state at `restart`, sets `restart_in_progress`, disables
`can_restart`, and briefly disconnects all projects and clients. It returns
`404` if setup is unavailable, `403` if the caller lacks daemon administration,
and `409` if restart is unavailable or setup is not ready. You cannot trigger a
restart through the daemon switcher. For other daemons, run
`kata daemon restart` on the host or use your service manager. Afterward, if the
address changed, reopen `kata ui`, or sign in again to check indexing progress.

Setup is available only when no embeddings provider is configured and no setup
job is active. You need a local owner browser session, a bootstrap token, or a
static daemon token, along with a writable browser session manager when one is
present. The route returns `403` for other viewers and through the daemon
switcher proxy, `404` when setup is unavailable, and `409` while setup is
already running. Status reads are allowed through the switcher proxy.

## Getting the schema

The schema is committed at `api/openapi.yaml`, and you can regenerate it with
the `kata` binary:

```sh
kata openapi > openapi.yaml
```

`kata openapi` builds the document in-process from the daemon's route
definitions, so you do not need a running daemon or a database. The runtime
`/openapi.json` route stays disabled; the committed file and this command are
the supported ways to get the schema.

## Errors

Daemon API errors return an `error` object with a stable `code` and a readable
`message`. An optional `hint` suggests a next step, and optional `data` holds
structured details. When a `kata` command can trigger the error, the hint names
CLI commands and flags, such as `--force`, rather than request fields. Hints for
errors that only direct API calls can trigger may name request fields. In
scripts, branch on `code` instead of matching `message` or `hint`.

HTTP 429 with code `server_busy` means an upstream hub refused the request
because it is busy. The message names the hub. A `Retry-After` header, when
present, states when to retry.

```json
{"error":{"code":"project_not_initialized","message":"no .kata.toml ancestor and no git ancestor","hint":"run \"kata init --project <name>\" in the workspace root, or pass --project <name>"}}
```

`project_not_initialized` includes the looked-up name in `data`, only when the
message names it:

| Request | `data` |
| --- | --- |
| An alias that points at an archived project | `alias` |
| Only an alias, not registered (the CLI in a git workspace without `.kata.toml`) | `alias` |
| A project name that is not registered, from `--project`, `.kata.toml`, or a `name:` project selector, with or without an alias | `project` |
| A `start_path` whose `.kata.toml` names an unregistered project | `project` |
| A `start_path` in a git workspace whose alias is not registered | none |
| No `.kata.toml` and no git ancestor | none |

An unregistered Git alias whose derived project name is invalid returns HTTP
400 with error code `validation`.

```json
{"error":{"code":"project_not_initialized","message":"alias github.com/example/spoke-project is not registered","hint":"run \"kata init\": it attaches this workspace to the active project \"spoke-project\", or creates it if none exists","data":{"alias":"github.com/example/spoke-project"}}}
```

The CLI keeps the hint in human, agent, and JSON errors. See
[agent output](agent-output.md#errors) for the stderr format.

On routes that take `{project_id}` in the path or as a `project_id` query
parameter, a missing ID returns `404 not_found`. An archived project that the
caller may read returns `404 project_not_found` with a hint naming
`kata projects restore <name>`. A `project_not_found` response with a hint means
the project is archived; check whether a hint is present rather than parsing its
text.

```json
{"error":{"code":"project_not_found","message":"project not found","hint":"project docs is archived; run 'kata projects restore docs' to bring it back"}}
```

You also get `project_not_found` without a hint in two cases:

- An issue-scoped credential requests an archived project.
- A caller without a principal on a lease, claim, or cron-job write route (on the
  local socket or using the daemon's own `[auth].token` over TCP) requests a
  missing ID, because those routes authenticate in the handler.

If the project access policy denies a caller, the daemon returns
`404 not_found`, the same as for a missing ID. If a host access controller
denies a caller, the caller gets `not_found` on most routes and never gets the
hint on any route.

On `/federation/` routes that a peer calls with its enrollment token, the daemon
checks the token against the project first, so a missing or archived project
returns `403 auth_invalid` without the hint. `federation/relay:disconnect` is an
exception: whenever the presented relay token matches, it revokes that token
even in an archived project, and otherwise it returns `404 not_found`.

## Editing an issue safely

Pass the edit precondition in the request body rather than in `If-Match`,
because `If-Match` checks the issue `revision`, which a title or body edit does
not change. On `PATCH /projects/{project_id}/issues/{ref}`, send the
`content_revision` you read as `expected_content_revision`. The daemon compares
it against the stored value in the same transaction as the write, so if someone
edited the title or body in between, the request returns HTTP 412 with code
`revision_conflict` and writes nothing, including any priority, owner, or link
changes in the same payload. To make an unconditional edit, omit
`expected_content_revision`. A new issue starts at `content_revision` `0`, so
send `0` instead of omitting the field.

## Recurrence timezones

When you create a recurrence with
`POST /api/v1/projects/{project_id}/recurrences`, use an IANA time zone such as
`America/New_York` or `UTC`:

```json
{
  "actor": "user-a",
  "rrule": "FREQ=WEEKLY",
  "dtstart": "2026-11-02",
  "timezone": "America/New_York",
  "template": {"title": "Weekly review"}
}
```

Creation, including attachment through `initial_issue_ref`, rejects the exact
value `Local` with HTTP 400 and error code `validation`, and changing another
timezone to `Local` with PATCH returns the same error. `Local` depends on the
daemon host, so moving the daemon could change the schedule's timezone.

Existing recurrences stored with `Local` continue to run and remain readable.
When you edit their schedule or template with PATCH, you may omit `timezone` or
echo `Local`. To make an existing schedule portable, update its timezone to an
IANA zone or `UTC` with PATCH. Every PATCH requires the current
`If-Match: "rev-N"` revision; stale requests return HTTP 412 with code
`revision_conflict`, including stale `Local` values. Other loadable timezone
names and aliases keep their existing behavior.

## What the schema is

The schema is a **per-release snapshot** of the daemon's current HTTP API. It
describes the routes and wire types as they exist at that commit and is useful
for client generation and review.

It is **not** a promise of a forever-stable API. Treat it as the contract for
the daemon version you generated it against, not a guarantee that a future
daemon accepts the same calls.

## Detecting the API version

The schema stores its version in `info.version` (`APISchemaVersion`), and
[`GET /api/v1/health`](#health-diagnostics) reports the same value at runtime as
`api_schema_version`:

```json
{
  "ok": true,
  "schema_version": 29,
  "api_schema_version": "0.23.0",
  "version": "1.4.2",
  "uptime": "5m0s",
  "idle_shutdown": {
    "timeout": "15m0s",
    "state": "armed",
    "deadline": "2026-08-17T17:15:00Z"
  }
}
```

`idle_shutdown` appears only when the process is an implicitly started,
owner-local daemon with an effective timeout. Its `state` is one of:

| State | Meaning |
| --- | --- |
| `armed` | No foreground request is active; `deadline` is the next eligible shutdown time. |
| `foreground` | Client work is active and the idle deadline is suspended. |
| `blocked` | The prior deadline elapsed, but already-admitted finite background work is still draining. |
| `stopping` | New work is no longer admitted and root shutdown has started. |

`deadline` is present for `armed` and `blocked`, and absent while foreground
work suspends the deadline or shutdown is already committed. Monitoring reads
of health, ping, and instance state do not reset the deadline.

The response includes three version fields that answer different questions:

| Field | Meaning |
| --- | --- |
| `api_schema_version` | The HTTP API contract version; match this against the schema you generated your client from. |
| `schema_version` | The database/storage schema version (`meta.schema_version`), an internal storage concern. |
| `version` | The daemon build version. |

You can read `api_schema_version` before using a versioned request feature.
Because versions increase in semantic-version order, your client can compare the
daemon's version against the first version that added the feature. That check
confirms the request field exists, not that every part of a generated client
matches a different schema version. Generate clients from the schema of your
target release.

The first-party CLI checks `api_schema_version` before sending requests where an
older daemon could ignore a filter and return unfiltered rows. Status-filtered
`search` requires API `0.20.0`, label-filtered `search` and filtered
`ready --all` require API `0.8.0`, and filtered `list --all` requires API
`0.9.0`. If the daemon is too old, the CLI stops before running the filtered
query and tells the operator to upgrade. `kata mcp serve` runs the same check
once at startup and requires API `0.11.0`, because its relationship tools always
send the pinned-target fields and its close-audit paging relies on `event_id`.
Status-filtered `kata.search` calls also check for API `0.20.0` before project
fanout. Paginated `kata.list` and explicit single-project lexical `kata.search`
require API `0.27.0`, and the CLI checks that same version before using new list
options or explicit lexical pagination.

Close requests may include an explicit `transcript`. See the
[configuration reference](configuration.md#closing-session-transcripts) for
agent names, session IDs, and daemon API compatibility:

```json
{
  "actor": "agent-one",
  "reason": "done",
  "message": "Implemented the requested behavior and ran the focused tests.",
  "evidence": [{"type": "test", "command": "go test ./internal/example"}],
  "transcript": {
    "agent": "pi",
    "session_id": "00000000-0000-4000-8000-000000000001",
    "url": "https://agentsview.example/sessions/pi/00000000-0000-4000-8000-000000000001"
  }
}
```

`agent` is required and names the coding agent. `session_id` follows the session
ID rules in the configuration reference, and `url` is an optional absolute
HTTP(S) URL without credentials, a query string, or a fragment. For namespaced
imported archives, supply a verified locator. The daemon stores `transcript`
separately from evidence in the close event and does not fetch or verify the
chat. `agent` and `session_id` are part of retry identity, while `url` is not,
and the object stays intact in scoped responses and history. Automatic capture
is a [CLI client preference](configuration.md#closing-session-transcripts), not
daemon-side session discovery; see that reference for AgentsView compatibility.

Starting in API `0.52.0`, close requests accept `transcript_selection` with
`project_id` and a required `record` flag. A recorded selection requires a
candidate `transcript`, while an opt-out can omit it. When you supply a
candidate, its agent and session identify keyed retries even when `record` is
false. A positive `project_id` binds the policy to that project, and if the
issue moved to another project, the daemon returns `issue_moved` before a fresh
close commits. For an unknown target, use project ID zero and omit the candidate
URL. The selection choice and URL do not change retry identity.

Guarded close requests check compatibility in the request itself rather than
with a separate health probe. Whenever you send `Idempotency-Key` or `If-Match`,
also send `retry_protocol: "close-v1"`. Current daemons require that marker, and
older daemons reject the unknown field before the close can run.

Starting in API `0.69.0`, a successful close can include
`parent_ready`, an issue reference for an open parent with no open children.
The field is omitted for retries, dry runs, unchanged closes, inaccessible
parents, and when the advisory lookup fails.

`api_schema_version` is **optional in the schema** even though current daemons
always send it. A version-detection field has to survive version skew, so a
client generated from a newer schema can still parse responses from an older
daemon that predates the field. Treat an **absent or empty**
`api_schema_version` as a daemon older than this field, not as a parse error.
Embedding hosts using `@kenn-io/kata-ui` must treat that state as incompatible
and refuse to render issue detail.

API contract versions advance independently of Kata release versions:

- Kata 0.24.0 includes API `0.71.0` (the changes from `0.44.0` through `0.71.0`
  below). Direct-spoke hub semantic search requires `0.55.0`, held federation
  polls require `0.67.0`, atomic label swaps require `0.70.0`, and multiplexed
  federation event watches require `0.71.0`.
- Kata 0.23.0 includes API `0.43.0`. Local embeddings setup requires `0.42.0`,
  and MCP Events require `0.43.0`.
- Kata 0.22.0 includes API `0.41.0`, and Kata 0.21.0 includes API `0.40.0` (the
  changes from `0.32.0` through `0.40.0` below).
- Kata 0.20.0 includes API `0.31.0`. Bridge preflight checks for cross-project
  issue links require hub API `0.31.0`, and acknowledging a request in the TUI
  Attention inbox requires `0.13.0`.
- Kata 0.19.0 includes API `0.28.0` (the changes from `0.22.0` through `0.28.0`
  below). Timed claims require API `0.22.0`, move previews require `0.23.0`,
  close session references require `0.26.0`, paginated issue lists require
  `0.27.0`, and comment replies require `0.28.0`.
- Kata 0.18.0 includes the changes from `0.18.0` through `0.21.0`. Teammate
  comments require API `0.18.0`, issue-scoped credentials use `0.19.0`,
  status-filtered search requires `0.20.0`, and oldest-first lists require
  `0.21.0`.
- Kata 0.17.0 includes API changes `0.15.0` through `0.17.0`. Custom clients
  upgrading from earlier releases must send array fields as `[]` (or omit
  optional fields), use exact request field names, and accept empty response
  collections as `[]` or `{}`.

### Issue event history

HTTP API `0.32.0` adds an `issue` query parameter to
`GET /api/v1/projects/{project_id}/events?issue={ref}` to return one issue's
history, filtered before the page limit is applied. Pass a short ID, a qualified
reference, or a full UID within the URL project as `ref`, and page through
results with `after_id` and `limit`. If you omit `issue`, the route polls the
project feed instead. Filtered history never returns `reset_required`: purging
another issue removes only events that name it, and the daemon omits events that
name an issue the caller cannot see. After a move, history includes events from
earlier projects only when the caller can access those projects. Scoped
credentials receive the same authorized history projection as issue detail.

Callers that the restore action would accept can read a soft-deleted issue's
history, including its `issue.soft_deleted` event. Pass the actor you would
restore as in the `actor` query parameter; an authenticated principal's own
actor takes precedence, as it does for restore. The same attributed write and
claim checks apply, so on a federated project only the claim holder can read the
history while another actor's claim is live. Every refusal (any API error below
500 other than a hub error), including an anonymous `--insecure-readonly`
caller, returns `issue_not_found`; 5xx errors, `federation_offline`, and
`hub_claim_failed` keep their status. When the daemon is embedded under a host
access controller, a soft-deleted issue's history always returns
`issue_not_found`. The daemon uses `actor` only with `issue=` on a soft-deleted
issue and ignores it otherwise.

If you need close evidence, pass `include_effective_close_event_uid=true`. When
a visible native close currently supplies the issue's closed status, the
response includes `effective_close_event_uid`. The daemon chooses that event
using the same federation clock order as issue projection, so scoped clients do
not need the redacted origin identity or status transitions to pick evidence. If
no visible effective close exists, the field is omitted.

Every successful filtered response, including empty pages, returns the resolved
issue UID in `X-Kata-Events-Issue-UID`. Require this header on every page: older
daemons can ignore the `issue` query and return unfiltered project events. When
the header is missing or changes between pages, the TUI stops pagination and
prompts you to upgrade the daemon.

### Version history

This table highlights notable changes. Marker history starts at `0.44.0`.

| Version | Change |
| --- | --- |
| `0.72.0` | Forwarded hub requests report busy refusals as `429 server_busy` and preserve `Retry-After` hints. |
| `0.71.0` | Federation event watches hold one request for many projects; event polls advertise `X-Kata-Federation-Watch-Max`. |
| `0.70.0` | Added `PATCH /api/v1/projects/{project_id}/issues/{ref}/labels`, which applies `add` and `remove` label lists in one transaction and answers with the issue's resulting `labels`. |
| `0.69.0` | Close responses can include a `parent_ready` issue reference. |
| `0.68.0` | Rejects priority labels (`p0` through `p4` and `priority:` prefixes) on label add, issue create, and recurrence template writes. |
| `0.67.0` | Federation event polls accept `wait` and advertise `X-Kata-Federation-Poll-Wait-Max`. |
| `0.66.0` | Federation search accepts `auto` and reports its effective mode, matched fields, and any fallback reason. |
| `0.65.0` | Exposes the effective close event in selected task snapshots. |
| `0.64.0` | Routes shared-project attachments to their Docbank origin. Attachment references can include `origin`; the UI attachment capability is enabled when the daemon has a local Docbank or any enabled spoke binding. A disabled spoke rejects uploads and links with `503 federation_offline`, even when it has a local vault. Federation peers advertise `docbank_origin_v1` before receiving origin references. |
| `0.63.0` | Browser issue reference resolution accepts a project name or UID and qualified references without a project ID. |
| `0.62.0` | Federation peers advertise `relay_cross_links_v1` in `X-Kata-Event-Features`. A common root can link issues across projects with a shared relay audience. Relays check access to each referenced project they can resolve before releasing events or checkpoints, and pause for older peers with `409 unsupported_event_features` at crossing links or links with unresolved endpoints. An unresolved endpoint may belong to another project. |
| `0.61.0` | Federation status reports the runner retry schedule for a refused or unavailable spoke binding. |
| `0.60.0` | Enrollment join instructions use the signing hub's external URL and include signing key placeholders. |
| `0.59.0` | Schedule and deadline civil dates and times accept an explicit `[Zone]` suffix. |
| `0.58.0` | `GET /api/v1/projects/{project_id}/events` accepts `actor` so callers who may restore a soft-deleted issue can read its history. |
| `0.57.0` | Cron run rows carry `job_name`, `workflow_name` and `issue_short_id`, resolved when read; deleted jobs, workflows and issues still name their runs. |
| `0.56.0` | Direct-spoke ingest returns `403 federation_project_not_enrolled` instead of `403 auth_invalid` when pushed work touches an issue whose current project has no matching push enrollment for the spoke and actor. The message names the issue but not the project, which the spoke has not joined. |
| `0.55.0` | `GET /projects/{project_id}/federation/search` lets a direct spoke ask its hub to answer a search by meaning over a shared project. |
| `0.54.0` | `GET /api/v1/docbank/probe` makes one read-only request to Docbank and reports whether it answered and accepted the daemon's key. |
| `0.53.0` | Issue responses carry `content_revision`, which moves when a title or body edit changes the text. `PATCH /projects/{project_id}/issues/{ref}` accepts `expected_content_revision`; a different current value returns 412 `revision_conflict` and writes nothing. Older daemons reject the unknown field. |
| `0.52.0` | Close requests accept project-bound transcript selection and retain candidate session identity for keyed retries. |
| `0.51.0` | Signed federation requests return `503 signing_warming` with `Retry-After: 5` while replay protection warms up. |
| `0.50.0` | A hub that requires signing and the restricted ingress answer requests without signature headers with `428 hub_signing_required` instead of `401 signature_invalid`. A request with only a digest follows the unsigned path, including when signing is optional. |
| `0.49.0` | `GET /api/v1/projects/resolve` resolves a project by name for readers. Project federation status accepts `omit_errors=true` to withhold diagnostic error text. |
| `0.48.0` | Project federation status accepts `compact=true` to omit relay checkpoint capacity measurement. |
| `0.47.0` | Attachment uploads accept `reuse_existing`. Reuse on uploads and links returns `attachment_invalid` when the daemon cannot read an existing reference. |
| `0.46.0` | Federation ingest responses add optional `superseded_event_uids`, the adoption snapshots the hub did not store. |
| `0.45.0` | Adds `POST /api/v1/embeddings/local/restart` to restart the daemon after local embeddings setup. Older daemons do not serve this route. |
| `0.44.0` | Attachment routes return `503 docbank_key_unavailable` instead of `502 docbank_unauthorized` when the daemon cannot resolve its own Docbank key. Health diagnostics add a `docbank` block. |
| `0.43.0` | Adds the daemon-owned MCP Events routes `POST /api/v1/mcp-events/list`, `POST /api/v1/mcp-events/subscribe`, `POST /api/v1/mcp-events/unsubscribe`, and `GET /api/v1/mcp-events/subscriptions` (status). Subscribe and unsubscribe arguments name either a non-empty `project_uids` list or `all_projects: true`. Failures return an `mcp_events` error whose `data` carries `mcp_code` and `mcp_data`. Older daemons do not serve these routes. Both issue list routes accept `compact=true`. |
| `0.42.0` | Adds daemon-wide local embeddings setup and status at `GET` and `POST /embeddings/local`, and the `X-Kata-Embeddings-Configured` search response header. |
| `0.41.0` | Adds `sort=priority` to both ready routes. `kata next` and MCP `kata.next` request one priority-ordered row; older daemons retain the full-queue fetch and client-side selection. |
| `0.40.0` | Issue history accepts `include_effective_close_event_uid=true` and returns the visible native close event that currently supplies the issue's closed status. |
| `0.39.0` | `POST /projects/{project_id}/issues/{ref}/attachments/links` accepts `reuse_existing=true` to reuse the smallest existing attachment reference for the resolved Docbank version, creating one if none exists. Omission keeps creating a new reference. |
| `0.38.0` | Publishes `POST /projects/{project_id}/issues/{ref}/attachments/links`, which saves a reference to an existing Docbank file version without uploading its bytes. `kata docbank attachments link` refuses older daemons. |
| `0.37.0` | `POST /projects/{project_id}/recurrences` accepts `If-Match: "rev-N"` with `initial_issue_ref`; a different issue revision returns 412 `revision_conflict` and writes nothing. An empty `If-Match` returns 400. Older daemons ignore the header. |
| `0.36.0` | Issue list rows, show output and ready rows carry `scheduled_on_date` and `deadline_on_date`, the `scheduled_on` and `deadline_on` metadata as `YYYY-MM-DD` calendar dates, each omitted when its key is absent or its value cannot be read. Older daemons omit both fields. |
| `0.35.0` | Moves include the issue subtree by default; `single` keeps the move to one issue. `X-Kata-Project-Scope` narrows requests to fixed projects and is enforced in the transaction, including historical issue references. Older daemons do not enforce this header. |
| `0.34.0` | Close transcripts accept agent names beyond Claude Code and Codex, with non-UUID session IDs for those agents. Claude Code and Codex session IDs remain UUIDs. |
| `0.33.0` | Publishes the issue attachment upload and content routes, and UI capabilities include `attachments` when the daemon has Docbank configured. Older daemons omit the field. |
| `0.32.0` | Project event polling accepts an `issue` filter and acknowledges the resolved subject in `X-Kata-Events-Issue-UID`. |
| `0.31.0` | Project federation metadata accepts optional `relay_protocol_version=1` to check existing cross-project issue links before relay enrollment. It returns `409 relay_cross_project_links` with a remedy, or sets `relay_links_checked` when the check passes. Bridge connect returns `hub_links_checked` and preserves this conflict. Older daemons ignore the parameter. |
| `0.28.0` | Comments accept typed replies through `reply_to`, `kind`, and `force`. Issue detail supports `thread`, `inbound`, `kind`, and `since` selectors and includes comment handles, reply targets, backlinks, and edit timestamps. |
| `0.27.0` | Adds filter-bound creation cursors, `complete`, optional filtered `total`, `priority=none`, and repeatable `project_ids` to issue listing. Explicit lexical search with `sort=oldest` supports continuation beyond the ranked candidate cap. |
| `0.26.0` | Close requests accept optional transcript provenance, stored alongside evidence in close events and retained in scoped history and retry receipts. |
| `0.23.0` | Added transactional move previews with the optional `dry_run` request field. Preview responses omit `new_short_id` until a move allocates the target ID. Generated clients now type `new_short_id` as optional; read `issue.short_id` for the issue's current short ID. |
| `0.22.0` | Added temporary assignments: claim requests accept `ttl_seconds`, claim responses return ordered `events`, issue projections include `assignment_expires_on`, and assignment renewal and expiry have distinct event types. |
| `0.21.0` | Added optional `sort=oldest` to both issue-list routes. Matching rows are ordered by `created_at` ascending and `id` ascending before `limit`; omission preserves each route's default. |
| `0.20.0` | Added optional `status=open` or `status=closed` to project search. Omission searches both statuses; explicit empty values are invalid. The predicate applies to lexical candidates and canonical semantic hits before result limits. |
| `0.19.0` | Added issue-subtree token scope and expiration, capability discovery, and redacted credential lifecycle state with a server observation time. Health storage, embedding, and federation diagnostics are optional and depend on caller authority. |
| `0.18.0` | Comments accept optional `teammate` attribution and preserve it in responses and retry fingerprints. |
| `0.17.0` | Create issue, edit issue, create comment, and add label accept a numeric project ID or `name:<project>` in the project path. Optional alias headers use alias-first resolution. Successful responses include `X-Kata-Project-Name`. Generated clients represent these four path parameters as strings. |
| `0.16.0` | Ordinary API array fields are non-null. Empty and nil Go slices serialize as `[]`, generated clients omit `null` from ordinary array types, and requests reject `null` for those arrays. JSON object member names are case-sensitive. |
| `0.15.0` | Added close idempotency and revision headers, the `close-v1` request marker, and retry receipt fields. |
| `0.14.0` | Added `external` close evidence with its required `account` field. |
| `0.13.0` | Added optimistic revision guards to issue and project metadata patch requests. |
| `0.12.0` | Added the optional `idle_shutdown` health block for effective auto-start idle shutdown state and capability discovery. |
| `0.11.0` | Added optional `to_project_uid` on create-time initial links and `expected_project_uids` on edit link deltas so clients can pin resolved relationship targets to immutable project identities inside the mutation transaction. Close-audit rows gain a required `event_id` (stable pagination cursor) and optional `parent_uid`. |
| `0.10.0` | Added the repeatable `issue_uid` query parameter to UI references so embedding hosts can hydrate summaries by stable issue UID. |
| `0.9.0` | Added owner, label, exclusion, and metadata filters to the cross-project issue list. Global list rows now include `project_name`. |
| `0.8.0` | Added repeatable `label` and `exclude_label` query parameters to project-scoped search. It also identifies daemons that support filtered global ready queries. |
| `0.7.0` | Added transactional federation enrollment rotation, idempotent replay semantics for caller-supplied enrollment tokens, and the optional `federation_config` health block used by startup reconciliation. |
| `0.6.0` | Ready responses now return fully hydrated issues. Project-scoped ready rows are `IssueOut` instead of the slimmer `Issue` projection, and global ready rows (`ReadyGlobalIssueOut`, renamed from `ReadyGlobalIssue`) embed `IssueOut` plus `project_name`, so both gain `labels`, `qualified_id` (required), link peers (`parent`, `blocks`, `blocked_by`, `related`), `blocked`, and `child_counts`. Clients regenerated from the schema see the new component name; the shipped Go client keeps `ReadyGlobalIssue` as a deprecated alias. |
| `0.5.0` | Added metadata patch endpoints for issues and projects, issue create metadata, and metadata-key filters on project issue lists. Generated clients can patch metadata with `If-Match` revisions, send initial metadata in create requests, and request selected metadata keys with the `meta` query parameter. |
| `0.4.0` | Added semantic-search response mode metadata and embeddings health fields. Search responses now always include the effective `mode`; configured daemons may also report sanitized embedding reconciler status from `/health`, including backlog and provider HTTP status without raw provider diagnostics. |
| `0.3.0` | Moved the author identity rewrite endpoint from `POST /api/v1/projects/{project_id}/federation/rewrite-author` to `POST /api/v1/projects/{project_id}/actions/rewrite-author`. The operation is project current-state hygiene rather than a federation command, so generated clients should use the project action route. |
| `0.2.0` | Removed `links[].project_id` from link projections. Links are now project-independent edges that may span projects, so a single `project_id` no longer describes a link. `links[].from` and `links[].to` (and the edit response's `changes` block peers) gain `project` and `qualified_id`, always populated. `IssueOut.parent_short_id` is replaced by `parent` (a `LinkPeer` object with all four fields). The cross-project link feature lands across this version. Event payloads are unchanged: a cross-project link mutation currently emits its event in the subject issue's project only (mirrored peer-project events are planned). |
| `0.1.0` | Initial published contract. |

The `0.16.0` wire format uses Go's JSON v2 defaults. Along with non-null slices,
nil maps encode as `{}`, the daemon rejects duplicate object member names and
invalid UTF-8, and struct field names are case-sensitive. JSON object member
order is unspecified. Request schemas still control unknown request members and
reject them on strict request objects.

## Health diagnostics

An unauthenticated `GET /api/v1/health` returns liveness and version fields.
`db_path`, `embeddings`, `docbank`, `federation_config`, and `root_signing` are
optional diagnostics that the daemon includes when configured, only for
daemon-owner credentials and genuine owner-local transport. Public remote
probes and scoped credentials do not receive them. An explicit scoped bearer
stays scoped even over loopback, and an invalid bearer returns an
authentication error. Your client must handle the public response without those
fields. `idle_shutdown` keeps its availability rules from above.

`root_signing` appears only when the daemon cannot sign root receipts for a hub
project it roots:

| Field | Meaning |
| --- | --- |
| `key_file` | Path of `federation-root.key` on the daemon host. |
| `key_loaded` | Whether the daemon holds a usable signing key. |
| `key_problem` | Why no key is loaded, such as `key file is missing`. |
| `projects` | Hub projects whose pin the loaded key cannot satisfy. Signed writes to them return `409 root_signing_key_unavailable`. |

`docbank` appears when `[docbank]` names a key source. When `local = true` and
no `url` is set, the key comes from the local Docbank's runtime record and the
block is absent. On each health request, the daemon resolves its key source
without contacting Docbank (use the [Docbank probe](#docbank-probe) to check
live connectivity):

| Field | Meaning |
| --- | --- |
| `credential` | `ok` when the key resolves, otherwise `missing`. |
| `credential_source` | The source that failed, `file:<path>` or `env:<NAME>`; absent when `ok`. |
| `credential_reason` | Why that source cannot supply a key; absent when `ok`. |

`GET /api/v1/federation/status` adds `root_signing_key` (`available` or
`unavailable`) to every hub project the daemon roots, and adds
`relay_checkpoint_bytes` and `relay_checkpoint_capacity_bytes` to each rooted
project that has a live relay enrollment. Those checkpoint fields show the
estimated size of the signed reset checkpoint the root would build now and the
capacity limit that refuses new relay enrollments and resets once exceeded. If
the measurement fails, the project includes `relay_checkpoint_error` instead of
the size fields, and the rest of the response is unaffected.

To skip relay checkpoint capacity measurement while keeping diagnostic errors,
call `GET /api/v1/projects/{project_id}/federation/status?compact=true`.
Starting with API 0.49.0, you can also pass `omit_errors=true` to withhold
sync, held-batch, embedding, and relay checkpoint error text. In that mode,
`has_sync_error` shows whether a non-empty sync error exists while status facts
stay intact. Omit `omit_errors` when you want the full diagnostic.

`POST /api/v1/federation/root-keys/{project_name}/repin` is owner-only. It pins
that hub project to the daemon's loaded root signing key and creates the key
file if it is missing. The request body accepts `preflight`,
`previous_key_file` (an absolute path on the daemon host that requires
daemon-owner transport or token), and `strand_relays`. The response reports
`status` (`ready`, `current`, `pinned`, `repinned`, or `rotated`), `method`
(`none`, `pin`, `signed_rotation`, or `unsigned_repin`), `key_file`,
`creates_key_file`, `key_id`, `previous_key_id`, `active_relay_enrollments`,
and `consequence`. Calling it on a project this daemon does not root returns
`409 root_key_not_local`, and a previous key that does not match the pin
returns `409 root_signing_key_mismatch`. An `unsigned_repin` while
`active_relay_enrollments` is above zero returns
`409 root_repin_strands_relays` unless `strand_relays` is true. See
[Root signing key](../operations/federation.md#root-signing-key).

### Docbank probe

`GET /api/v1/docbank/probe` (API 0.54.0) sends one authenticated read-only
request to Docbank and reports the result. It answers the same callers that
receive the `docbank` block in health, and returns `404 not_found` to everyone
else as well as when `[docbank]` has no key source (including `local = true`).
If the key does not resolve, the daemon sends nothing and returns
`503 docbank_key_unavailable`, matching the attachment routes. The request
times out after five seconds, and the result is advisory:

| Field | Meaning |
| --- | --- |
| `reachable` | Docbank answered. |
| `key_accepted` | Docbank answered with a 2xx whose body is a folder with an id and directory type, so it accepted the key. `false` when it answered `401` or `403`, answered with another error, answered with a body that is not Docbank's, or was not reached. |
| `http_status` | Docbank's status when it answered with an error; absent otherwise, including when it answered `200` with a body that is not Docbank's. |

## Project access

Team visibility and credential-bound relay enrollment require Kata 0.19.0 or
later.

### Project visibility

Ordinary identity credentials can read, write, and administer a project when
its visibility is `all`, or when their authenticated actor belongs to any team
assigned to a project with visibility `teams`. That rule includes archived
projects: `GET /api/v1/projects?include=archived` lists the archived projects
the credential can access. If a project is restricted to teams and has no teams
assigned, ordinary credentials cannot access it. The system project is
administrative state. Requests with no proxy identity and anonymous clients see
only unrestricted projects, and a request-supplied actor grants no team
membership. Explicit tokenless private-network writes stay limited to
unrestricted projects.

Project access applies across direct references, collections, search
candidates, counts, graphs, relationships, audit events, and browser snapshots.
Unauthorized references return a generic `404`, including references sent in
request bodies. Project access intersects issue-subtree and hosted grants.
Writes, including purge and merge, recheck the current state of the issue or
projects inside their transaction.

### Requests during access changes

Before sending a read or write response, the daemon rechecks project access. It
withholds the response and returns `404` if the caller lost access to one of
the projects admitted for the request, or if an issue the request read (for a
list, a preview, a reload after a commit, or a plain show) now sits in a
project the caller does not hold. For an event page, digest, or issue history,
that check covers every issue its events name: the subject, the related issue,
and any issues in the payload. A write that committed stays committed even when
its response is withheld. Check current state before retrying: a `404` after an
access change does not prove the write failed. Unrelated changes, such as a new
team, a new project, an issue move elsewhere, the deletion of an issue the
request read, or a project that the request itself merged or purged away, do
not affect an in-flight request. Archiving a project does not remove access
from its holders, so an issue linked to a peer in an archived project remains
readable.

An open event stream picks up projects created or restored after it connected,
including one created while the stream was being admitted. The stream closes
when the caller loses an admitted project, or gains a project whose creation or
restore the stream has already passed or is inside the batch it is delivering;
the client then reconnects and reloads. Before sending each frame, the stream
rechecks that frame's event against the caller's current projects. If the
event's issue, or an issue it references, now sits in a project the caller does
not hold, the stream sends `sync.reset_required` at that event's ID instead of
the frame and closes, regardless of the credential's issue scope. Browser cache
validators include the principal and policy revision.

### Administration

Project administration follows project access. Any credential that can access a
project can rename it, archive it (`DELETE /api/v1/projects/{project_id}`),
restore it, purge it, detach its aliases, purge its issues, rewrite its author
identities, and enable, disable, inspect, or run its issue sync. Merging
requires access to both the source and target projects. Every operation rechecks
access to all projects it names inside its transaction.

Daemon-owner credentials keep administration that reaches beyond a single
project: listing, creating, and revoking tokens; managing teams and project
visibility; reading diagnostics; managing federation status, replica creation,
bridges, and the enrollment catalog; and configuring root signing keys. Token
administration stays owner-only because a credential that could mint a token
for another actor would inherit that actor's team access.

### Relay enrollment

Ordinary user credentials may create a credential-bound relay enrollment (by
sending `relay` in the request) for an authorized project. Use
`relay.rebind_parent` to re-attach an existing relay grant to a live
same-account credential while keeping its transport token; the enrollment
rotation route refuses account tokens, so a relay grant is never rotated.
Attempting a legacy project grant with an account token returns
`403 federation_enrollment_requires_relay` regardless of
`[auth].require_token_identity`, because a legacy grant would outlive that
token. An all-project grant requires owner authority and returns
`404 not_found` to anyone else. If a relay enrollment's handshake advertised a
root key that was replaced before the grant committed, the daemon returns
`409 relay_root_changed`; the peer enrolls again and receives the current key. If
a project's signed reset checkpoint would exceed the 64 MiB relay checkpoint
capacity, relay enrollment returns `409 relay_project_too_large` with the
measured size in the message and creates no grant. Purging unused issues can
shrink the checkpoint, though permanent receipts set a floor that purge cannot
remove; see
[Relay checkpoint capacity](../operations/federation.md#relay-checkpoint-capacity).
Enrollment transport checks the actor bound to its authenticated credential
against current project access and cannot claim team authority from an event's
source labels.

If project policy hides at least one project from a principal, or if its token
is limited to an issue subtree, the browser gateway hides configured daemon
targets from that principal. For that principal, the daemon roster omits
non-local targets, and proxied requests return `403` with code
`project_authority_not_delegated` before the gateway contacts the target. A principal that can see every project
keeps the configured targets under the ordinary gateway rules, and daemon-owner
credentials are never limited this way. Project delegation across the browser
gateway requires a separate protocol.

An ordinary account token may also include `expires_in_seconds` without
`scope`. The lifetime must be positive and within the server's duration range.
The token keeps its account authority until it expires; omitting the field or
passing zero keeps the existing non-expiring ordinary credential.

## Issue-scoped credentials

`POST /api/v1/tokens` accepts `scope` and `expires_in_seconds` together:

```json
{
  "actor": "worker-a",
  "name": "worktree-a",
  "scope": {
    "kind": "issue_subtree",
    "project_uid": "01J00000000000000000000002",
    "root_issue_uid": "01J00000000000000000000001"
  },
  "expires_in_seconds": 14400
}
```

`scope.kind` is fixed to `issue_subtree`, both UIDs must identify an active
issue in its active project, and `expires_in_seconds` must be a positive whole
number of seconds within the server's duration range. The server calculates the
deadline. You need identity mode and token-administration authority to mint a
scoped token, and calling this on a spoke project returns
`scoped_token_spoke_forbidden`. Once created, scope, actor, and expiration are
immutable, and there is no renewal or worker-issued delegation endpoint.

Authenticated `GET /api/v1/instance` advertises `issue_subtree_tokens`. Check
that field before minting; if it is missing or false, the daemon does not
support scoped tokens. Token creation returns a one-time `plaintext` value
along with redacted token metadata. CLI and MCP provisioners write the scoped
plaintext to a protected file and keep it out of command output; see
[token CLI usage](cli.md#remote-and-identity-tokens).

The instance `auth` object describes the caller's authority, and browser
session responses, UI snapshot capabilities, and UI reference capabilities
carry the same fields:

| Field | Meaning |
| --- | --- |
| `scope` | `kind`, `project_uid`, and `root_issue_uid`; absent for an unscoped principal. |
| `expires_at` | The scoped token's absolute RFC 3339 UTC deadline. |
| `allowed_actions` | A fixed set derived from daemon policy; callers cannot select individual permissions. |
| `close_requires_evidence` | Requires the normal close message and typed evidence contract, including for TUI requests. |
| `token_audit_read` | Allows the redacted token inventory, independently of ordinary issue write access. |

Writable scoped credentials receive `issue.read`, `issue.create_child`,
`issue.edit`, `issue.comment`, `issue.labels`, `issue.metadata`, `issue.assign`,
`issue.claim`, `issue.close`, `issue.reopen`, `issue.link`, `issue.lease`, and
`activity.read`. Under a read-only policy, they receive only `issue.read` and
`activity.read`. Browser `writable`, `updates`, and `actor_policy` keep their
existing meaning. Clients reject unknown or incomplete scoped authority. Scope
support alone grants no token-administration permission, and domain constraints
can still refuse an otherwise allowed action.

`GET /api/v1/tokens` returns `observed_at` and redacted token rows that include
`scope`, `expires_at`, and `state`. All states use that single observation
time: `revoked` takes precedence over `expired`, and every remaining row is
`live`. A `live` row does not prove the worker is running or that its root
issue is still usable. See
[credential auditing](../guide/web-ui.md#audit-provisioned-credentials) for
operator access and last-use limitations.

Scoped project responses keep their required field types by returning an empty
object for `metadata` and a zero timestamp for `created_at`, and alias
resolution returns an empty alias object. None of those placeholders expose
private project data. For membership, operation constraints, and
[event invalidation and revocation](../design/issue-scoped-credentials.md#events-concurrency-and-revocation),
see the [scope guide](../design/issue-scoped-credentials.md).

## Ready issues

Pass `sort=priority` to sort ready issues by lowest priority number first, with
unprioritized issues last and ties broken by `updated_at DESC, id DESC`. To
keep the default ready order, omit `sort` or send an empty value. Any other
value returns 400.

| Route | Parameter | Values |
| --- | --- | --- |
| `GET /api/v1/projects/{project_id}/ready` | `sort` | `priority` or empty; requires API `0.41.0`. |
| `GET /api/v1/ready` | `sort` | `priority` or empty; requires API `0.41.0`. |

To fetch the highest-priority due issue, combine `sort=priority` with
`limit=1`. Parked issues do not count toward the limit. The CLI and MCP `next`
check the API version first; against older daemons, they fall back to fetching
the full queue and selecting on the client.

## Temporary assignments

`POST /api/v1/projects/{project_id}/issues/{ref}/actions/claim` accepts an
optional `ttl_seconds` integer from 60 through 86400. If you omit it on an
unassigned issue, the daemon creates a permanent assignment. When the current
actor repeats a timed claim, the daemon renews its expiry; repeating a
permanent claim preserves any existing expiry. `if_unowned` refuses every live
assignment, including one held by the same actor, while `force` can replace
another actor.

The `events` array in the response is ordered. Taking over an expired
assignment returns `issue.assignment_expired` before the new `issue.assigned`
event, while the singular `event` field stays the final event for
compatibility. Renewal emits `issue.assignment_renewed`. Issue projections
expose the nullable absolute UTC deadline as `assignment_expires_on`.

At the expiry boundary, `ready` and `next` treat the issue as unassigned even
before the cleanup worker records the expiry event. Cleanup later clears the
stored owner and deadline conditionally, so a concurrent renewal or takeover is
preserved.

On a federated spoke, the hub is authoritative for temporary claims. The spoke
forwards the request with its enrollment credential, persists the returned
events in order, and does not fall back to a local claim when the hub is
unavailable. A hub may also include `replay_events` carrying the assignment
history needed to apply a renewal after the spoke missed earlier events. The
spoke persists those prerequisites before the mutation events and omits them
from the client-facing response.

## Resolving projects inside mutations

These operations accept either a numeric project ID or a `name:` selector:

| Method | Path |
| --- | --- |
| `POST` | `/api/v1/projects/name:example-project/issues` |
| `PATCH` | `/api/v1/projects/name:example-project/issues/{ref}` |
| `POST` | `/api/v1/projects/name:example-project/issues/{ref}/comments` |
| `POST` | `/api/v1/projects/name:example-project/issues/{ref}/labels` |
| `PATCH` | `/api/v1/projects/name:example-project/issues/{ref}/labels` |

URL-encode the project selector as a single path segment. Numeric paths such as
`/api/v1/projects/42/issues` remain valid, and the issue ref and mutation body
keep their existing meanings. Successful responses return the canonical project
name in `X-Kata-Project-Name`.

A `name:` request can also send `X-Kata-Project-Alias` (the workspace alias
identity) and `X-Kata-Project-Alias-Kind` (`git` or `local`). The alias takes
precedence over the name, preserving rename and merge resolution. If the alias
is unknown, the daemon uses the existing named project and attaches the alias.
Sending `name:` with no name requires a registered alias. Resolution does not
create projects, and numeric selectors do not accept alias headers.

Attaching an alias requires attributed write authority and is unavailable to
local browser sessions. When a host access controller is installed, named
resolution requires all-project authority before resolution or attachment,
matching the standalone project-resolve endpoint. Project-scoped clients can
continue using numeric IDs.

## Listing issues

Both `GET /api/v1/projects/{project_id}/issues` and `GET /api/v1/issues`
accept `sort=oldest` (creation ascending) and `sort=created` (creation
descending). Both break equal `created_at` timestamps by `id` in the same
direction and apply the sort before `limit`. `sort=oldest` requires API 0.21.0;
`sort=created` and the pagination contract below require API 0.27.0.

Responses include `issues`, `complete`, `truncated`, and, when another page
exists, `next_cursor`. To fetch the next page, pass the opaque `next_cursor` as
`cursor` with the same explicit sort and filters. Pass `limit=0` to return all
remaining matches, and pass `include_total=true` to add `total`, a SQL count
with the same filters and permissions that does not depend on the cursor or
page size.

Pass `compact=true` to omit `body` from each row and keep only the metadata
keys named by `meta` filters. Use this when only those keys matter: an imported
issue can carry a body, title, or metadata value larger than your client wants
to read. If a compact row exceeds 64 KiB, it carries only the issue's identity
and status along with `oversized: true`, so a page of up to 100 compact rows
stays under 8 MiB and still reaches the next cursor. `compact=true` requires
API 0.43.0; older daemons ignore it and return full rows.

To filter for issues with unset priority, pass `priority=none`. That filter is
mutually exclusive with `max_priority`; numeric `priority` accepts 0 through 4,
and `max_priority` excludes unset priorities. The global route also accepts
repeated positive `project_ids` values to return one filtered page across
selected projects. The daemon rejects an empty or invalid selection.
`project_ids` and `project_id` are mutually exclusive, and you must have
authorization for every selected project.

If you omit `sort` or send an empty value, the project route defaults to
`updated_at` descending and the global route defaults to `created_at`
descending. The project default cannot continue with a cursor; use an explicit
creation sort for exhaustive scans.

Ordinary edits preserve creation order. Cursors validate the daemon instance,
project selection, authorization scope, sort, and filters, and changing any of
them returns HTTP 400. You may change the page size and `include_total` between
pages. These are live reads rather than snapshots. Restart pagination after
changes to filter membership, project membership, deletion, or imported
creation timestamps.

### Search continuation

`GET /api/v1/projects/{project_id}/search` accepts `cursor` only when you pass
explicit `mode=lexical&sort=oldest`. It returns oldest-creation pages with the
same `next_cursor`, `complete`, and `truncated` fields, without the
200-candidate ceiling of ranked search. The query, filters, daemon, and
authorization scope must stay the same across pages, and search does not
provide `total`.

Other searches keep ranked ordering and bounded candidate retrieval with no
continuation cursor. `complete` is false for truncated or degraded results and
whenever ranked retrieval cannot confirm exhaustion; do not treat a full
candidate window as the whole matching set.

## Compatibility expectations

These are the current intentions, not a contractual guarantee:

- **Contract changes bump `api_schema_version`.** New endpoints and schema
  fields, including optional response fields, get a new version, while
  documentation-only edits keep the existing version. Response objects use
  `additionalProperties: true`, so older clients accept unknown optional
  response fields. Request schemas stay strict by default and reject unknown
  fields unless a type explicitly opts in. The OpenAPI 3.0 flavor
  (`kata openapi --version 3.0`) leaves `additionalProperties` unset on
  response schemas so generators model optional object-valued response fields
  as pointers.
- **Breaking changes bump `api_schema_version`.** Removing or renaming a field,
  changing a field's type, or removing an endpoint is a breaking change and
  bumps `api_schema_version`. A client that pins or checks the version it was
  generated against can catch the mismatch upfront instead of failing at an
  arbitrary call site.
- **Regeneration stays honest.** A committed golden test fails if
  `api/openapi.yaml` drifts from the routes, so the published schema cannot
  silently fall out of date with the daemon it describes.

For version bumps and marker records, see the
[API contract workflow](../development/contributing.md#api-contract-changes).

If you build against this schema and hit a gap, open an issue. The contract is
meant to be useful to external clients, and your feedback shapes how far the
compatibility guarantees go.

## Operator diagnostics

`GET /api/v1/doctor` supplies the daemon-side hook findings that
[`kata doctor`](../operations/doctor.md) displays. Its `hooks` block reports
current executable and working-directory availability, queue capacity and
occupancy, process-lifetime dropped events, and counts of runs and failures
from the last seven days found in scanned history. `history_truncated` marks a
scan that hit the eight-MiB or 64-rotated-file budget, where counts may omit
recent runs. `history_incomplete` marks missing, unreadable, or malformed
history. Both flags are independent: a capped scan can also hit a read error.
The route never returns arguments, environment variables, hook output, or raw
errors. `available: false` means the hook sink does not supply diagnostics.

This endpoint requires owner-local or configured static/bootstrap-token
authority. Mounted hosts authorize it as a non-mutating
`integration_administration` operation with the `manage` capability. Ordinary
identity tokens, browser sessions, trusted-proxy principals, and
unauthenticated private-network modes cannot access these process diagnostics.
A 404 can mean either that the endpoint is unsupported or that it is hidden
from your principal.

The optional `/health` embeddings block also includes `error_present` when the
reconciler has an error, including failures without a provider HTTP status,
without disclosing the provider's raw diagnostic text.

## Browser UI endpoints

The daemon serves the production browser application and its canonical routes
from the same origin as the API. Browser reads use two native projection
endpoints published in OpenAPI, and the daemon also publishes a launch-target
endpoint there:

- `GET /api/v1/ui/snapshot` returns one coherent catalog, collection, optional
  selected detail/history/graph projection, capability envelope, durable event
  cursor, and strong ETag.
- `GET /api/v1/ui/references` returns bounded projects, owners, labels, and
  qualified issue references for pickers and typeahead. Passing `q` returns
  only issue references, with empty projects, owners, and labels. Repeating
  `issue_uid` hydrates summaries for up to 200 stable issue UIDs and combines
  with the existing project and text filters.
- `GET /api/v1/ui/issue-reference?ref=<ref>` resolves an issue reference to its
  issue and project UIDs. A bare reference needs `project_id` or `project`, an
  exact name or UID; `project` also returns `scope_project_uid`. When both are
  supplied, `project_id` takes precedence and `project` is ignored.
- `GET /api/v1/ui/launch-target?issue_uid=<uid>` validates one active issue and
  returns a safe absolute route into the standalone browser application:

  ```json
  {"available": true, "url": "https://kata.example/kata?issue=<uid>#direct=1"}
  ```

  If no credential-safe browser origin is configured, it returns HTTP 200 with
  `{"available":false,"reason":"browser_origin_unavailable"}` and no URL.
  Unknown or deleted issues, archived projects, and host-denied resources
  still return not found. The `#direct=1` fragment keeps the standalone
  application on the daemon that issued the target even when its roster
  defaults to a different remote daemon.

When you pass `include_graph=true`, the snapshot returns only issues and links
reachable from the selected issue through parent, blocks, or related links in
either direction. Traversal stays within issues the caller can read, and the
graph is empty if no selected issue is available.

You can count a parent's open and total children from the snapshot alone,
without a second `GET /api/v1/issues` request. `collection_links` includes
every link between two issues the principal can read (non-deleted issues in
non-archived projects) that has at least one endpoint in `collection`,
regardless of the collection's view, project_uid, status, owner, label,
relationship, text, local_date, time_zone, or limit parameters. For each
`parent` link whose parent (`to_issue_uid`) is in `collection`, `from_status`
gives the child's status. Counting those links per parent produces the same
open and total counts as `child_counts` on `IssueOut` from `GET /api/v1/issues`
for the same principal at the same cursor, where an absent `child_counts` means
zero of zero. Children the principal cannot read are excluded from both. This
holds for every daemon whose snapshot reports `contract_version` `2`. The
daemon-switcher proxy (`/api/v1/ui/proxy`) does not forward
`GET /api/v1/issues` or project issue lists, so `collection_links` is how you
get child counts through it.

If a snapshot request sends a matching `If-None-Match`, the daemon returns
`304` without rebuilding the projection. For a newly built projection, the
daemon captures the response cursor in the same consistent storage read.
Browser events act as invalidations: the SPA refreshes the snapshot instead of
building a second state store from event payloads.

The browser-session routes are deliberately outside the generated client
contract. A fresh tab on a direct-loopback origin calls
`POST /api/v1/ui/session/local`, which checks the exact Host, peer address,
forwarding headers, and identity/proxy authentication policy before issuing a
local-web session. A static daemon token
does not disable this owner-local path. The mint normally requires the exact
Origin; an embedded owner-local browser may omit or send an empty Origin only
on this endpoint, while cross-site Fetch Metadata remains forbidden. Exact Host
validation runs first and remains the DNS-rebinding boundary.

Both the URL from `kata daemon status` and local `kata ui` open that loopback
origin directly. Configured origins exchange a bearer or identity token at
`POST /api/v1/ui/session/login`. To log out, call `DELETE /api/v1/ui/session`
with the issued cookie, `X-Kata-Web-Session`, `X-Kata-CSRF`, and the exact
configured `Origin`. A successful logout returns 204 and revokes that tab's
session immediately, including its event stream, while other tabs stay valid.
`/api/v1/ui/session/logout` does not exist, and there is no browser-session
refresh endpoint.

CLI federation enrollment can use the hub's admin bearer token even when the
web UI uses `login` mode. The CLI reads the variable named by
`--hub-token-env <env-name>` or uses a daemon catalog entry whose URL matches
the hub origin, and never forwards the local daemon's global token to the hub.
Ordinary API requests without browser credentials use daemon bearer
authentication on both browser and shared TCP listeners. Host validation still
applies, and requests carrying browser credentials or Fetch Metadata go through
browser session and Origin checks. On session-authentication errors, the daemon
returns the canonical browser origin in `X-Kata-Web-Origin` and its `loopback`
or `login` mode in `X-Kata-Web-Authentication`, so `kata ui` can tell a
configured direct loopback URL apart from a login-only deployment without
carrying a launch credential.

The daemon builds the launch-target URL only from its configured canonical
browser origin. Request `Host`, `Origin`, and forwarding headers never supply
authority, and origins with credentials, path prefixes, query strings, or
fragments are treated as unavailable rather than rewritten.

After creating a session, send both the instance cookie and
`X-Kata-Web-Session` on every browser data request. For browser data mutations,
also send the exact `Origin` and `X-Kata-CSRF`. The protected event stream
sends the session header and `Last-Event-ID` through `fetch`. In
unauthenticated `--insecure-readonly` mode, the UI advertises read-only
authority and polls snapshots because the event stream remains protected.

## Federation Enrollment and Health Endpoints

`GET /api/v1/projects/{project_id}/federation/events` accepts an optional
`wait` in seconds. A caught-up poll may wait up to 45 seconds for new events;
a zero or omitted wait answers at once. Successful responses advertise the
maximum wait in seconds in `X-Kata-Federation-Poll-Wait-Max`, and the most
projects one watch can name in `X-Kata-Federation-Watch-Max`. A hub that
requires signed federation requests advertises `0` watch projects.

`POST /api/v1/projects/{project_id}/federation/events:watch` holds one request
for the route project and up to 255 more. The bearer authorizes the route
project. Each other project in the body carries its own enrollment token:

```json
{"after_id": 120, "projects": [{"project_id": 7, "after_id": 33, "token": "..."}]}
```

The hub checks each token against its own project, as it would check a poll
by that token. It answers as soon as any named project has events or a reset
after its cursor, when no named project is left to watch, or when the `wait`
ends. The response names projects only; pull their events with the event
poll:

```json
{"ready": [7], "unwatched": []}
```

`ready` also names a project whose own poll would refuse the read, such as
for event features the spoke lacks; that poll says why. `unwatched` lists
projects the hub did not watch: their token does not authorize pulls from
them, they are outside the request's `X-Kata-Project-Scope`, or the request is
signed. A signed request proves only the route credential's key, so it
watches the route project alone. A list
with more than 255 other projects, a repeated project, the route project, a
negative cursor, or a missing token returns `400 validation`. The response
carries the same `X-Kata-Federation-Poll-Wait-Max` and
`X-Kata-Federation-Watch-Max` headers as the event poll.

`POST /api/v1/projects/{project_id}/federation/events:ingest` returns
`400 validation` when a fresh event references a stored sibling-project issue
through an added link, close parent, or duplicate/superseded evidence and
either project uses a relay. The error names the event UID and rolls back the
entire batch. Before retrying or skipping a refused batch, see
[quarantine recovery](../operations/federation.md#quarantine).

`POST /api/v1/federation/enrollments` accepts an optional `token`. If you omit
it, the endpoint creates one enrollment and returns a newly generated plaintext
enrollment token. If you supply a token, an exact retry with the same spoke
instance UID, project ID, canonical capabilities, resolved actor, and adoption
policy returns the same active enrollment. Reusing that token with different
attributes, or after its enrollment was revoked, returns `409` with
`federation_enrollment_token_conflict`. Actor resolution runs before this
comparison. A DB-backed account token may create a relay enrollment and
supplies its actor, and cannot create or rotate a direct grant. In identity
mode, the owner's bootstrap token may create or rotate direct grants with
`actor` naming an existing account that has an unscoped API token, including an
expired or revoked token. For a project-scoped grant, that account must be able
to see the project, or the request returns `400` with `validation`. The grant
follows that account's project visibility and stays valid after its API tokens
expire or are revoked. See
[Federation token boundaries](../operations/federation.md#token-boundaries).

Enrollment creation accepts both `lease` and `claim`, and returns sorted,
deduplicated capabilities using the canonical `claim` name. A project-scoped
grant with a supplied `hub_url` requires enabled hub federation. Once
federation is enabled, creation returns a `join` object with the hub project ID
and UID, local project name, baseline and replay cursors, actor, capabilities,
token, transport options, and `join_command`. Set `hub_url` to an HTTP(S) base
URL the spoke can reach, optionally with a federation API reverse-proxy prefix.
The server does not infer a federation address from its browser origin, and
dedicated browser listeners and authentication proxies may not serve federation
transport. For an ordinary join, non-loopback HTTP URLs, including private IP
addresses, require `allow_insecure: true`; without it, enrollment returns `400`
before issuing a token. The returned command includes `--allow-insecure` when
needed, and HTTPS still verifies certificates. A hub that requires signing
ignores any supplied `hub_url`; `join.hub_url` uses its signing `external_url`,
and the command includes signing key placeholders without `--allow-insecure`.

The returned command includes `--project`, the hub project ID, and any push or
adoption options. Join fetches the current UID and cursors from the hub when it
runs, so a purge after enrollment does not leave it on an old baseline. Treat
the response and command as secrets. Wildcard grants have no project `join`
object, and grants without `pull`, or without a supplied hub URL or a
required-signing external URL, have no runnable join command. Legacy API
callers that omit `hub_url` may issue a grant before enabling federation; those
responses omit `join` until hub metadata exists. Listing grants never returns
plaintext tokens or join commands.

To repair a project-scoped spoke whose local binding still exists after its
enrollment credential was lost, call
`POST /api/v1/federation/enrollments/actions/rotate`. It uses the same hub
administration authorization as enrollment creation and requires
`spoke_instance_uid`, a positive `project_id`, `capabilities`, and an explicit
replacement `token`; `actor` and `allow_adoption_snapshot_authors` work the
same way as on creation. In a single transaction, the hub revokes active grants
for that spoke and project and installs the replacement. After canonical
capability normalization and actor resolution (including explicit account
selection for bootstrap callers), retrying the same replacement token with the
same spoke instance UID, project ID, canonical capabilities, resolved actor,
and adoption policy returns the same active enrollment. An attribute mismatch
or a revoked replacement enrollment returns `409` with
`federation_enrollment_token_conflict`.

`POST /api/v1/federation/replicas/{project_id}/actions/leave` accepts three
mutually exclusive coordination flags:

- `preflight=true` runs the existing read-only eligibility check.
- `prepare=true` validates the same local conditions, durably marks any
  config-managed credential as leaving, blocks new config reconciliation for
  that mapping, and waits for earlier enrollment or rotation requests to drain.
  Its `pending_enrollment` response returns the non-secret hub coordinates
  needed for revoke, and `preparation_id` identifies this preparation. After
  hub cleanup, call the endpoint again without any flag for authoritative local
  teardown. Repeating prepare and finalization is safe after interruption.
- `abort=true` releases a preparation if hub cleanup fails or the prepare
  response is lost, so rebind and join work again without a daemon restart.

Send `prepare` a random `preparation_id` of at most 128 characters. Your client
then knows the ID even if the prepare response is lost and can retry prepare
with the same ID: repeated prepares with one ID hold a single preparation. If
you omit `preparation_id`, prepare generates one and returns it.

When you call `abort=true` with a `preparation_id`:

- Abort releases only the named preparation of this project on this daemon.
  Another client's preparation keeps the mapping blocked until it finishes or
  is released.
- Aborting an unknown or already released preparation succeeds and changes
  nothing, so your client can retry it safely.
- Abort returns `409 federation_leave_started` when a credential or
  config-managed reservation is marked for leave, the project is archived, or
  the binding is detached. Finish those leaves instead.
- Calling abort without `preparation_id`, or passing `preparation_id` with
  preflight or teardown, returns `400 validation`.

`POST /api/v1/federation/replicas/{project_id}/actions/rebind` accepts only
`{"hub_catalog":"<name>"}`. The server resolves that name from its own daemon
catalog, and callers cannot supply a replacement URL or credential. The catalog
entry must be a remote HTTPS entry. The endpoint deliberately sends the spoke's
existing enrollment token to that configured endpoint and proceeds only when
federation metadata returns the persisted hub project ID and UID. Catalog
administration tokens are never resolved for this call. On success, it returns
the local project, display-safe old and new origins, and `rebound`, `resumed`,
or `unchanged`. Retries safely converge from fully old,
credential-moved-first, binding-moved-first, or fully migrated state. Before
local convergence, the action drains project-scoped federation transport using
the old endpoint and prevents new transport from starting until both stores
name the target.

When config-driven mappings are present and the caller may read
[health diagnostics](#health-diagnostics), `GET /api/v1/health` adds an
optional `federation_config` object:

| Field | Meaning |
| --- | --- |
| `configured` | Number of startup mappings. |
| `reconciled` | Mappings that reached the requested active binding. |
| `pending` | Mappings awaiting their first attempt or retrying a runtime failure. |
| `conflicted` | Mappings whose latest attempt found incompatible configuration, credentials, or a local binding. They continue retrying in case an operator resolves the conflict. |
| `last_attempt_at` | Latest attempt time across all mappings, when any attempt has run. |
| `last_success_at` | Latest successful reconciliation time across all mappings, when any has succeeded. |
| `last_error_category` | Sanitized category from the most recent failed attempt; omitted until a failure occurs. |
| `last_error_status` | Remote HTTP status for that failure when one is available; omitted otherwise. |

Error categories are `configuration_conflict`, `binding_conflict`,
`credential_io`, `hub_unavailable`, `hub_authentication`, `hub_validation`,
`local_storage`, and `internal`, where `internal` marks an unexpected sanitized
reconciler failure.

The block is absent when no mappings are configured, and it never exposes
tokens, hashes, headers, URLs, project names, actors, or raw remote response
bodies. Reconciliation is fail-open: `ok` stays `true` while mappings are
pending or conflicted, so hub availability does not control spoke readiness.

## Comment Endpoints

Append a comment with
`POST /api/v1/projects/{project_id}/issues/{ref}/comments` and edit one in
place with
`PATCH /api/v1/projects/{project_id}/issues/{ref}/comments/{comment_ref}`.
Use the comment UID for `comment_ref`; numeric comment IDs are local storage
artifacts.

If you may need to retry comment creation, send `Idempotency-Key`. For seven
days, repeating the same issue, actor, body, and key returns the original
comment. Keys belong to individual issues, so a retry can survive an issue move
between projects. Reusing the key on that issue with a different actor or body
returns a conflict. To identify the issue across moves, use its full issue UID.

Editing a comment overwrites its current body while keeping the original
author, UID, creation time, and thread position. This is the supported way to
redact comment text before enrolling a project in federation; it does not purge
data that has already crossed into another federated event log.

## Label Endpoints

`POST /api/v1/projects/{project_id}/issues/{ref}/labels` attaches one label and
`DELETE /api/v1/projects/{project_id}/issues/{ref}/labels/{label}` detaches one.

To change several labels at once, for example moving an issue from
`ready-for-agent` to `ready-for-human`, send `PATCH
/api/v1/projects/{project_id}/issues/{ref}/labels` with `add` and `remove`
lists (API `0.70.0` or newer):

```json
{"actor": "reviewer", "add": ["ready-for-human"], "remove": ["ready-for-agent"]}
```

The daemon removes the `remove` labels, then attaches the `add` labels, in one
transaction. Labels already attached or already absent are skipped without an
event. The response carries `issue`, `labels` (the issue's labels after the
change, sorted, read in the same transaction), `events` (`issue.unlabeled` then
`issue.labeled`, empty when nothing changed), and `changed`.

A request returns `400 validation` and changes nothing when both lists are
empty, when a label in either list does not match `[a-z0-9._:-]` with length
1..64 (a mistyped `remove` entry included), or when the same label appears in
both lists. The PATCH route's project path accepts a `name:` selector.

Labels `p0` through `p4` and labels beginning with `priority:` are reserved.
Set issue priority through the priority field. You can remove a priority label
already attached to the issue.

## Attachment Endpoints

`POST /api/v1/projects/{project_id}/issues/{ref}/attachments?name=<file>`
uploads the raw request body as `application/octet-stream` to Docbank and
returns the `attachment_id` with its stored reference. Since API 0.47.0,
set the query parameter `reuse_existing=true` to return the smallest existing
attachment ID for the resolved Docbank version, or create a reference if none
exists. On uploads and links, reuse returns `attachment_invalid` if the daemon cannot read an existing reference. Omitting it or setting it to `false` creates a new reference. `GET
/api/v1/projects/{project_id}/issues/{ref}/attachments/{attachment_id}/content`
streams the file back. `POST /api/v1/projects/{project_id}/issues/{ref}/attachments/links` saves a reference to an existing Docbank file version. Its JSON body takes exactly one of `node_id` or `path`, an optional `version_id`, and `actor`. Set `reuse_existing` to `true` to return the smallest existing attachment ID for that version without adding another reference; if no reference exists, the route creates one. Omitting this field creates a new reference. Local routes return `docbank_not_configured` when the daemon has no Docbank, and `503 docbank_key_unavailable` when the daemon cannot resolve its Docbank key. See [Issue attachments](../guide/attachments.md).

Federation transport uses these routes under `/api/v1/projects/{project_id}/federation/docbank`:

- `GET /capabilities` identifies the project, origin, and protocol version.
- `POST /issues/{ref}/uploads` forwards bytes unless another holder has an active claim on the issue.
- `POST /issues/{ref}/links` resolves a file reference unless another holder has an active claim on the issue.
- `GET /versions/{version_id}/content` streams content from the origin.

An attachment upload or link checks the route's capabilities before forwarding; a read requests content directly. References without an origin resolve in the local vault, including when a standalone project later becomes a hub. The UI attachment capability is enabled when the daemon has a local vault or any enabled spoke. A project without an available route can still show attachment controls; individual requests report route errors.

## Project Hygiene Endpoints

Call `POST /api/v1/projects/{project_id}/actions/rewrite-author` to rewrite one
exact author identity across a project's current rows. Send `actor`, `from`,
and a non-empty `to` in the request body. The daemon updates every issue
author, issue owner, comment author, and link author that exactly matches
`from`, emits one `project.author_rewritten` event if anything changed, and
returns per-field counts. The endpoint refuses already-federated projects
because it cleans up local current-state rows rather than rewriting federated
history.

## Issue Graph Endpoint

Call `GET /api/v1/projects/{project_id}/issues/{ref}/graph` to fetch the
relationship graph reachable from one source issue. `ref` accepts the same
issue reference forms as other project-scoped issue endpoints. The optional
`depth` query parameter accepts either `full` (the default) or a non-negative
hop count such as `1`, `2`, or `3`. To keep the source issue while excluding
closed non-source issues from traversal and output, pass `hide_done=true`.

The response returns a canonical graph payload:

| Field | Meaning |
| --- | --- |
| `source_uid` | Stable UID of the source issue. |
| `depth` | Effective traversal depth, either `full` or the bounded hop count. |
| `hide_done` | Whether closed non-source issues were hidden. |
| `nodes` | Reached issues, including normal issue fields plus `qualified_id` (`project#short_id`) for display. |
| `edges` | Directed relationship edges with `from_uid`, `to_uid`, `kind`, and `layout`. |
| `unresolved_refs` | Link endpoints that exist in storage but could not be materialized as graph nodes. |

The daemon normalizes edge direction for clients: parent edges point from
parent to child, `blocks` edges point from blocker to blocked, and related
edges use Kata's canonical related-link ordering. `layout=false` keeps an edge
in the graph while telling layout engines they can omit it from force or layout
calculations; current daemons set that hint on transitive `blocks` edges.

The daemon hides soft-deleted issues and issues in archived projects instead of
reporting them as unresolved references. `unresolved_refs` is reserved for
dangling endpoints that can appear after imports, federation repair, or manual
database maintenance, so graph clients can show incomplete data without
discarding the reachable graph.

## Issue Sync Endpoints

Issue sync endpoints are project-scoped and provider-qualified, configuring
one-way external issue sync into a Kata project. The `github` and `notion`
providers back the `kata sync github ...` and `kata sync notion ...` commands.
Each project can have only one external issue-sync binding.

The daemon handles all GitHub access itself. For the `github` provider, v1
accepts `github.com` and exact GitHub Enterprise hostnames listed in
`KATA_GITHUB_SYNC_ALLOWED_HOSTS`. The daemon looks up credentials in this
order:

1. A matching `[[github_sync.app]]` entry.
2. The explicit token environment variable named by `[github_sync].token_env`
   (default `KATA_GITHUB_TOKEN`), only when `[github_sync].token_host` matches
   the binding host.
3. `gh auth token --hostname <host>` as a local fallback.

Remote clients call these endpoints on the daemon, so client-side GitHub
credentials are not enough when the daemon runs elsewhere, for example behind
`https://daemon.example`.

For shared daemons, use a GitHub App credential; the App only needs Metadata
read and Issues read permissions. The `gh auth token` fallback is best suited
to local and single-user daemons. When the GitHub host exposes the queried
parent fields, the daemon imports GitHub-sourced parent links; unsupported
schemas are nonfatal and keep existing source-managed parent links intact.

The endpoint layout is provider-neutral so future providers such as GitLab or
Linear can share the same lifecycle routes. Provider-specific identity belongs
in `config`, which must not contain raw credentials; providers that need
credentials should use a credential reference or a daemon-local auth mechanism.

JSONL restore imports issue sync bindings in a disabled state. Verify the
restored host and daemon credentials before re-enabling them locally.

Enable sync:

```http
POST /api/v1/projects/{project_id}/issue-sync/github/enable
Content-Type: application/json

{
  "config": {
    "host": "github.com",
    "owner": "example-org",
    "repo": "example-repo",
    "title_prefix": true
  },
  "interval": "5m"
}
```

For GitHub, `config.host` defaults to `github.com`, and you may send
`interval_seconds` instead of `interval`. `config.title_prefix` defaults to
`true` initially and keeps the saved choice when omitted on re-enable. Set it
to `false` to keep GitHub issue titles as-is and add the plain `github` label,
or pass `true` explicitly to restore the `[GitHub #123]` prefix. Local title
edits and local labels are preserved, and matching upstream `github` labels are
deduplicated. The daemon validates the repository with its configured GitHub
credential chain, stores the repository identity, and returns the binding and
status.

For Notion, use `/api/v1/projects/{project_id}/issue-sync/notion/enable`. All
Notion reads use the daemon's token environment variable selected by
`[notion_sync].token_env` (default `KATA_NOTION_TOKEN`). The enable request
accepts:

| Notion `config` key | Meaning |
| --- | --- |
| `data_source_id` | Data source UUID. Initial enable requires this or `database`, never both. |
| `database` | Database UUID or supported Notion URL containing exactly one data source. |
| `status_property` | Status property ID or exact name; omitted initially selects the sole Status property. |
| `assignee_property` | People property ID or exact name. A People property is required; omit the selector initially only when exactly one exists. |
| `done_statuses` | Array of completed status IDs or exact names; at least one is required initially. |
| `title_prefix` | Boolean, initially true. False keeps source titles and adds the `notion` label. |
| `since` | Exclusive updated-after UTC date or whole-second RFC3339 timestamp; empty clears the cutoff. |

When you re-enable Notion, omitted selections, interval, cutoff, and
title-prefix choice keep their saved values. The response config includes
resolved `data_source_id`, `database_id`, `title_property_id`,
`status_property_id`, `assignee_property_id`, `done_status_ids`, `since`, and
`title_prefix`. Source and mapping IDs cannot change on re-enable. For access,
field ownership, polling limits, and recovery, see
[Notion sync](../operations/notion-sync.md).

If another edit modifies the binding while enable is running, the route returns
HTTP `409` with error code `issue_sync_binding_changed`; read the current
status before retrying your change. If Notion times out during enable, the
route returns HTTP `504` with error code `notion_timeout`.

Disable sync:

```http
POST /api/v1/projects/{project_id}/issue-sync/github/disable
Content-Type: application/json

{}
```

Disabling stops polling while keeping the binding, cursor, status, and import
mappings so re-enabling can resume against the same source identity. For Notion
bindings, replace `github` with `notion` in the disable, status, and once
paths.

Read status:

```http
GET /api/v1/projects/{project_id}/issue-sync/github/status
```

Status returns a `not_enabled` state when the project has no binding, and
returns existing disabled bindings with their last run status.

Run once:

```http
POST /api/v1/projects/{project_id}/issue-sync/github/once
Content-Type: application/json

{}
```

`once` runs one immediate daemon-side sync for an enabled binding. It bypasses
the interval schedule while still respecting the in-flight guard; overlapping
runs return a conflict while the claim stays active. Operator edits fence an
older run from further writes or imports while it keeps its claim until it
finishes or the stale horizon expires.

The shared response body contains:

| Field | Meaning |
| --- | --- |
| `binding` | The stored provider binding, including provider, stable source key, remote ID, display name, opaque config, enabled flag, interval, cursor, and timestamps. It is absent only for a project with no binding for the requested provider. |
| `status` | Current state, `status_sync` mode, `pending_count`, and the last attempt, success, error, and import counts. |
| `status_updated` | Present on `once`; incoming status transitions committed separately from content imports. |
| `import` | Present only on `once`; reports created, updated, unchanged, comment, and link counts from the import run. |

GitHub owns the title, body, state, labels, owner, and imported GitHub comments
on synced issues. Treat those fields as read-mostly in Kata: newer GitHub
updates can overwrite local edits to them. Explicit close and reopen
write back when `status_sync` is `two-way`; other fields stay incoming-only.

V1 does not support outbound fields other than status, timeline events, pull
requests, deleted or transferred issue propagation, edited or deleted comment
propagation, or multiple assignees beyond the first GitHub assignee.

Plane uses the same issue-sync routes with provider `plane`. Initial enable
`config` requires string `workspace` and UUID string `project_id`, while
optional `since` and boolean `title_prefix` keep their saved values when
omitted on re-enable. Top-level `status_sync` accepts `one-way` or `two-way`
and keeps the saved choice when omitted. Optional config strings
`closed_state_id` and `open_state_id` pick UUIDs in the completed and unstarted
groups, and empty strings clear those overrides. Pass either `interval` or
`interval_seconds` for the poll interval. Origins and tokens are daemon-owned
and rejected in request config. Source identity is immutable, and validation
reads the project and states before a guarded upsert. See
[Plane sync](../operations/plane-sync.md).

Linear uses the same issue-sync routes with provider `linear`. Initial enable
`config` requires UUID strings `workspace_id` and `team_id`, and an optional
string `project_id` restricts the team to a single project. Scope is immutable.
Optional `since`, `closed_state_id`, and `open_state_id` are strings, and
`title_prefix` is a boolean. Top-level `status_sync` and the interval fields
follow the same issue-sync contract, and re-enable keeps omitted options.
Binding requests reject credentials and endpoint selection. See
[Linear sync](../operations/linear-sync.md).

Twenty uses the same issue-sync routes with provider `twenty`. Enable discovers
and verifies workspace identity from the daemon-owned credential. Optional
`config` keys are string `since`, boolean `title_prefix`, string
`closed_status`, string `open_status`, and string array `open_statuses`.
Top-level `status_sync` accepts `one-way` or `two-way`, and the interval
accepts `interval` or `interval_seconds`. Omitted settings keep their saved
values. The route rejects origins, credential selectors, keys, and workspace
IDs in request config, and validates status classification against live
metadata before a guarded upsert. Empty or whitespace-only `closed_status` and
`open_status` values are rejected; to keep a saved value, omit the key. See
[Twenty sync](../operations/twenty-sync.md).

Todoist uses the same issue-sync routes with provider `todoist`. Initial enable
`config` requires opaque string `project_id`. Optional string `history_since`
sets a UTC date or whole-second RFC3339 completion floor, defaulting to thirty
days ago when omitted initially. Boolean `title_prefix`, top-level
`status_sync`, and the interval fields keep their saved values when omitted on
re-enable. The daemon resolves `account_id`. Account, origin, project, and
history floor are immutable, and requests cannot supply tokens or origins.
Two-way delivery calls completion and reopen endpoints after fresh safety
checks. See [Todoist sync](../operations/todoist-sync.md).

TickTick uses the same issue-sync routes with provider `ticktick`. Initial
enable `config` requires string `project_id`, which is immutable, and optional
boolean `title_prefix` keeps its saved value when omitted on re-enable.
Top-level `status_sync` accepts `one-way` or `two-way`, and the interval
accepts `interval` or `interval_seconds`. Tokens and the API origin are
daemon-owned and rejected in request config. Enable reads the project to
validate its kind, archive state, and permission before a guarded upsert. See
[TickTick sync](../operations/ticktick-sync.md).
