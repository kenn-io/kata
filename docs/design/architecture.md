# Architecture and design principles

This note records why kata is shaped the way it is: the goals it optimizes for,
the invariants it refuses to break, and the boundaries it deliberately keeps.
It is written for maintainers and operators. For day-to-day usage start with the
[guide](../guide/concepts.md); for the durable data and event model see the
[data model and durability notes](data-model.md).

## What kata optimizes for

kata is a single-binary, local-first issue tracker with a long-lived daemon, a
SQLite database, a CLI, and a TUI. Agents are the primary writers; humans
observe and steer. Three goals drive almost every decision.

**Agent ergonomics.** Stable JSON, stable exit codes, search-before-create,
idempotency keys with fingerprints, structured error envelopes, and no implicit
`$EDITOR` invocation on machine paths. Project binding is explicit so an agent
never silently writes into the wrong namespace.

**Auditability.** Every state change appends to an immutable event log with the
actor recorded. Comments are append-only. Deletion has a reversible soft tier
and an irreversible hard tier, each gated behind explicit confirmation.

**A small, sharp surface.** Issue lifecycle, three relationship types
(`parent`, `blocks`, `related`, with `blocked_by` as the inverse of `blocks`),
labels, owners, comments, and an optional priority. The surface stays small on
purpose. kata has no `in_progress` status (use `owner` or a label), no severity
field, no threaded comment replies, no reactions, no file attachments, and no
markdown rendering. Priority is the one field that was added after v1: it is a
constrained integer `0`–`4` rather than an open taxonomy, so it earns its place
without reopening the door to arbitrary custom fields.

## Lineage: borrowed from roborev, narrowed on purpose

kata borrows its shape from roborev: pure-Go SQLite (`modernc.org/sqlite`, no
CGO), a Huma-based HTTP API over a Unix socket, per-PID runtime files, a durable
resumable SSE event stream, and directory-style installable agent skills. The
divergence is the point. Where roborev runs review and fix workloads, kata runs
only issue CRUD, an event broadcaster, and a small bounded hook runner. There is
no agentic worker pool and no daemon-driven code execution beyond the user's own
configured hooks. Adopting a proven runtime shape kept the build simple; refusing
roborev's execution surface kept kata a tracker rather than a workflow engine.

## The daemon is the single access path

All reads and writes go through the daemon's HTTP API. The CLI and TUI are
clients of the same API; neither opens SQLite directly. One access path means
every client sees identical resolution, validation, and event ordering, and it
keeps the door open to remote and shared deployments where the database is not
on the caller's machine (see [trust boundaries](#trust-boundaries) below).

A read-only direct-SQLite fast path was considered and deferred. It would create
an immediate split between local behavior and any networked deployment, so kata
does not pay that complexity until a measured hot path justifies it.

### Transport and discovery

The daemon listens on a Unix socket by default (parent directory `0700`, socket
`0600`), falling back to loopback TCP on Windows or where a socket is
unavailable; TCP binds are validated as loopback unless an operator opts into a
remote listener. Runtime state is namespaced per database: a `<dbhash>` derived
from the absolute database path keeps two databases from colliding on sockets,
runtime files, or logs. Clients discover a running daemon by computing the
`<dbhash>`, scanning its runtime files, probing `/api/v1/ping`, and auto-starting
a daemon only if none is live.

## Project binding over current directory

The single most load-bearing decision in kata is that a project is bound to a
workspace explicitly, never inferred from wherever a command happens to run.

A **project** is the namespace for issues, numbering, links, labels, and events.
A workspace declares its project through a committed `.kata.toml` file. Resolution
walks up from the start path to find that file and the enclosing git root, then
resolves the project. Outside a bound workspace, and without an explicit
`--project`, every command except `kata init` fails with `project_not_initialized`.
kata never silently namespaces issues to a random directory, and `kata init` is
the only command that creates a project row. Auto-creation on first read or write
is deliberately excluded — it is exactly how agents end up writing into the wrong
project.

### Identity comes from the git remote, not the path

A workspace's alias identity is derived from its git remote URL — normalized
across SSH and HTTPS forms and stripped of embedded credentials — not from its
filesystem location. Cloning a repository to a new path therefore resolves to the
same project, and two repositories that ship together can share one project by
committing the same project name. Because alias identity is unique per git
origin, one git repository attaches to exactly one project; genuine multi-project
monorepos are out of scope rather than silently approximated. Workspace paths are
recorded only as last-seen diagnostic metadata, never as durable identity, which
is what lets the same model serve a daemon that cannot see the caller's checkout.

## Trust boundaries

Local mode and shared mode are distinct deployment modes. Shared mode is not
"expose the local daemon on a public interface"; conflating the two is the trap
this boundary exists to prevent.

**Local mode** trusts the OS user. The daemon listens on a Unix socket or
loopback only, runs without authentication, and treats the local user as the
boundary. To keep a loopback listener safe from drive-by browser requests, the
HTTP server rejects any non-empty `Origin` header, requires
`Content-Type: application/json` on mutations, and emits no CORS headers. The CLI
and TUI never set `Origin`, so they are unaffected.

**Shared and remote modes** add a network boundary. They require a bearer token
and an explicit trust opt-in before the daemon will put credentials on a
plaintext non-loopback connection, and they can derive the actor from the token
rather than trusting a client-supplied field. The resource model and HTTP API are
identical across modes; only transport, authentication, and how the actor is
established differ. Operational detail lives in the
[remote daemon](../operations/remote-daemon.md),
[hosted mode](../operations/hosted-mode.md), and
[federation](../operations/federation.md) guides.

## Actor attribution

Every mutation records an actor string. Local precedence is
`--as` > `$KATA_AUTHOR` > `$USER` > `git config user.name` > `anonymous`. `$USER`
ranks above `git config user.name` on purpose: login names such as `wesm` read
more cleanly as event actors and owner tokens than display names with spaces such
as `Wes McKinney`. In shared identity mode the daemon derives the actor from the
bearer token and ignores any body-supplied actor.

Actors are free-form snapshots stored on each event, not foreign keys into a user
table. kata has no global user registry; nodes in a federated deployment are
disambiguated by their instance UID. Storing snapshots keeps historical audit
readable after a person is renamed or a token is revoked.

## Closure is an explicit act, never inferred from git

kata does not scan commit messages, branches, or pull requests to decide that an
issue is done. Inferring closure from git history recreates what the design notes
call the "beads gravity well": commit-message conventions, orphan checks, branch
heuristics, and workflow linting that drift out of sync with reality.

Instead, closing is an explicit API mutation. CI or merge automation that knows
which issues a change resolves calls the close endpoint directly and can attach
source metadata (provider, repository, pull request, commit) in the event
payload. This supports merge-driven closure without turning kata into a git
workflow engine. Because closing asserts completion, it carries more than a
status flip — a reason, a substantive message, and typed evidence — so a reviewer
can later verify the claim. The user-facing rules are in
[close discipline](../guide/concepts.md#close-discipline).

## Hooks: local automation with a hard boundary

Hooks run local commands in response to `issue.*` events. They are deliberately
constrained:

- They fire **after** the database commit, asynchronously, on a bounded worker
  pool. A hook can never block or roll back a state change.
- They are invoked with `exec.Command(cmd, args...)` — **no shell**, and no
  environment-variable expansion of arguments. Event data reaches a hook as JSON
  on stdin and a small set of `KATA_*` scalar environment variables.
- This avoids the obvious shell-injection surface, but a hook is **not a
  sandbox**. It runs with the daemon's OS user and inherits no kata internals
  beyond the documented variables. Operators who need OS-level isolation should
  write hooks that re-exec under their own sandbox (a container, `firejail`,
  `bwrap`, and so on).

In a shared deployment, hooks are server configuration: they run on the server
with the server's OS user. A developer's committed `.kata.toml` must never be
able to cause server-side code execution, which is why workspace-local hook
configuration is excluded and `.kata.toml` carries only the project binding.
Hooks are configured globally on the daemon host, not per workspace.

## `kata doctor` checks the system, not the workflow

`kata doctor` is read-only and reports on system health only: daemon
reachability, database integrity, schema drift, stale runtime files, config parse
errors, and skill-install drift. It never lints the workflow — no "stale open
issues," no "dangling owners," no commit-reference orphans — and it never mutates
state. Its recommendations are limited to system-repair commands such as
reloading the daemon or reinstalling skills. Keeping diagnostics free of workflow
opinions is what makes the command safe to run anywhere and trustworthy when it
does flag something.

## Deliberately out of scope

Some exclusions are permanent design choices rather than unbuilt features:
workspace-local behavior overrides, per-issue SSE subscriptions, bulk mutation
endpoints, remote webhooks, a `(kata-#N)` commit-message convention, and the
workflow lints noted above. The list of non-goals is itself a design tool: it
keeps the surface small enough that an agent can learn the whole system and a
human can audit it. Features that later earned their place — federation, a
constrained priority field, token authentication — were added narrowly and only
after real use justified them, not speculatively.
