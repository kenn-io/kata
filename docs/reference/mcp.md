---
title: Model Context Protocol server
description: Configure Kata's MCP server and use its typed issue, administration, and event tools.
last_edited: 2026-10-31
---

# Model Context Protocol server

Kata includes a native Model Context Protocol (MCP) server for coding agents
and other MCP clients. It supports stdio and Streamable HTTP transport and
gives typed access to Kata issue data, administration, automation, and event
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
newer so the daemon enforces their project limits inside each request.
`--all` retains the daemon API `0.11.0` minimum. Startup rejects an older
daemon before resolving projects or serving tool calls.

With the default stdio transport, JSON-RPC uses stdin and stdout. Kata writes
no status text or logs to stdout. The actor is fixed when the process starts
and uses the normal precedence: `--as`, `KATA_AUTHOR`, `USER`, Git `user.name`,
then `anonymous`.

### Discover running HTTP listeners

Run `kata mcp status --json` to list HTTP MCP listeners started by this
version. `--format=json` selects the same output. The command reads local
runtime records without starting a server.
Each entry includes `transport`, `url`, `pid`, `backend_url` when known,
and `token_path` when the listener requires a bearer token. Read the token
from that private file; the status output does not print it.

The listener publishes its actual bound port after startup, including when
started with port zero, and removes its record on orderly shutdown. Status
omits records whose process has exited or whose PID belongs to a different
process, using the recorded process identity or start time. Stdio sessions are
not listening endpoints and do not appear. An empty JSON list means no HTTP
listeners were found in this application's configured data directory.

For Unix-socket daemons, `backend_url` is the actual `unix:///path` address,
not the synthetic HTTP request URL. An embedding client that has already
selected a local runtime can use `kata mcp serve --all --runtime-dir
/path/to/runtime` to reach that daemon over stdio. This option ignores the
working directory's server selection and never starts a missing daemon.

## Transport

The default transport is stdio. Use `--http` to run a Streamable HTTP listener
instead:

```sh
export KATA_MCP_HTTP_TOKEN='<random bearer token>'
kata mcp serve --all \
  --http 127.0.0.1:8080 \
  --http-token-env KATA_MCP_HTTP_TOKEN
```

Configure the MCP client with `http://127.0.0.1:8080/mcp` and the same bearer
token. `--http-token-env` names the environment variable that the Kata process
reads; the token value does not appear in the process arguments. Every HTTP
listener requires this inbound bearer, including a loopback listener. This
credential protects the MCP listener and is separate from the daemon
credential that the Kata process uses for its own API calls.

Use `--http-token-file /run/secrets/mcp-token` to read the inbound bearer from
an owner-only file instead. It wins over `--http-token-env`; a missing,
unreadable, insecure, or empty selected file fails without falling back. The
[daemon secret-file restrictions](configuration.md#daemon-config) apply.
Both token-source flags require `--http`. The MCP listener does not inherit the
daemon's generated owner token automatically.

Loopback listeners require the exact configured Host and reject cross-origin
requests. A non-loopback listener additionally requires
`--trust-private-network` and a literal non-public IP or wildcard bind:

```sh
kata mcp serve --all \
  --http 0.0.0.0:8080 \
  --http-token-env KATA_MCP_HTTP_TOKEN \
  --trust-private-network
```

Use that plaintext mode only on an operator-trusted private network or behind
an HTTPS reverse proxy. Public IPs and DNS hostnames are rejected. The
listener also serves unauthenticated `GET` and `HEAD` requests at `/healthz`;
the probe returns only readiness text and disables caching.

For a remote gateway, enable the daemon's [`[mcp]` section](configuration.md#daemon-config) and restart.
Route `/mcp` through the hub's HTTPS reverse proxy to the daemon. Give the gateway `https://daemon.example/mcp` and the separate MCP bearer.
Choose the mount's scope with `[mcp] projects` or `all_projects = true`; daemon startup stops when neither or both are set. Listed names resolve to UIDs at startup, so renames don't widen scope. Missing or archived listed projects disable `/mcp` (503, reason logged); REST and the web UI keep serving. Create or restore the project, then restart.
An allowlist limits tools to those projects and rejects project administration operations, including purge. With `all_projects = true`, the mount includes future projects and permits project administration.
Treat an all-project MCP bearer as equivalent to the daemon's `[auth]` token, or to owner-local access when the daemon has no token.
The mount never exposes token administration; use `kata mcp serve --all --enable-token-admin` with an identity-mode token.
The mount uses the actor chosen at startup and has no `/healthz` probe or `kata mcp status` record; `kata daemon status` reports it as `serving` or `unavailable`.
The daemon's `/api/v1/ping` endpoint works as a container health probe.
When `actor` is unset, it defaults to `KATA_AUTHOR`, `USER`, or Git `user.name`, then `anonymous`.
With a TCP daemon `listen`, a gateway using a container hostname needs that authority in `[web].allowed_hosts`.
With a Unix-socket daemon and `[web] listen`, the gateway's Host must match the web listener's bound address or `public_origin`.

You can also run a separate MCP process alongside the hub using [Client configuration](#client-configuration).
Route `/mcp` to this listener through the HTTPS proxy and give the gateway its separate MCP bearer:

```sh
kata mcp serve --all --http 0.0.0.0:8080 --http-token-file /run/secrets/mcp-token --trust-private-network
```

Use `--projects <names>` in place of `--all` to limit the sidecar's project scope.

## Client configuration

A typical stdio client command has this shape:

```json
{
  "command": "kata",
  "args": ["mcp", "serve"]
}
```

Use `--daemon <name>` to select a configured daemon. The normal client
settings also apply, including `KATA_SERVER`, bearer authentication,
private-network checks, and Unix socket discovery. The server checks the
daemon's `api_schema_version` once at startup and refuses to serve a daemon
older than API `0.11.0` (see the [HTTP API version history](http-api.md));
upgrade the daemon rather than the MCP client in that case.

When the selected daemon advertises effective auto-start idle shutdown in its
health response, the MCP server sends a marked liveness ping immediately and
then waits half of the advertised timeout after each completed attempt. The
marked keepalive uses `GET /api/v1/ping` and runs for the lifetime of the
`kata mcp serve` process in both stdio and `--http` modes. This keeps a quiet
stdio session usable and keeps a
streamable-HTTP bridge ready for future clients without relying on the MCP
process's local Kata configuration.
Stop the MCP server process when that bridge should no longer keep the selected
daemon resident.

## Project scope

The server binds the current workspace's project by default, so a bare
`kata mcp serve` never grants an MCP peer more authority than the repository
it was launched for. Broader boundaries are explicit startup choices:

- `--workspace` or `--project` serves one explicit project. Bare issue
  references remain valid in one-project mode.
- `--projects` resolves the supplied names once and keeps their immutable
  project UIDs. A later rename does not change the boundary, and a member
  that is later archived or merged away drops out without disabling the
  remaining allowlist.
- `--all` follows the selected daemon's active project catalog for
  long-lived clients that need every project the daemon can access.

Multi-project issue reads and writes use `project#ref`. Project-list tools can
read all projects in scope. Multi-project results carry a `projects` list and
omit the singular `project` field; multi-project search interleaves each
project's own ranking (per-project scores are not comparable) and reports
`mode: "mixed"` when projects resolve different effective search modes. Issue creation and other project-selected writes
require an explicit `project` in multi-project mode. Project administration
(create, rename, metadata, merge, archive, restore, and purge) requires the
`--all` daemon-wide scope; a scoped server can read its projects but
cannot alter or destroy the catalog it was bound to.

Tool calls cannot change the startup actor or expand the startup scope.
Historical issue references remain usable while both the lookup route and the
issue's current project are visible within the startup scope. Retained
references also work as relationship and closure evidence targets. Returned
references and delete/purge confirmations use the current project and short ID.
Reads and writes retain that boundary if
an issue moves during a call. `kata.move` moves the issue and its active
descendants by default and lists every moved issue; pass `single: true` to move
only the selected issue or `dry_run: true` to preview. A subtree move is
rejected atomically if any descendant comes from an excluded project. Against a
daemon older than API `0.35.0`, `kata.move` requires `single: true` instead of
silently moving only the root. The daemon remains authoritative for
authentication, attribution, revision checks, federation trust, claims, and
mutation policy. Scoped servers replace
close-guard refusal messages (`parent_has_open_children`,
`sibling_throttle`, `duplicate_message`) with scope-safe guidance because
the daemon prose can name children, siblings, and prior closes in other
projects.

The process may take `--teammate` or `KATA_TEAMMATE` as its default
participant attribution. `kata.comment` and `kata.create` also accept an
optional per-call `teammate`; an explicit empty value suppresses the startup
default. Use the per-call value when siblings share one MCP process so one
participant's handle cannot leak into another call. Comment output keeps
`author` and `teammate` separate. New issues store the teammate in initial
`metadata.teammate`; comments store it in their dedicated field.

The MCP server still starts against its documented baseline daemon. A
`kata.comment` call with a nonempty teammate checks for API 0.18.0 before
mutating and returns a tool error explaining the API requirement against an older daemon.
Teammate-free calls retain their existing compatibility floor.

With daemon API `0.28.0` or newer, `kata.comment` accepts `reply_to` (a comment
handle or full UID), `kind` (`reply`, `confirm`, `refute`, or `supersede` (reply is a general response; confirm asserts verification or reproduction)),
and optional `force` to bypass duplicate-reply rejection. The required
`idempotency_key` includes the resolved target and kind in its request identity.
`kata.show` accepts `thread`, `inbound`, `kind`, and `since` selectors and
returns handles, reply lines, backlinks, and edit state. Thread results retain
their root within the requested comment limit and report truncation.
Endpoint identities and backlinks remain within the server's fixed startup
project UID scope, including after project moves or renames. See the
[CLI reference](cli.md#issue-lifecycle) for selector behavior, reply validation,
and handle syntax.

`kata.search` accepts optional `status: "open"` or `status: "closed"`; omit
it to search both statuses. It combines with labels and the selected search
mode across every project in scope. Empty and other status values are invalid.
A status-filtered call checks for API 0.20.0 before searching and returns a
tool error for an older daemon. Other searches without status retain their existing
compatibility floor, except the explicit single-project lexical pagination below.

## Exhaustive issue discovery

`kata.list` returns `issues`, `complete`, `truncated`, and `next_cursor` when
another page exists. Its default limit is 20 and its maximum is 100. Pass
`next_cursor` back as `cursor` with the same filters to continue. The default
`sort: "created"` orders newest creation first; `sort: "oldest"` reverses it.
Both use the issue ID to break creation-time ties. Ordinary edits do not move
issues between pages. These calls require daemon API 0.27.0 or newer.

Each row carries `scheduled_on_date` and `deadline_on_date` when the issue has
`scheduled_on` or `deadline_on` metadata: `YYYY-MM-DD` calendar dates resolved
as described in [Calendar dates on issue
rows](metadata.md#calendar-dates-on-issue-rows). A daemon older than API
`0.36.0` omits them. `kata.list` takes no date filter; compare the row dates
yourself.

Set `include_total: true` to request `total`, the count of all matching issues
in the permitted projects, independent of the page limit and cursor. Counts
use the same filters and authorization scope as the page. `priority_unset: true`
selects issues with no priority and cannot be combined with `priority` or
`max_priority`. `priority: null` still means no exact-priority filter.
`max_priority` excludes issues with no priority.

A single-project `kata.search` with explicit `mode: "lexical"` supports the
same `cursor`, `next_cursor`, and `complete` fields in oldest-creation order,
and requires API 0.27.0. Other search modes and multi-project searches use
ranked, bounded results without continuation. Check `complete` before treating
those results as exhaustive; truncation or degraded retrieval makes it false.
On daemons older than API 0.27.0, searches without continuation retain the
legacy extra-row probe to report truncation.

Cursors are opaque and bound to the daemon, project selection, authorization
scope, sort, and filters. Changing these rejects a cursor. The page size and
`include_total` may change. Pages and totals are live reads, not a snapshot:
restart a scan after changes to filter membership, project membership, deletion,
or imported creation timestamps. Stop only when `complete` is true.

## Progressive tool catalog

By default, the initial catalog exposes read-only section loaders. Call the applicable
loader, then refresh the tool list when the server sends the standard
`notifications/tools/list_changed` notification. This exposes only the detailed
typed tools needed for the current task instead of placing every detailed tool in the
model context at startup.

| Loader | Detailed tools |
| --- | --- |
| `kata.load_issue_discovery` | `kata.search`, `kata.list`, `kata.show`, `kata.ready`, `kata.next`, `kata.labels`, `kata.graph` |
| `kata.load_issue_mutation` | `kata.create`, `kata.edit`, `kata.comment`, `kata.edit_comment`, `kata.claim`, `kata.set_label`, `kata.set_metadata`, `kata.set_schedule`, `kata.set_deadline`, `kata.move` |
| `kata.load_coordination` | `kata.assign`, `kata.unassign`, `kata.inbox`, `kata.status` |
| `kata.load_cron` | `kata.cron_capabilities`, `kata.cron_new_uid`, `kata.cron_jobs`, `kata.cron_job`, `kata.cron_job_create`, `kata.cron_job_update`, `kata.cron_job_delete`, `kata.cron_job_restore`, `kata.cron_workflows`, `kata.cron_workflow`, `kata.cron_workflow_create`, `kata.cron_workflow_update`, `kata.cron_workflow_delete`, `kata.cron_workflow_restore`, `kata.cron_runs`, `kata.cron_run`, `kata.cron_run_observe` |
| `kata.load_issue_lifecycle` | `kata.close`, `kata.reopen`, `kata.delete`, `kata.restore`, `kata.purge`, `kata.wait`, `kata.audit_closes` |
| `kata.load_leases` | `kata.lease_status`, `kata.lease`, `kata.lease_force_release`, `kata.lease_steal` |
| `kata.load_projects` | `kata.projects`, `kata.project_show`, `kata.project_create`, `kata.project_update`, `kata.project_merge`, `kata.project_remove`, `kata.project_restore`, `kata.project_purge` |
| `kata.load_teams` | `kata.teams`, `kata.team_create`, `kata.team_show`, `kata.team_delete`, `kata.team_member_set`, `kata.project_access_show`, `kata.project_access_set` with the same explicit daemon-wide administration capability |
| `kata.load_tokens` | `kata.tokens`, `kata.token_create`, `kata.token_revoke` when `--enable-token-admin` is set in daemon-wide mode |
| `kata.load_system` | `kata.system` |
| `kata.load_federation` | `kata.federation_bridge_connect`, `kata.federation_bridge_status`, `kata.federation_bridge_disconnect`, `kata.federation_status`, `kata.federation_enrollment_revoke`, `kata.federation_rebind`, `kata.federation_leave`, `kata.federation_quarantine`, `kata.federation_root_key_repin` |
| `kata.load_sync` | `kata.sync_status`, `kata.sync_update`, `kata.sync_once` |
| `kata.load_recurrence` | `kata.recurrences`, `kata.recurrence_update`, `kata.recurrence_delete` |
| `kata.load_activity` | `kata.digest`, `kata.events` |
| `kata.load_import` | `kata.import_issues` |
| `kata.load_external_roots` | `kata.connectors`, `kata.connector_fields`, `kata.connector_field_map`, `kata.connector_field_unmap`, `kata.bridge_bind`, `kata.bridge_show`, `kata.bridge_reconcile`, `kata.bridge_pause`, `kata.bridge_resume`, `kata.bridge_resolve_field`, `kata.bridge_resolve_comment`, `kata.bridge_unbind` |
| `kata.load_storage` | `kata.storage_export`, `kata.storage_import` when host storage is enabled |
| `kata.load_docs` | `kata.search_docs`, `kata.read_doc` |

`kata.connectors`, `kata.connector_fields`, `kata.connector_field_map`,
`kata.connector_field_unmap`, and `kata.bridge_bind` require the
`--all` daemon-wide scope. The remaining bridge tools operate on
already-bound issues inside the startup project scope.

`kata.inbox` lists open issues that carry a request for one recipient's
attention, the same requests `kata inbox` shows, and never clears them. It
reads up to `limit` issues and sets `truncated` when more carry requests.
`kata.status` reports an issue's owner, hold, and lease along with the actor
the server writes as. `kata.unassign` accepts `expected_owner` so a handoff
clears ownership only while that actor still holds the issue.
`kata.project_show` returns one project's metadata and workspace aliases.

`kata.search_docs` and `kata.read_doc` search and read the user documentation
bundled into the `kata` binary: the overview, getting-started, guide,
workflow, reference, and operations pages. Search returns section ids and
short excerpts; `kata.read_doc` returns one section's Markdown. They work
without network access and describe the documentation of the running build.

Loaders are idempotent. A loader reports `available=false` when its optional
startup dependency is absent. Loaded tools keep their individual input and
output schemas and safety annotations; Kata does not combine unrelated actions
into a generic command tool.

The tools use structured input and output. List-like results, including
`kata.audit_closes` rows, default to 20 and are bounded at 100;
`kata.audit_closes` pages with an opaque `cursor` (`truncated` plus
`next_cursor` out) validated against the close history below it, so shared
timestamps cannot skip or repeat rows and a project merge or issue purge
during pagination fails the page with a restart error instead of silently
skewing it. `kata.show` returns at most 100 comments. Create and
comment require idempotency keys. `kata.close` accepts an optional
`idempotency_key` for safe retries and an optional `revision` for a conditional
close. An exact retry returns the original close event in the mutation output.
`kata.token_create`, recurrence creation,
`kata.storage_import`, `kata.lease`, and `kata.sync_once` are annotated
non-idempotent: the first two mint a new record on every identical retry, a
forced storage import replaces the target again (with a fresh instance
identity when `new_instance` is set), a lease renewal extends the expiry on
every call, and each sync pass re-imports from the provider.
`kata.cron_new_uid` is read-only but non-idempotent: each call returns a fresh,
unreserved UID.
`kata.lease_steal` acquires first and only force-releases a holder that
denies that acquire, so a retry after a lost response keeps a lease the
startup principal already holds instead of releasing and re-acquiring it.
Destructive tools preserve Kata's exact
confirmation and revision contracts. `kata.delete` and `kata.purge` confirm
against `project#short_id` and then address the daemon by the issue's
immutable UID.

Recurrence patch and delete calls require the current positive `revision` and
send it as `If-Match`. Create calls do not use a revision.

`kata.create` supports `force_new`. `kata.claim` supports `force`,
`if_unowned`, and an optional `ttl_seconds` from 60 through 86400. A timed
claim renews when the same actor repeats it, and the result includes the
assignment expiry and previous owner when present. `kata.edit` supports field,
owner, priority, relationship, scheduling, and generic metadata changes. An
issue-field or relationship change and a metadata change must use separate
`kata.edit` calls so one failed request cannot leave a partial edit.

### Startup sections

For gateways that need typed tools in their first `tools/list`, configure the
daemon-mounted endpoint before starting it:

```toml
[mcp]
startup_sections = ["issue_discovery", "issue_mutation", "coordination", "issue_lifecycle", "projects", "docs"]
```

Use section IDs, such as `issue_discovery`, rather than loader tool names such
as `kata.load_issue_discovery`. The IDs are `activity`, `coordination`, `cron`,
`docs`, `external_roots`, `federation`, `import`, `issue_discovery`, `issue_lifecycle`,
`issue_mutation`, `leases`, `projects`, `recurrence`, `storage`, `sync`, `system`,
`teams`, and `tokens`. Each ID selects the detailed tools in its loader row above.

Only available sections can load at startup. `storage` requires host storage
opt-in; `teams` and `tokens` require explicit token administration with
daemon-wide scope. These three sections are unavailable on the daemon mount.
An unknown, duplicate, or unavailable requested ID disables `/mcp` with HTTP
503 and logs the reason; the daemon's REST API and web UI keep serving.

`KATA_MCP_STARTUP_SECTIONS` overrides the array with comma-separated, trimmed
IDs. An empty environment value preserves TOML. Restart the daemon after
changing either setting, then reconnect the gateway or refresh its catalog.
Every fresh server publishes the configured tools before any loader call;
no priming hook is needed. All loaders remain available and idempotent, so
clients can still load other sections progressively. Omit the setting or use
`startup_sections = []` to retain the default loader-only initial catalog.
These settings apply to the daemon mount; `kata mcp serve` retains progressive
loading.

## Native cron through MCP

Load `kata.load_cron` for job and workflow lifecycle tools, live capability
discovery, and attributed run evidence. This interface uses cron job definitions;
the separate `kata.load_recurrence` tools manage RRULE issue templates.

| Purpose | Tools |
| --- | --- |
| Discovery and identity | `kata.cron_capabilities`, `kata.cron_new_uid` |
| Jobs | `kata.cron_jobs`, `kata.cron_job`, `kata.cron_job_create`, `kata.cron_job_update`, `kata.cron_job_delete`, `kata.cron_job_restore` |
| Workflows | `kata.cron_workflows`, `kata.cron_workflow`, `kata.cron_workflow_create`, `kata.cron_workflow_update`, `kata.cron_workflow_delete`, `kata.cron_workflow_restore` |
| Run evidence | `kata.cron_runs`, `kata.cron_run`, `kata.cron_run_observe` |

Every operation uses the MCP server's configured daemon credentials, project
scope, and accountable actor. Select `project` explicitly in multi-project modes.
Run observations accept the usual per-call `teammate` override; an empty value
suppresses the startup default without changing the actor. Issue-scoped tokens
do not gain project-level cron authority.

Call `kata.cron_new_uid` before creation and retain its returned `uid`. The helper
creates and reserves nothing and does not bind the UID to a project. Creates use
that caller-retained identity without an expected event UID. After a lost create
response, read the same UID with `kata.cron_job` or `kata.cron_workflow`; repeating
a create preserves the daemon's conflict rather than becoming an update.

Update, delete, and restore require the current `expected_event_uid`. Read the
current definition after a conflict and decide whether to retry; MCP never fetches
a new CAS value on the caller's behalf. Delete is the existing reversible cron
tombstone operation. Workflow-in-use checks and native parent bindings are
preserved. Typed definitions and opaque option objects retain exact JSON numbers.

`kata.cron_runs` defaults to 20 records, accepts `limit` from 1 through 100,
and returns `next_before_uid`; pass it as `before_uid` to continue. `job_uid`
filters history. `kata.cron_run_observe` requires a caller-retained run UID,
`expected_revision` (`0` for new evidence), a job or workflow reference with its
definition event UID, status, and a versioned summary. Identical retries return
`replayed: true`; distinct run UIDs for one scheduled occurrence remain separate
observations. Recording evidence does not launch execution, claim an issue, or
change its status.

Enabled native `notify` jobs activate the existing daemon delivery. Execution
jobs and workflows require an installed external adapter. Check
`kata.cron_capabilities` for the selected daemon's actual advertised features;
permission denial, unsupported capability, and temporary unavailability remain
distinct errors. See [Native cron](cron.md) for job definitions and adapters.

## Scheduling and someday

`kata.create` and `kata.edit` have first-class `scheduled_on` and `timezone`
fields. Accepted `scheduled_on` forms are:

- `YYYY-MM-DD`
- `YYYY-MM-DDTHH:MM`
- `YYYY-MM-DDTHH:MM:SS`
- UTC RFC 3339, such as `2026-09-01T22:00:00Z`

Civil date and time values use the supplied IANA timezone. Numeric offsets are
rejected. Use `clear_scheduled_on` or `clear_timezone` when editing.

`kata.set_schedule` writes the reserved `scheduled_on` metadata key. Pass
`schedule` to set a value or `clear_schedule: true` to remove it.
`kata.set_deadline` writes `deadline_on`; pass `deadline` or
`clear_deadline: true`. Each pair is mutually exclusive. An optional `revision`
adds the same conditional-write guard as `kata.set_metadata`. Both tools accept
the same date, local date-time, and UTC-instant forms as `scheduled_on`.

Generic metadata supports the native parking marker:

```json
{"metadata":{"someday":true}}
```

Set the key to JSON `null` to remove it. Do not write `someday=false`. The same
null-removal rule applies to `kata.set_metadata` and other generic metadata
patches.

## Team and project access administration

Start with `--all --enable-token-admin`, then load `kata.load_teams`.
The startup capability allows the tools to appear; the daemon still checks the
credential's owner authority on every call. An ordinary account does not gain
administrative access through an MCP actor label.

Use `kata.team_create` with `name`, and select teams by `team_uid` for
`kata.team_show` or `kata.team_delete`. Set membership through
`kata.team_member_set` with `team_uid`, `member_actor`, and `present`.
The member actor is the account being enrolled, separate from the authenticated
actor making the change. Deleting a team keeps its projects restricted.

`kata.project_access_show` reads policy by project name.
`kata.project_access_set` accepts `project`, `visibility` (`all` or `teams`),
and `team_uids`. An optional `revision` guards against a stale edit; omitting
it captures the current revision before the write. A concurrent change returns
a conflict. See [team visibility](../operations/federation.md#team-visibility)
for the authorization rules.

## Read relay attribution

Issue and comment results retain `author` and `teammate`, alongside
`accountable_actor`, `source_actor`, `authority_uid`, and `verification` when
available. The accountable actor identifies the authenticated account;
source fields retain original attribution across relay hops. Keep `pending`,
`verified`, and `legacy` verification states distinct. Source labels do not
grant access, and an omitted verification field makes no verified claim.

## Events, tokens, and federation

`kata.events` supports immediate `poll` and bounded `wait` modes. It returns a
resume cursor. Wait mode uses the daemon's SSE stream where one stream can
enforce the selected scope. Fixed multi-project allowlists use bounded scoped
polling. A `sync.reset_required` result returns `reset_after_id`, advances
`next_after_id` to that reset cursor, and returns no stale events.

Unscoped `kata.token_create` returns the plaintext token once. `kata.tokens`, status
tools, errors, and later calls never return that secret or its hash. Token
administration requires both the `--all` daemon-wide startup scope and
the explicit `--enable-token-admin` startup capability. A default workspace
server, a one-project server, and a fixed-allowlist server cannot read, create,
or revoke global daemon tokens.

`kata.token_create` accepts `team_uids` to commit initial account membership
with token issuance. This works for ordinary and scoped credentials; invalid
teams leave neither a new token nor partial membership.

In daemon-wide token-admin mode, `kata.token_create` also accepts `issue`,
`expires_in_seconds`, and `token_file` together. This creates an
`issue_subtree` credential and writes its plaintext once to a new owner-only
file. The tool withholds the plaintext from its result and attempts immediate
revocation if delivery or response validation fails. A scoped credential
cannot mint another credential; it creates delegated work by creating children
with an accessible parent. The file belongs to the MCP host. See the
[worker provisioning example](../operations/remote-daemon.md#identity-tokens)
for consuming it and the [scope guide](../design/issue-scoped-credentials.md)
for membership and revocation rules.

Project bridges require Kata 0.19.0 or later. Use
[Share projects between hubs](../guide/project-relays.md) for setup and target
selection. The
[operator reference](../operations/federation.md#connect-a-project-through-the-daemon-catalog)
owns the credential, URL and daemon-side source requirements.

MCP can connect one project through a catalog account, but it
never handles enrollment secrets. It has no tool to issue an enrollment from
this daemon, accept an enrollment token, or run a full `kata federation join`;
the bridge's enrollment token stays in the daemon's credential store.
`kata.federation_bridge_connect` selects `project`, `hub_catalog` and
`hub_project`; the local daemon uses that catalog account to enroll the narrow
bridge. `preflight=true` previews without enrollment and checks cross-project issue links;
`hub_links_checked` is `false` when the hub predates that check. Set `serve_downstream=true`
only when this daemon will also relay the project to its own spokes; it defaults
to false. `signing_key_id` with `signing_key_env` or an absolute
`signing_key_file` selects the daemon-side signing source that a
required-signing hub needs. The local actor comes from MCP startup state.
`kata.federation_bridge_status` reads one selected project's recorded bridge
state, including a pending enrollment. `kata.federation_bridge_disconnect`
accepts `project` and optional `preflight`, preserving local data during detach
and retaining retry state after interrupted cleanup. `local_only=true` detaches
from a hub that is gone for good without revoking its grant; the result's
`warning` says the hub operator must revoke it. These tools require
`--all` and daemon-owner authority at the API. They return no token values or
credential inventory and accept no caller-supplied actor or token override.

`kata.federation_root_key_repin` accepts `project`, optional `preflight`,
optional `previous_key_file`, and optional `strand_relays`, and calls the
[root key repin endpoint](../operations/federation.md#root-signing-key). It
requires `--all` and daemon-owner authority at the API.

`kata.federation_status` lists secret-free enrollment records, and
`kata.federation_enrollment_revoke` can revoke one.

Enabling issue synchronization selects which external repository the daemon's
configured GitHub credentials read, so `kata.sync_update` with
`action: "enable"` requires the `--all` daemon-wide scope. Scoped
servers can still disable the operator-configured binding and run
`kata.sync_once` against it.

Federation leave exposes `preflight`, `prepare`, `abort`, and `commit` phases
so an operator can preserve the normal revoke-before-local-teardown order. The
phase is required. Pass `prepare` a random `preparation_id`; it returns that
ID, or one it picked when you pass none. When the hub revoke fails after
`prepare`, or the `prepare` result is lost, run `abort` with that
`preparation_id` to release the prepared leave so rebind and join work again.
Retrying `prepare` with the same `preparation_id` holds the same preparation. The daemon refuses `abort` with
`federation_leave_started` once a reservation is marked for leave or local
teardown has begun; finish that leave instead. A commit without external hub
revocation also requires `COMMIT FEDERATION LEAVE <project>`. The `archive` disposition and
`kata.federation_rebind`, which routes the replica's enrollment token to the
selected catalog origin, require the `--all` daemon-wide scope.
Quarantine retry and skip require
`RETRY FEDERATION BATCH <id>` or `SKIP FEDERATION BATCH <id>`.

## Host-storage opt-in

JSONL storage access is absent by default. Enable it only on the daemon host:

```sh
kata mcp serve --all \
  --storage-root /srv/kata/exchange \
  --storage-target restore=restore.db
```

`kata.storage_export` additionally requires the `--all` daemon-wide
scope even when a `project` filter is supplied: a project-filtered JSONL
export still contains cross-project link rows and unredacted event payload
references that scoped reads deliberately hide.

`--storage-target alias=path-or-DSN` is repeatable. Tool calls select an alias;
they cannot submit a database path or DSN. Artifact paths are relative to the
storage root. Absolute paths, `..`, symlink traversal, directories, and special
files are rejected. Storage operations stay anchored to an open root descriptor
so a directory symlink swap cannot redirect them outside the configured root.
SQLite target paths are also contained by the root.

Export opens the active storage read-only and atomically installs the JSONL
artifact. It cannot use the active SQLite database, its sidecar files, or any
configured SQLite import target as an artifact path. Replacing an existing
non-storage artifact requires `force=true` and
`OVERWRITE ARTIFACT <artifact>`. Export includes deleted records unless
`include_deleted=false` is explicit. Import refuses the active daemon storage.
Active SQLite sidecars cannot be configured as restore targets. Restored SQLite
files use owner-only permissions. Replacing an existing SQLite target requires `force=true` and
`REPLACE STORAGE <alias>`. Force replacement of PostgreSQL storage is not
available through MCP.

## Protocol contract

Kata delegates protocol negotiation to the official Go MCP SDK. Stateless
clients can use `server/discover`; session clients can use `initialize` and
`notifications/initialized`. Stdio accepts both. The Streamable HTTP listener
keeps sessions so section loaders can send tool-list changes, so it answers
other calls that carry 2026-07-28 per-request metadata with an
unsupported-version error listing the session versions it accepts. JSON-RPC
batches are rejected. Each compact JSON message is limited to 8 MiB.

Kata advertises tools only, including tool-list changes. It does not advertise prompts, resources, roots,
logging, sampling, subscriptions, or server-to-client requests. Discovery uses
a five-minute private cache hint. Tool execution allows 20 starts per second,
a burst of 20, and at most eight concurrent daemon calls per server process.

Host process control stays outside MCP: daemon lifecycle, the TUI and Web UI,
install/update, database migrations and cutovers, and raw internal replication
endpoints remain CLI or operator workflows.
