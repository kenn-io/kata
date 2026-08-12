---
last_edited: 2026-08-12
---

# Model Context Protocol server

Kata includes a native stdio MCP server for coding agents and other MCP
clients. The server gives typed access to Kata issue data, administration,
automation, and event workflows.

```sh
# Every project visible to the selected daemon (default)
kata mcp serve

# Add explicit daemon credential administration
kata mcp serve --enable-token-admin

# One project
kata --workspace /path/to/repository mcp serve
kata --project example-project mcp serve

# A fixed project allowlist
kata mcp serve --projects example-project,shared-project
```

JSON-RPC uses stdin and stdout. Kata writes no status text or logs to stdout.
The actor is fixed when the process starts and uses the normal precedence:
`--as`, `KATA_AUTHOR`, `USER`, Git `user.name`, then `anonymous`.

## Client configuration

A typical client command has this shape:

```json
{
  "command": "kata",
  "args": ["mcp", "serve"]
}
```

Use `--daemon <name>` to select a configured daemon. The normal client
settings also apply, including `KATA_SERVER`, bearer authentication,
private-network checks, and Unix socket discovery.

## Project scope

The server follows the selected daemon's active project catalog by default.
This matches a long-lived MCP client's need to use any project that the daemon
can access. Use startup flags only when the MCP process needs a narrower
boundary:

- `--workspace` or `--project` serves one project. Bare issue references remain
  valid.
- Allowlist mode resolves the supplied names once and keeps their immutable
  project UIDs. A later rename does not change the boundary.

Multi-project issue reads and writes use `project#ref`. Project-list tools can
read all projects in scope. Issue creation and other project-selected writes
require an explicit `project` in multi-project mode. Project creation is available
only in daemon-wide mode.

Tool calls cannot change the startup actor or expand the startup scope. The
daemon remains authoritative for authentication, attribution, revision
checks, federation trust, claims, and mutation policy.

## Progressive tool catalog

The initial catalog contains 13 read-only section loaders. Call the applicable
loader, then refresh the tool list when the server sends the standard
`notifications/tools/list_changed` notification. This exposes only the detailed
typed tools needed for the current task instead of placing all 57 tools in the
model context at startup.

| Loader | Detailed tools |
| --- | --- |
| `kata.load_issue_discovery` | `kata.search`, `kata.list`, `kata.show`, `kata.ready`, `kata.next`, `kata.labels`, `kata.graph` |
| `kata.load_issue_mutation` | `kata.create`, `kata.edit`, `kata.comment`, `kata.edit_comment`, `kata.claim`, `kata.set_label`, `kata.set_metadata`, `kata.set_schedule`, `kata.set_deadline`, `kata.move` |
| `kata.load_issue_lifecycle` | `kata.close`, `kata.reopen`, `kata.delete`, `kata.restore`, `kata.purge`, `kata.wait`, `kata.audit_closes` |
| `kata.load_leases` | `kata.lease_status`, `kata.lease`, `kata.lease_force_release`, `kata.lease_steal` |
| `kata.load_projects` | `kata.projects`, `kata.project_create`, `kata.project_update`, `kata.project_merge`, `kata.project_remove`, `kata.project_restore`, `kata.project_purge` |
| `kata.load_tokens` | `kata.tokens`, `kata.token_create`, `kata.token_revoke` when `--enable-token-admin` is set in daemon-wide mode |
| `kata.load_system` | `kata.system` |
| `kata.load_federation` | `kata.federation_status`, `kata.federation_enroll`, `kata.federation_enrollment_revoke`, `kata.federation_join`, `kata.federation_rebind`, `kata.federation_leave`, `kata.federation_quarantine` |
| `kata.load_sync` | `kata.sync_status`, `kata.sync_update`, `kata.sync_once` |
| `kata.load_recurrence` | `kata.recurrences`, `kata.recurrence_update`, `kata.recurrence_delete` |
| `kata.load_activity` | `kata.digest`, `kata.events` |
| `kata.load_import` | `kata.import_issues` |
| `kata.load_storage` | `kata.storage_export`, `kata.storage_import` when host storage is enabled |

Loaders are idempotent. A loader reports `available=false` when its optional
startup dependency is absent. Loaded tools keep their individual input and
output schemas and safety annotations; Kata does not combine unrelated actions
into a generic command tool.

The tools use structured input and output. List-like results default to 20 and
are bounded at 100. `kata.show` returns at most 100 comments. Create and
comment require idempotency keys. Destructive tools preserve Kata's exact
confirmation and revision contracts.

Recurrence patch and delete calls require the current positive `revision` and
send it as `If-Match`. Create calls do not use a revision.

`kata.create` supports `force_new`. `kata.claim` supports `force` and returns
the previous owner when the daemon reports one. `kata.edit` supports field,
owner, priority, relationship, scheduling, and generic metadata changes. An
issue-field or relationship change and a metadata change must use separate
`kata.edit` calls so one failed request cannot leave a partial edit.

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

## Events, tokens, and federation

`kata.events` supports immediate `poll` and bounded `wait` modes. It returns a
resume cursor. Wait mode uses the daemon's SSE stream where one stream can
enforce the selected scope. Fixed multi-project allowlists use bounded scoped
polling. A `sync.reset_required` result returns `reset_after_id`, advances
`next_after_id` to that reset cursor, and returns no stale events.

`kata.token_create` returns the plaintext token once. `kata.tokens`, status
tools, errors, and later calls never return that secret or its hash. Token
administration requires both the default daemon-wide startup scope and the
explicit `--enable-token-admin` startup capability. A normal server, a
one-project server, and a fixed-allowlist server cannot read, create, or revoke
global daemon tokens.

Federation leave exposes `preflight`, `prepare`, and `commit` phases so an
operator can preserve the normal revoke-before-local-teardown order. The phase
is required. A commit without external hub revocation also requires
`COMMIT FEDERATION LEAVE <project>`. Replica credentials stay pinned to the hub
origin by the daemon. Quarantine retry and skip require
`RETRY FEDERATION BATCH <id>` or `SKIP FEDERATION BATCH <id>`.

## Host-storage opt-in

JSONL storage access is absent by default. Enable it only on the daemon host:

```sh
kata mcp serve \
  --storage-root /srv/kata/exchange \
  --storage-target restore=restore.db
```

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
`notifications/initialized`. JSON-RPC batches are rejected. Each compact JSON
message is limited to 8 MiB.

Kata advertises tools only, including tool-list changes. It does not advertise prompts, resources, roots,
logging, sampling, subscriptions, or server-to-client requests. Discovery uses
a five-minute private cache hint. Tool execution allows 20 starts per second,
a burst of 20, and at most eight concurrent daemon calls per server process.

Host process control stays outside MCP: daemon lifecycle, the TUI and Web UI,
install/update, database migrations and cutovers, and raw internal replication
endpoints remain CLI or operator workflows.
