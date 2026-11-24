---
title: Model Context Protocol server
description: Configure Kata's MCP server and use its typed issue, administration, and event tools.
last_edited: 2026-11-21
---

# Model Context Protocol server

Kata ships a native Model Context Protocol (MCP) server for coding agents and
other MCP clients. It speaks stdio and Streamable HTTP, and gives clients
typed access to Kata issue data, administration, automation, and event
workflows.

```sh
# The current workspace's project (default)
kata mcp serve

# One explicit project
kata --workspace /path/to/repository mcp serve
kata --project example-project mcp serve

# A fixed project allowlist
kata mcp serve --projects example-project,shared-project

# Every project visible to the selected daemon
kata mcp serve --all

# Add explicit daemon credential administration
kata mcp serve --all --enable-token-admin
```

Workspace-bound and explicit project scopes require daemon API `0.35.0` or
newer so the daemon can enforce project limits on each request, while `--all`
keeps the daemon API `0.11.0` minimum. If the daemon is older than the
required version, startup fails before resolving projects or serving any tool
calls.

With the default stdio transport, JSON-RPC runs over stdin and stdout, and
Kata writes no status text or logs to stdout. The server fixes its actor at
startup using the usual precedence: `--as`, `KATA_AUTHOR`, `USER`, Git
`user.name`, and finally `anonymous`.

### Server identity

MCP clients receive Kata's website and icon in the server metadata. Because
the icon is an embedded PNG data URI, both stdio and HTTP clients can load it
without an external request, though whether it appears depends on the client.

Both the standalone HTTP listener and the daemon's enabled MCP mount also
serve that same PNG at `/icon.png` and `/favicon.ico`. Those public image
routes accept `GET` and `HEAD` without a bearer token. Listener Host
validation still applies, and `/mcp` still requires its configured MCP bearer
or, on an identity-mode daemon mount in Kata 0.23.0 and later, the caller's
identity token.

### Discover running HTTP listeners

To list HTTP MCP listeners started by this version, run
`kata mcp status --json` or pass `--format=json`. The command reads local
runtime records without starting a server. Each entry reports `transport`,
`url`, `pid`, `backend_url` when known, and `token_path` when the listener
requires a bearer token. Read the token from that private file; the status
output never prints it.

After startup, a listener publishes its actual bound port, including when you
start it on port zero, and removes its record during orderly shutdown. Status
skips records whose process has exited or whose PID now belongs to a different
process, checked through the recorded process identity or start time. Stdio
sessions do not listen on a port and do not appear in this list. An empty JSON
list means no HTTP listeners were found in this application's configured data
directory.

For Unix-socket daemons, `backend_url` shows the real `unix:///path` address
rather than a synthetic HTTP request URL. If an embedding client has already
chosen a local runtime, it can run
`kata mcp serve --all --runtime-dir /path/to/runtime` to talk to that daemon
over stdio. That flag ignores the working directory's server selection and
never starts a missing daemon.

## Transport

By default, `kata mcp serve` uses stdio. Pass `--http` to run a Streamable
HTTP listener instead:

```sh
export KATA_MCP_HTTP_TOKEN='<random bearer token>'
kata mcp serve --all \
  --http 127.0.0.1:8080 \
  --http-token-env KATA_MCP_HTTP_TOKEN
```

Point your MCP client at `http://127.0.0.1:8080/mcp` with the same bearer
token. `--http-token-env` names the environment variable Kata reads, keeping
the token value out of process arguments. Every HTTP listener, including a
loopback listener, requires this inbound bearer. It protects the MCP listener
itself and is separate from the daemon credential Kata uses for its own API
calls.

To read the inbound bearer from an owner-only file instead, pass
`--http-token-file /run/secrets/mcp-token`. That flag takes precedence over
`--http-token-env`, and if the selected file is missing, unreadable, insecure,
or empty, startup fails without falling back. The
[daemon secret-file restrictions](configuration.md#daemon-config) apply here
too. Both token-source flags require `--http`, and the MCP listener never
inherits the daemon's generated owner token automatically.

Loopback listeners require the exact configured Host and reject cross-origin
requests. For a non-loopback listener, you must also pass
`--trust-private-network` and bind a literal non-public IP or wildcard
address:

```sh
kata mcp serve --all \
  --http 0.0.0.0:8080 \
  --http-token-env KATA_MCP_HTTP_TOKEN \
  --trust-private-network
```

Use that plaintext mode only on an operator-trusted private network or behind
an HTTPS reverse proxy; public IPs and DNS hostnames are rejected. The
listener also serves unauthenticated `GET` and `HEAD` probes at `/healthz`,
returning only readiness text with caching disabled.

For a remote gateway, enable the daemon's
[`[mcp]` section](configuration.md#daemon-config) and restart the daemon.
Route `/mcp` through the hub's HTTPS reverse proxy to the daemon, and give the
gateway `https://daemon.example/mcp` along with the separate MCP bearer.
Choose the mount's scope with either `[mcp] projects` or `all_projects = true`;
daemon startup stops if neither or both are set. Listed project names resolve
to UIDs at startup, so renames do not widen scope. If any listed project is
missing or archived, `/mcp` is disabled with HTTP 503 and logs the reason
while REST and the web UI keep serving. Create or restore the project, then
restart the daemon.

An allowlist restricts tools to those projects and rejects project
administration operations, including purge. With `all_projects = true`, the
mount includes future projects and allows project administration. Treat an
all-project MCP bearer as equivalent to the daemon's `[auth]` token, or to
owner-local access when the daemon has no token. The daemon mount never
exposes token administration; for that, run
`kata mcp serve --all --enable-token-admin` with an identity-mode token.
Outside identity mode, the mount uses the actor chosen at startup, or falls
back to `KATA_AUTHOR`, `USER`, Git `user.name`, and then `anonymous` when
`actor` is unset. The mount has no `/healthz` probe or `kata mcp status`
record. `kata daemon status` reports it as `serving` or `unavailable`, and
the daemon's `/api/v1/ping` endpoint works as a container health probe. With a TCP
daemon `listen`, add any container hostname the gateway uses to
`[web].allowed_hosts`. With a Unix-socket daemon and `[web] listen`, the
gateway's Host must match the web listener's bound address or `public_origin`.

### Daemon mount with identity tokens

In Kata 0.23.0 and later, a daemon with `[auth] require_token_identity = true`
accepts each caller's database identity token at `/mcp`. Create the token with
[`kata tokens create`](../operations/remote-daemon.md#identity-tokens) and
configure the MCP client to send it as `Authorization: Bearer <token>` on
every request. The daemon uses that same credential for REST reads and writes
and sets the session's actor from the token's stored actor. Project scope,
token grants, expiration, revocation, and REST authorization all still apply.

```toml
[mcp]
enabled = true
projects = ["example-project"] # or all_projects = true
```

In identity mode, remove `[mcp] token_file` and `token_env`; startup rejects
either setting. `[mcp] actor` and `KATA_MCP_ACTOR` also have no effect here.
Missing, invalid, expired, revoked, and bootstrap tokens receive HTTP 401.
Each session stays bound to the exact token that initialized it. To switch
tokens, initialize a new session, even when both tokens name the same actor. Token administration remains available only through the explicitly
enabled sidecar tools. Kata 0.21.0 and 0.22.0 require the sidecar in identity
mode.

### Separate HTTP process

You can also run a separate MCP process alongside the hub using the settings
in [Client configuration](#client-configuration). Route `/mcp` to that
listener through the HTTPS proxy and give the gateway its separate MCP bearer:

```sh
kata mcp serve --all --http 0.0.0.0:8080 --http-token-file /run/secrets/mcp-token --trust-private-network
```

To limit the sidecar's project scope, replace `--all` with `--projects <names>`.

## Client configuration

A typical stdio client configuration looks like this:

```json
{
  "command": "kata",
  "args": ["mcp", "serve"]
}
```

Pass `--daemon <name>` to target a configured daemon. The usual client
settings apply as well, including `KATA_SERVER`, bearer authentication,
private-network checks, and Unix socket discovery. The server checks the
daemon's `api_schema_version` once at startup and refuses to serve a daemon
older than API `0.35.0`, or older than `0.11.0` when started with `--all` (see
the [HTTP API version history](http-api.md)). If startup rejects the daemon,
upgrade the daemon rather than the MCP client.

When the selected daemon advertises effective auto-start idle shutdown in its
health response, the MCP server sends a marked liveness ping right away and
then waits half of the advertised timeout after each completed attempt. That
marked keepalive uses `GET /api/v1/ping` and runs for the lifetime of the
`kata mcp serve` process in both stdio and `--http` modes. It keeps a quiet
stdio session usable and keeps a Streamable HTTP bridge ready for future
clients without depending on the MCP process's local Kata configuration. When
you no longer want that bridge to keep the daemon resident, stop the MCP
server process.

## Project scope

By default, the server binds to the current workspace's project, so a bare
`kata mcp serve` never gives an MCP peer more authority than the repository it
started in. To widen that boundary, pass an explicit startup flag:

- `--workspace` or `--project` serves one explicit project, and bare issue
  references remain valid in this one-project mode.
- `--projects` resolves the supplied names once and locks in their immutable
  project UIDs. Later renames do not change the boundary, and if a listed
  project is archived or merged away, it drops out without disabling the rest
  of the allowlist.
- `--all` follows the selected daemon's active project catalog for long-lived
  clients that need every project the daemon can reach.

In multi-project mode, issue reads and writes use `project#ref`, and
project-list tools can read all projects in scope. Multi-project results carry
a `projects` list and omit the singular `project` field. Multi-project search
interleaves each project's own ranking (per-project scores are not comparable)
and reports `mode: "mixed"` when projects resolve different effective search
modes. Issue creation and other project-selected writes require an explicit
`project` in multi-project mode. Project administration (create, rename,
metadata, merge, archive, restore, and purge) requires the `--all` daemon-wide
scope; a scoped server can read its projects, but cannot alter or destroy the
catalog it is bound to.

Tool calls cannot change the startup actor or widen the startup scope.
Historical issue references keep working as long as both the lookup route and
the issue's current project remain inside the startup scope, and retained
references also work as relationship and closure evidence targets. Returned
references and delete or purge confirmations always use the current project
and short ID, and reads and writes keep that boundary even if an issue moves
during a call. By default, `kata.move` moves the issue along with its active
descendants and lists every moved issue; pass `single: true` to move only the
selected issue or `dry_run: true` to preview. If any descendant belongs to an
excluded project, the subtree move fails atomically. Against a daemon older
than API `0.35.0`, `kata.move` requires `single: true` instead of silently
moving only the root. The daemon remains authoritative for authentication,
attribution, revision checks, federation trust, claims, and mutation policy.
Because daemon prose can name children, siblings, and prior closes in other
projects, scoped servers replace close-guard refusal messages
(`parent_has_open_children`, `sibling_throttle`, `duplicate_message`) with
scope-safe guidance.

You can pass `--teammate` or set `KATA_TEAMMATE` to configure a default
participant attribution for the process. `kata.comment` and `kata.create` also
accept a per-call `teammate` override, and passing an explicit empty value
suppresses the startup default. Use the per-call value when sibling agents
share one MCP process so one participant's handle cannot leak into another
call. Comment output keeps `author` and `teammate` separate. New issues record
the teammate in initial `metadata.teammate`, while comments store it in their
dedicated field.

The MCP server still starts against its documented baseline daemon. When a
`kata.comment` call includes a nonempty teammate, the server checks for API
`0.18.0` before mutating and returns a tool error explaining the API
requirement if the daemon is older. Calls without a teammate keep their
existing compatibility floor.

With daemon API `0.28.0` or newer, `kata.comment` accepts `reply_to` (a
comment handle or full UID), `kind` (`reply`, `confirm`, `refute`, or
`supersede`, where `reply` is a general response and `confirm` asserts
verification or reproduction), and an optional `force` flag to bypass
duplicate-reply rejection. The required `idempotency_key` includes the
resolved target and kind in its request identity. `kata.show` accepts
`thread`, `inbound`, `kind`, and `since` selectors and returns handles, reply
lines, backlinks, and edit state. Thread results keep their root inside the
requested comment limit and report truncation. Use a comment's `author_filter` as the `inbound` author. A `kata.digest` actor's `actor_filters` contains the exact values to pass to its `actors` filter. Use a `kata.audit_closes` row's `actor_filter` as its `actor` filter. Endpoint identities and
backlinks stay inside the server's fixed startup project UID scope, even after
project moves or renames. See the [CLI reference](cli.md#issue-lifecycle) for
selector behavior, reply validation, and handle syntax.

When a lexical multi-word `kata.search` returns an empty first page, it
includes
`hints: [{"code": "every_word_required", "message": "no issue contains every word; search again with fewer words"}]`,
even when status or label filters are set.

`kata.search` also accepts optional `status: "open"` or `status: "closed"`, or
you can omit it to search both statuses. It combines with labels and the
selected search mode across every project in scope; empty or other status
values are invalid. A status-filtered call checks for API `0.20.0` before
searching and returns a tool error against an older daemon. Searches without a
status filter keep their existing compatibility floor, except for the explicit
single-project lexical pagination described in
[Exhaustive issue discovery](#exhaustive-issue-discovery).

## Exhaustive issue discovery

`kata.list` returns `issues`, `complete`, `truncated`, and `next_cursor` when
another page exists. Its default limit is 20 and its maximum is 100. To fetch
the next page, pass `next_cursor` back as `cursor` with the same filters. The
default `sort: "created"` orders newest creations first, and `sort: "oldest"`
reverses that order. Both break creation-time ties by issue ID. Ordinary edits
do not move issues between pages. These calls require daemon API `0.27.0` or
newer.

Each row includes `scheduled_on_date` and `deadline_on_date` when the issue
carries `scheduled_on` or `deadline_on` metadata, formatted as `YYYY-MM-DD`
calendar dates resolved as described in
[Calendar dates on issue rows](metadata.md#calendar-dates-on-issue-rows).
Daemons older than API `0.36.0` omit these fields. `kata.list` takes no date
filter; compare the row dates yourself.

Pass `include_total: true` to request `total`, which counts all matching
issues in the permitted projects regardless of page limit and cursor, using
the same filters and authorization scope as the page. Set
`priority_unset: true` to match issues with no priority; you cannot combine it
with `priority` or `max_priority`. `priority: null` still means no
exact-priority filter, and `max_priority` excludes issues with no priority.

A single-project `kata.search` with explicit `mode: "lexical"` supports the
same `cursor`, `next_cursor`, and `complete` fields in oldest-creation order
and requires API `0.27.0`. Other search modes and multi-project searches
return ranked, bounded results without continuation. Check `complete` before
treating those results as exhaustive; truncation or degraded retrieval sets it
to `false`. Against daemons older than API `0.27.0`, searches without
continuation keep the legacy extra-row probe to report truncation.

Cursors are opaque and bound to the daemon, project selection, authorization
scope, sort, and filters; changing any of them rejects the cursor. You can
still change the page size and `include_total` between pages. Pages and totals
are live reads rather than a snapshot. Restart a scan after changes to filter
membership, project membership, deletion, or imported creation timestamps.
Stop only when `complete` is `true`.

## Find tools for a task

In Kata 0.23.0 and later, you can call `kata.search_tools` to find an
operation without knowing which section owns it. For example:

```json
{"query":"add a comment to an issue","limit":3}
```

`kata.search_tools` searches tool metadata, while `kata.search` searches issue
data. Discovery works before you load any sections and returns the same ranked
results afterward. It does not call issue APIs, run CLI commands, or activate
public sections.

Each result includes the exact tool `name`, its `summary`, its section
`loader`, applicable `guides`, and sorted top-level `inputs`. Each input
reports its name, JSON `types`, and whether it is unconditionally required; an
empty types list means you need to consult the full schema. Call the returned
loader if present, refresh `tools/list`, and read the tool's `inputSchema` for
full constraints, conditional requirements, and nested inputs. Loaders are
safe to call again. For any returned guide name, call `kata.skills` with that
`name`.

`query` must be non-blank UTF-8 text of at most 512 bytes. Omit `limit` to get
five results, or pass an integer from 1 through 10. Invalid input returns an
MCP tool error, while unmatched, punctuation-only, and stopword-only queries
return an empty `results` array. Results are deterministic and rank an exact
tool name first. Structured output is capped at 32 KiB, with `truncated: true`
when that byte limit stops another result from fitting.

Discovery respects the server's startup scope and omits operations that always
require daemon-wide scope from project-limited servers. Storage tools also
require configured host storage, and token and team tools require explicit
token administration. Finding a tool through discovery does not grant
permission to invoke it.

## Progressive tool catalog

At startup, the catalog exposes read-only section loaders, `kata.skills`, and
(in Kata 0.23.0 and later) `kata.search_tools`. Call the loader you need, then
refresh the tool list when the server sends the standard
`notifications/tools/list_changed` notification. That keeps only the detailed
typed tools for your current task in the model context instead of loading
every tool up front.

The first `tools/list` from the workspace-bound default `kata mcp serve` is
about 23 KB for 20 tools. With every default-available section loaded, it
grows to about 271 KB for 112 tools, with output schemas accounting for about
two thirds. A test keeps these catalogs under 24 KB and 289 KB, including
every section loader. Detailed storage, team, and token tools require startup
flags and fall outside these default catalogs.

| Loader | Detailed tools |
| --- | --- |
| `kata.load_issue_discovery` | `kata.search`, `kata.list`, `kata.show`, `kata.ready`, `kata.next`, `kata.labels`, `kata.graph` |
| `kata.load_issue_mutation` | `kata.create`, `kata.edit`, `kata.comment`, `kata.edit_comment`, `kata.claim`, `kata.set_label`, `kata.set_metadata`, `kata.set_schedule`, `kata.set_deadline`, `kata.move` |
| `kata.load_coordination` | `kata.assign`, `kata.unassign`, `kata.inbox`, `kata.status`, and `kata.event_subscriptions` when the server offers Events |
| `kata.load_cron` | `kata.cron_capabilities`, `kata.cron_new_uid`, `kata.cron_jobs`, `kata.cron_job`, `kata.cron_job_create`, `kata.cron_job_update`, `kata.cron_job_delete`, `kata.cron_job_restore`, `kata.cron_workflows`, `kata.cron_workflow`, `kata.cron_workflow_create`, `kata.cron_workflow_update`, `kata.cron_workflow_delete`, `kata.cron_workflow_restore`, `kata.cron_runs`, `kata.cron_run`, `kata.cron_run_observe` |
| `kata.load_issue_lifecycle` | `kata.close`, `kata.reopen`, `kata.delete`, `kata.restore`, `kata.purge`, `kata.wait`, `kata.audit_closes` |
| `kata.load_leases` | `kata.lease_status`, `kata.lease`, `kata.lease_force_release`, `kata.lease_steal` |
| `kata.load_projects` | `kata.projects`, `kata.project_show`, `kata.project_create`, `kata.project_update`, `kata.project_merge`, `kata.project_remove`, `kata.project_restore`, `kata.project_purge` |
| `kata.load_teams` | `kata.teams`, `kata.team_create`, `kata.team_show`, `kata.team_delete`, `kata.team_member_set`, `kata.project_access_show`, `kata.project_access_set` with the same explicit daemon-wide administration capability |
| `kata.load_tokens` | `kata.tokens`, `kata.token_create`, `kata.token_revoke` when `--enable-token-admin` is set in daemon-wide mode |
| `kata.load_system` | `kata.system` |
| `kata.load_federation` | `kata.federation_bridge_connect`, `kata.federation_bridge_status`, `kata.federation_bridge_disconnect`, `kata.federation_status`, `kata.federation_enrollment_revoke`, `kata.federation_spoke_name`, `kata.federation_rebind`, `kata.federation_leave`, `kata.federation_quarantine`, `kata.federation_root_key_repin` |
| `kata.load_sync` | `kata.sync_status`, `kata.sync_update`, `kata.sync_once` |
| `kata.load_recurrence` | `kata.recurrences`, `kata.recurrence_update`, `kata.recurrence_delete` |
| `kata.load_activity` | `kata.digest`, `kata.events` |
| `kata.load_import` | `kata.import_issues` |
| `kata.load_external_roots` | `kata.connectors`, `kata.connector_fields`, `kata.connector_field_map`, `kata.connector_field_unmap`, `kata.bridge_bind`, `kata.bridge_show`, `kata.bridge_reconcile`, `kata.bridge_pause`, `kata.bridge_resume`, `kata.bridge_resolve_field`, `kata.bridge_resolve_comment`, `kata.bridge_unbind` |
| `kata.load_storage` | `kata.storage_export`, `kata.storage_import` when host storage is enabled |
| `kata.load_docs` | `kata.search_docs`, `kata.read_doc` |

`kata.connectors`, `kata.connector_fields`, `kata.connector_field_map`,
`kata.connector_field_unmap`, and `kata.bridge_bind` require the `--all`
daemon-wide scope. The remaining bridge tools work on already-bound issues
inside the startup project scope.

`kata.inbox` lists open issues that carry a request for one recipient's
attention, the same requests `kata inbox` shows, and never clears them. It
reads up to `limit` candidate issues per page and returns `next_cursor` when
more candidate issues remain, with `truncated` indicating that more candidate
issues remain. Pass `next_cursor` back as `cursor` and follow it until it is
absent, including through empty pages. Pass a request's `from_address` to `kata notify --to`. `kata.status` reports an issue's owner,
hold, and lease along with the actor the server writes as. `kata.unassign`
accepts `expected_owner` so a handoff clears ownership only while that actor
still holds the issue. `kata.project_show` returns one project's metadata and
workspace aliases.

`kata.search_docs` and `kata.read_doc` search and read the user documentation
bundled into the `kata` binary, covering the overview, getting-started, guide,
workflow, reference, and operations pages. Search returns section IDs and
short excerpts, and `kata.read_doc` returns one section's Markdown. Both work
offline and reflect the documentation of the running build.

Loaders are idempotent, and a loader reports `available=false` when its
optional startup dependency is absent. Loaded tools keep their own input and
output schemas and safety annotations; Kata never combines unrelated actions
into a generic command tool.

Every tool uses structured input and output. List-like results, including
`kata.audit_closes` rows, default to 20 and cap at 100. `kata.audit_closes`
pages with an opaque `cursor` (returning `truncated` and `next_cursor`)
validated against the close history below it, so shared timestamps cannot skip
or repeat rows and a project merge or issue purge during pagination fails the
page with a restart error instead of silently skewing it. `kata.show` returns
at most 100 comments. Create and comment calls require idempotency keys, while
`kata.close` accepts an optional `idempotency_key` for safe retries and an
optional `revision` for a conditional close; an exact retry returns the
original close event in the mutation output.

`kata.token_create`, recurrence creation, `kata.storage_import`, `kata.lease`,
and `kata.sync_once` are annotated non-idempotent. The first two mint a new
record on every identical retry, a forced storage import replaces the target
again (with a fresh instance identity when `new_instance` is set), a lease
renewal extends the expiry on every call, and each sync pass re-imports from
the provider. `kata.cron_new_uid` is read-only and non-idempotent, returning a
fresh, unreserved UID on each call. `kata.lease_steal` tries to acquire first
and only force-releases a holder that denies that acquire, so a retry after a
lost response keeps a lease the startup principal already holds instead of
releasing and re-acquiring it. Destructive tools keep Kata's exact
confirmation and revision contracts. `kata.delete` and `kata.purge` confirm
against `project#short_id` and then address the daemon by the issue's
immutable UID.

Recurrence patch and delete calls require the current positive `revision` and
send it as `If-Match`. Create calls do not use a revision.

`kata.create` supports `force_new`. `kata.claim` supports `force`,
`if_unowned`, and an optional `ttl_seconds` from 60 through 86400; a timed
claim renews when the same actor repeats it, and the result includes the
assignment expiry and previous owner when present. When a timed claim on an open issue expires while `work.attention` is `ok`, Kata sets it to `needs-human` with the message `claim expired without hand-off`. Renew before expiry by calling `kata.claim` again with `ttl_seconds`, without `if_unowned`. A new claim leaves the attention state and message unchanged. `kata.edit` supports field,
owner, priority, relationship, scheduling, and generic metadata changes. Keep
issue-field or relationship changes separate from metadata changes in distinct
`kata.edit` calls so one failed request cannot leave a partial edit.

`kata.create` and `kata.set_label` reject labels `p0` through `p4` and labels beginning with `priority:`. Set the priority field with `kata.create` or `kata.edit`.

### Startup sections

If a gateway needs typed tools in its first `tools/list`, configure the
daemon-mounted endpoint before starting it:

```toml
[mcp]
startup_sections = ["issue_discovery", "issue_mutation", "coordination", "issue_lifecycle", "projects", "docs"]
```

List section IDs such as `issue_discovery` rather than loader tool names such
as `kata.load_issue_discovery`. The valid IDs are `activity`, `coordination`,
`cron`, `docs`, `external_roots`, `federation`, `import`, `issue_discovery`,
`issue_lifecycle`, `issue_mutation`, `leases`, `projects`, `recurrence`,
`storage`, `sync`, `system`, `teams`, and `tokens`. Each ID selects the
detailed tools in its loader row above.

Only available sections can load at startup. `storage` requires host storage
opt-in, and `teams` and `tokens` require explicit token administration with
daemon-wide scope. These three sections are unavailable on the daemon mount. If you request an unknown, duplicate, or unavailable ID, `/mcp` is
disabled with HTTP 503 and logs the reason while the daemon's REST API and web
UI keep serving.

`KATA_MCP_STARTUP_SECTIONS` overrides the TOML array with comma-separated,
trimmed IDs, while an empty environment value preserves TOML. After changing
either setting, restart the daemon and then reconnect the gateway or refresh
its catalog. Every fresh server publishes the configured tools before any
loader call with no priming hook required. All loaders remain available and
idempotent, so clients can still load other sections progressively. To keep
the default loader-only initial catalog, omit the setting or set
`startup_sections = []`. These settings apply only to the daemon mount;
`kata mcp serve` always uses progressive loading.

## Native cron through MCP

Load `kata.load_cron` for job and workflow lifecycle tools, live capability
discovery, and attributed run evidence. These tools manage cron job
definitions; to manage RRULE issue templates, load `kata.load_recurrence`
instead.

| Purpose | Tools |
| --- | --- |
| Discovery and identity | `kata.cron_capabilities`, `kata.cron_new_uid` |
| Jobs | `kata.cron_jobs`, `kata.cron_job`, `kata.cron_job_create`, `kata.cron_job_update`, `kata.cron_job_delete`, `kata.cron_job_restore` |
| Workflows | `kata.cron_workflows`, `kata.cron_workflow`, `kata.cron_workflow_create`, `kata.cron_workflow_update`, `kata.cron_workflow_delete`, `kata.cron_workflow_restore` |
| Run evidence | `kata.cron_runs`, `kata.cron_run`, `kata.cron_run_observe` |

Every cron call uses the MCP server's configured daemon credentials, project
scope, and accountable actor. In multi-project modes, pass `project`
explicitly. Run observations accept the usual per-call `teammate` override,
and an empty value suppresses the startup default without changing the actor.
Issue-scoped tokens do not gain project-level cron authority.

Before creating a job or workflow, call `kata.cron_new_uid` and keep the
returned `uid`. That helper creates and reserves nothing and does not bind the
UID to a project. Pass that caller-retained identity to the create call
without an expected event UID. If a create response is lost, read the same UID
with `kata.cron_job` or `kata.cron_workflow`; repeating the create preserves
the daemon's conflict instead of turning into an update.

Update, delete, and restore calls require the current `expected_event_uid`.
After a conflict, read the current definition and decide whether to retry; MCP
never fetches a new compare-and-set value on your behalf. Delete performs the
existing reversible cron tombstone operation. Workflow-in-use checks and native
parent bindings are preserved, and typed definitions and opaque option objects
keep exact JSON numbers.

`kata.cron_runs` defaults to 20 records, accepts `limit` from 1 through 100,
and returns `next_before_uid` to pass back as `before_uid` for the next page.
Use `job_uid` to filter history, and see
[Run list page size](cron.md#run-list-page-size) for the HTTP, CLI, and TUI
defaults. `kata.cron_run_observe` requires a caller-retained run UID,
`expected_revision` (`0` for new evidence), a job or workflow reference with
its definition event UID, status, and a versioned summary. Identical retries
return `replayed: true`, while distinct run UIDs for one scheduled occurrence
remain separate observations. Recording evidence does not launch execution,
claim an issue, or change its status.

Enabled native `notify` jobs activate the existing daemon delivery, while
execution jobs and workflows require an installed external adapter. Call
`kata.cron_capabilities` to see what the selected daemon actually advertises;
permission denial, unsupported capability, and temporary unavailability stay
distinct errors. See [Native cron](cron.md) for job definitions and adapters,
its [MCP recurring inbox example](cron.md#mcp-recurring-inbox-example), and
[MCP Events](../operations/mcp-events.md) for signed webhook pings.

## Agent guides (`kata.skills`)

`kata.skills` is always present in the initial tool catalog. It is not a
section loader, and you do not need to call a loader first to list or read a
guide. Call it with no arguments to list the catalog, or pass a guide name:

```json
{"name":"workflow"}
```

In Kata 0.23.0 and later, the available guide names are `setup`, `quickstart`,
`workflow`, `issues`, `claiming`, `closing`, `teammates`, `scheduling`, and
`events`. Read `workflow` at the start of a session and before your first
write. `quickstart` walks through search, creation, ownership, findings, and
completion using native JSON tool calls. The guides describe the connected
server's scope, section loaders, required idempotency keys, and typed close
evidence without requiring shell commands. The Markdown body comes back once
as text content, while structured content holds the guide's name, title, and
summary. `setup` also returns `managed_block`.

### Install MCP-only instructions

An assistant with a connected Kata server and its own local file editor can
request the exact repository instruction block by passing this input to
`kata.skills`:

```json
{"name":"setup"}
```

The server returns the content and a bounded edit plan. It never reads or
writes client files, and it accepts no filesystem path or file content. This
sets up instructions for an already authorized connection without creating a
project or binding a workspace. If the assistant has no local file tool, it
presents the returned text as pending setup.

The caller inspects local `AGENTS.md` and `CLAUDE.md`, refuses a symlinked
`AGENTS.md`, skips a symlinked `CLAUDE.md`, and updates selected regular
files, creating `AGENTS.md` if neither file exists. It replaces only one
unambiguous Kata managed span, or appends the block when no markers exist.
Because both instruction modes share markers, switching modes preserves all
unrelated content. Malformed, reversed, orphaned, or duplicate markers require
conflict resolution.

The edit plan preserves file permissions, compares the preimage before
writing, and checks readback afterward. Complete Beads integration blocks
leave the original file untouched and write an exclusive-create
`.kata-proposed` sidecar without overwriting existing sidecars. If the local
editor cannot enforce those rules, the caller returns a proposal; receiving
content alone is never reported as a successful file installation.

For local CLI installation and switching back to CLI guidance, see
[workspace initialization](cli.md#workspace-initialization).

`kata.skills` serves a fixed catalog of agent workflow guidance and cannot
read arbitrary files or other skills. For reference documentation, load
`kata.load_docs` and use `kata.search_docs` and `kata.read_doc`.

Some workflows have no equivalent MCP call in this version. Sending or
clearing notification requests requires an operator with CLI access, and
`kata.wait` waits on issue status only. For an attention wait, poll
`kata.show` for every watched ref with your own deadline; a closed issue
completes first, then `work.attention` of `needs-human` or `stuck`, and report
timeouts distinctly. `kata.inbox` lists notifications on open issues and does
not replace an attention wait. In multi-project scope, use `project#ref` for
existing-issue calls and pass `project` only where the tool's schema includes
it.

## Scheduling and someday

`kata.create` and `kata.edit` include first-class `scheduled_on` and
`timezone` fields. Accepted `scheduled_on` forms are:

- `YYYY-MM-DD`
- `YYYY-MM-DDTHH:MM`
- `YYYY-MM-DDTHH:MM:SS`
- UTC RFC 3339, such as `2026-09-01T22:00:00Z`

Civil date and time values use the supplied IANA timezone, and numeric offsets
are rejected. To clear either field during an edit, pass `clear_scheduled_on`
or `clear_timezone`.

`kata.set_schedule` writes the reserved `scheduled_on` metadata key with
either `schedule` to set a value or `clear_schedule: true` to remove it.
`kata.set_deadline` writes `deadline_on` with either `deadline` or
`clear_deadline: true`. Each pair is mutually exclusive. Both tools accept the
same date, local date-time, and UTC-instant forms as `scheduled_on`, along
with an optional `revision` that adds the same conditional-write guard as
`kata.set_metadata`.

Generic metadata supports the native parking marker:

```json
{"metadata":{"someday":true}}
```

To remove it, set the key to JSON `null` rather than writing `someday=false`.
The same null-removal rule applies to `kata.set_metadata` and other generic
metadata patches.

## Team and project access administration

To manage teams and project access, start the server with
`--all --enable-token-admin`, then load `kata.load_teams`. That startup
capability makes the tools available, while the daemon still checks the
credential's owner authority on every call. An ordinary account cannot gain
administrative access through an MCP actor label.

Create a team with `kata.team_create` and `name`, and select existing teams by
`team_uid` for `kata.team_show` or `kata.team_delete`. Manage membership with
`kata.team_member_set` using `team_uid`, `member_actor`, and `present`;
`member_actor` is the account being enrolled, separate from the authenticated
actor making the change. Deleting a team keeps its projects restricted.

Read policy by project name with `kata.project_access_show`, and update it
with `kata.project_access_set` using `project`, `visibility` (`all` or
`teams`), and `team_uids`. Pass an optional `revision` to guard against a
stale edit; omitting it captures the current revision before the write, and a
concurrent change returns a conflict. See
[team visibility](../operations/federation.md#team-visibility) for the
authorization rules.

## Read relay attribution

Issue and comment results retain `author` and `teammate` alongside
`accountable_actor`, `source_actor`, `authority_uid`, and `verification` when
available. The accountable actor identifies the authenticated account, while
source fields keep the original attribution across relay hops. Keep `pending`,
`verified`, and `legacy` verification states distinct. Source labels never
grant access, and an omitted verification field makes no verified claim.

## Events, tokens, and federation

`kata.events` supports immediate `poll` and bounded `wait` modes and returns a
resume cursor. Wait mode uses the daemon's SSE stream wherever one stream can
enforce the selected scope, while fixed multi-project allowlists use bounded
scoped polling. When a `sync.reset_required` result arrives, it returns
`reset_after_id`, advances `next_after_id` to that reset cursor, and returns
no stale events.

Unscoped `kata.token_create` returns the plaintext token once; `kata.tokens`,
status tools, errors, and later calls never expose that secret or its hash.
Token administration requires both the `--all` daemon-wide startup scope and
the explicit `--enable-token-admin` startup capability. Default workspace
servers, one-project servers, and fixed-allowlist servers cannot read, create,
or revoke global daemon tokens.

You can pass `team_uids` to `kata.token_create` to commit initial account
membership alongside token issuance for both ordinary and scoped credentials.
If any team is invalid, the call leaves neither a new token nor partial
membership.

In daemon-wide token-admin mode, `kata.token_create` also accepts `issue`,
`expires_in_seconds`, and `token_file` together to mint an `issue_subtree`
credential and write its plaintext once to a new owner-only file on the MCP
host. The tool withholds the plaintext from its result and attempts immediate
revocation if delivery or response validation fails. A scoped credential
cannot mint another credential; instead, it creates delegated work by creating
children under an accessible parent. See the
[worker provisioning example](../operations/remote-daemon.md#identity-tokens)
for consuming the file and the
[scope guide](../design/issue-scoped-credentials.md) for membership and
revocation rules.

Project bridges require Kata 0.19.0 or later. Follow
[Share projects between hubs](../guide/project-relays.md) for setup and target
selection, and check the
[operator reference](../operations/federation.md#connect-a-project-through-the-daemon-catalog)
for credential, URL, and daemon-side source requirements.

MCP can connect one project through a catalog account without ever handling
enrollment secrets. It has no tool to issue an enrollment from this daemon,
accept an enrollment token, or run a full `kata federation join`; the bridge's
enrollment token stays inside the daemon's credential store. Pass `project`,
`hub_catalog`, and `hub_project` to `kata.federation_bridge_connect`, and the
local daemon uses that catalog account to enroll the narrow bridge. Set
`preflight=true` to preview without enrollment and check cross-project issue
links (`hub_links_checked` is `false` when the hub predates that check). Set
`serve_downstream=true` only when this daemon also relays the project to its
own spokes; it defaults to `false`. Pass `signing_key_id` with
`signing_key_env` or an absolute `signing_key_file` to select the daemon-side
signing source that a required-signing hub needs. The local actor comes from
MCP startup state.

To inspect one project's recorded bridge state, including a pending
enrollment, call `kata.federation_bridge_status`. To detach while keeping
local data and retaining retry state after interrupted cleanup, call
`kata.federation_bridge_disconnect` with `project` and an optional
`preflight`. Pass `local_only=true` to detach from a hub that is gone for good
without revoking its grant; the result's `warning` notes that the hub operator
must revoke it. All of these bridge tools require `--all` and daemon-owner
authority at the API. They never return token values or credential inventory
and accept no caller-supplied actor or token override.

`kata.federation_root_key_repin` takes `project`, optional `preflight`,
optional `previous_key_file`, and optional `strand_relays` to call the
[root key repin endpoint](../operations/federation.md#root-signing-key). It
requires `--all` and daemon-owner authority at the API.

`kata.federation_status` lists secret-free enrollment records, including each
instance's hub-local `spoke_name` when set, and
`kata.federation_enrollment_revoke` can revoke one.

`kata.federation_spoke_name` takes `spoke_instance_uid` and either `name` or
`clear: true` to set or clear that instance's
[hub-local name](../operations/federation.md#operator-commands). Because a
name belongs to the instance across every project, this tool requires `--all`
and daemon-owner authority at the API.

Because enabling issue synchronization selects which external repository the
daemon's configured GitHub credentials read, `kata.sync_update` with
`action: "enable"` requires the `--all` daemon-wide scope. Scoped servers can
still disable an operator-configured binding and run `kata.sync_once` against
it.

Federation leave exposes `preflight`, `prepare`, `abort`, and `commit` phases
so an operator can keep the normal revoke-before-local-teardown order, and the
phase is required. Pass `prepare` a random `preparation_id` (it returns that
ID, or one it picked if you omit it). If the hub revoke fails after `prepare`
or the `prepare` result is lost, run `abort` with that `preparation_id` to
release the prepared leave so rebind and join work again. Retrying `prepare`
with the same `preparation_id` holds the same preparation. Once a reservation
is marked for leave or local teardown has begun, the daemon refuses `abort`
with `federation_leave_started`; finish that leave instead. The `commit` phase
requires `COMMIT FEDERATION LEAVE <project>`. The `archive` disposition and
`kata.federation_rebind`, which routes the replica's enrollment token to the
selected catalog origin, require the `--all` daemon-wide scope. Quarantine
retry and skip require `RETRY FEDERATION BATCH <id>` or
`SKIP FEDERATION BATCH <id>`.

## Host-storage opt-in

JSONL storage access is off by default. Enable it only on the daemon host:

```sh
kata mcp serve --all \
  --storage-root /srv/kata/exchange \
  --storage-target restore=restore.db
```

`kata.storage_export` also requires the `--all` daemon-wide scope even when
you supply a `project` filter. A project-filtered JSONL export still contains
cross-project link rows and unredacted event payload references that scoped
reads deliberately hide.

`--storage-target alias=path-or-DSN` is repeatable. Tool calls pick an alias
and cannot submit a database path or DSN. Artifact paths are relative to the
storage root, and absolute paths, `..`, symlink traversal, directories, and
special files are rejected. Storage operations stay anchored to an open root
descriptor so a directory symlink swap cannot redirect them outside the
configured root. SQLite target paths are also contained by the root.

Export opens the active storage read-only and atomically installs the JSONL
artifact. It cannot write to the active SQLite database, its sidecar files, or
any configured SQLite import target as an artifact path. Replacing an existing
non-storage artifact requires `force=true` and
`OVERWRITE ARTIFACT <artifact>`. Export includes deleted records unless you
explicitly pass `include_deleted=false`. Import refuses the active daemon
storage, and active SQLite sidecars cannot be configured as restore targets.
Restored SQLite files use owner-only permissions. Replacing an existing SQLite
target requires `force=true` and `REPLACE STORAGE <alias>`. Force replacement
of PostgreSQL storage is not available through MCP.

## Protocol contract

Kata delegates protocol negotiation to the official Go MCP SDK. Stateless
clients can use `server/discover`, session clients can use `initialize` and
`notifications/initialized`, and stdio accepts both. The standalone and
daemon-mounted HTTP endpoints route MCP 2026-07-28 requests to a stateless
JSON server with Events, while legacy clients keep sessions and tool-list
changes without Events. JSON-RPC batches are rejected, and each compact JSON
message is limited to 8 MiB.

Kata advertises tools, including tool-list changes, and the webhook Events
capability described in [Native inbox events](#native-inbox-events). It does
not advertise prompts, resources, roots, logging, sampling, subscriptions, or
server-to-client requests. Discovery uses a five-minute private cache hint.
Tool execution allows 20 starts per second, a burst of 20, and at most eight
concurrent daemon calls. The standalone and static daemon mount share limits
across protocol paths. Identity mounts keep per-session limits for legacy
clients, while modern requests share limits by exact token ID independently of
other tokens. Modern clients using the same token also share loaded
tool-section selections until the token expires or the daemon restarts.

Host process control stays outside MCP. Daemon lifecycle, the TUI and Web UI,
install or update, database migrations and cutovers, and raw internal
replication endpoints remain CLI or operator workflows.

## Native inbox events

The daemon owns MCP Events. Enable it with `[mcp_events] enabled = true` in
the daemon configuration (see [MCP Events](configuration.md#mcp-events)). Once
enabled, `kata mcp serve` forwards `events/list`, `events/subscribe`, and
`events/unsubscribe` to the daemon and keeps no Events state of its own, with
no separate Events flag. The daemon-mounted `/mcp` endpoint offers the same
Events methods with either static or identity tokens once both `[mcp]` and
`[mcp_events]` are enabled and the daemon starts or restarts. Identity
requests use the caller's exact token as the subscription owner, while static
mounts use the daemon's backend credential. The daemon owns all subscription
state.

A forwarder advertises Events on stdio and on the MCP 2026-07-28 HTTP path. It
does not advertise or serve Events on legacy-session HTTP, where `events/*`
receives the SDK's plain HTTP 400 "not handled" response.

`kata.inbox.changed` accepts an exact `for` recipient along with optional
`project_uids` and `issue_uid` filters. `project_uids` must be non-empty,
sorted ascending, and free of duplicates. Every forwarder mode except `--all`
(the default workspace scope, `--project`, and `--projects`) requires an
explicit `project_uids` list inside the projects it serves. Delivery is a
signed HTTPS webhook ping that names only the recipient. The event type does
not replay; a supplied cursor is ignored and results carry `cursor: null`.
Receivers deduplicate by `webhook-id`, exhaust `kata.inbox` pages, and read
the issue. A ping never claims, clears, or completes work.

On the HTTP path, `events/subscribe` and `events/unsubscribe` require an
`Mcp-Name` header equal to `params.name`. A missing or mismatched header
returns HTTP 400 and `-32020`, and a body over 1 MiB returns 413. The
capability is `extensions["io.modelcontextprotocol/events"]` plus a top-level
`events` alias.

The [MCP Events operator guide](../operations/mcp-events.md) owns subscription
ownership, project selection, lifetimes, what triggers a ping, delivery and
retry rules, terminated notices, and key handling. `kata.event_subscriptions`,
available through `kata.load_coordination`, shows the caller's subscriptions
and their delivery health. For a narrowed forwarder, it lists only
subscriptions inside that forwarder's projects.
