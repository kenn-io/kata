# kata カタ

Local-first issue tracking for humans and coding agents.

kata gives agents a structured place to record tasks, decisions, links,
comments, and state changes without turning GitHub Issues, markdown plans, or
chat transcripts into the source of truth.

The CLI is built for agents and automation: stable commands, JSON output, and
predictable failure modes. The TUI is built for people: browse, triage, edit,
and supervise agent-written work without reading raw JSON. Both talk to the
same local daemon and SQLite database.

Status: early public preview. The CLI, daemon, and TUI are usable, but command
contracts and UI details may still change before a stable release.

## Quick Start

```sh
go install go.kenn.io/kata/cmd/kata@latest   # or see Install for other options

cd your-repo
kata init                                         # bind this workspace to a kata project
kata create "fix login race"                      # returns the issue's short_id, e.g. abc4
kata list                                         # list open issues
kata show abc4                                    # inspect by short_id
kata close abc4 --done --message "Fixed; tests green." --commit <sha>
kata tui                                          # browse and triage interactively
```

See [Install](#install) for `go install`, build-from-source, and Windows
instructions; see [Working with kata](#working-with-kata) for a longer
walkthrough.

## What kata does today

What you can do:

- Track issues separately per project, with short IDs derived from each
  issue's ULID (`kata#abc4`).
- Create, list, edit, close, reopen, comment, label, assign, and link issues.
- Search, idempotent-create, soft-delete, restore, and irreversibly purge.
- Browse and triage in a TUI (`kata tui`) over the same data.
- Stream state changes as durable events for polling, live tailing, hooks, and
  TUI updates.

How it's built:

- Workspace-to-project binding lives in `.kata.toml`, falling back to a git
  remote URL when no binding file exists.
- Data lives locally in SQLite under `KATA_HOME` behind a long-running daemon.
- Issues have stable ULID `uid` values; `short_id` (the lowercased last 4+
  chars of the ULID) is the display label, qualified as `kata#abc4` across
  projects.
- `kata export` and `kata import` provide a git-friendly JSONL backup and
  schema cutover path.
- Successful commands emit JSON for reliable parsing by agents and scripts.

## Goals

Three priorities:

- Agent ergonomics: stable commands, JSON-first workflows, explicit workspace
  binding, search-before-create, idempotency keys, and predictable exit codes.
- Human oversight: a TUI that helps people browse, triage, edit, and supervise
  agent activity without reading raw JSON.
- Auditability: append-only comments, event history, actor attribution, and
  explicit destructive operations.

Longer term, kata is intended to support a shared server mode for teams, CI,
and multiple agents. That mode should be a real authenticated deployment, not
the local daemon exposed on a public interface.

## Why kata, and how is it different from Beads or git-bug?

kata is intentionally small. It is not a project-management suite, a git
workflow engine, or an agent worker pool. It is a durable task ledger that
humans and agents can both understand.

[Beads](https://github.com/gastownhall/beads) is a substantial tool in the
same space: a Dolt-powered distributed graph issue tracker for AI agents. Its
default shape is project-local: `bd init` creates a `.beads/` Dolt database
alongside the code, with native Dolt history, branching, merging, push/pull,
and optional server mode for concurrent writers. That does not mean Beads
requires git; it also supports git-free workflows.

kata makes a different architectural bet: the issue ledger should be a local
service adjacent to workspaces, not a database owned by each repository. A
repository that uses kata gets only a small, secret-free `.kata.toml` binding;
the canonical state lives in `KATA_HOME` behind a daemon API. That keeps task
state out of code history while still giving agents a structured coordination
layer and giving humans a TUI over the same event stream.

It also has a different complexity budget. Beads is a large, capable system
with distributed database semantics, merge behavior, federation, MCP
integration, and agent workflow machinery. kata is deliberately smaller: one
daemon, one local store, one HTTP API, one TUI, and a narrow issue model that
should stay easy to understand, operate, and teach to agents.

[git-bug](https://github.com/git-bug/git-bug) takes the most git-native
approach of the three: it stores issues, comments, and identities as git
objects under custom refs in the repository itself, and distributes them
through ordinary `git push` / `git pull`. Every clone carries the full issue
history offline, and bridges sync with GitHub, GitLab, and Jira. kata sits at
the other end of that spectrum — issue state lives outside git entirely, so
the workspace stays clean, non-git workspaces work identically, and issue
history is not interleaved with code history. The trade-off is that kata
cannot piggyback on git remotes for sharing; that is what the future
authenticated server is for.

| Design choice | Beads | kata |
|---|---|---|
| Storage boundary | Project-local `.beads/` Dolt database by default | User-local `KATA_HOME` SQLite database behind a daemon |
| Repository footprint | Owns issue state near the repo by default; can sync via Dolt remotes | Repo stores only `.kata.toml` project binding |
| Collaboration model | Dolt push/pull, Dolt server mode, federation, MCP tooling | Local daemon today; future authenticated shared server |
| IDs | Hash-based IDs by default; counter IDs optional | Short IDs derived from each issue's ULID (`kata#abc4`) |
| Workflow shape | Rich graph tasks, priorities, claiming, messages, dependencies | Deliberately small issue ledger: status, comments, labels, owner, links, events |
| Git relationship | Git integration is optional but first-class; commit conventions and doctor checks can connect code history to issues | Git can help identify workspaces; kata does not infer issue state from commits |

All three approaches are useful. Beads is strongest when you want
distributed, database-versioned task memory that can travel with a project and
merge across branches or agents. git-bug is strongest when you want issue
state to live inside the repository's own git history and ride the same
remotes the code does. kata is aimed at a smaller, API-first issue system
that can span workspaces and eventually teams without forcing every user and
agent to understand the repository, git remote, or distributed database that
carries the issue state.

## Install

kata is a single Go binary with no runtime dependencies and builds on macOS,
Linux, and Windows. Pre-built release binaries are not published yet; install
from source using one of the options below. All paths require **Go 1.26 or
later** (<https://go.dev/dl/>).

### `go install` (any platform)

```sh
go install go.kenn.io/kata/cmd/kata@latest
```

Go places `kata` in `$(go env GOBIN)`, falling back to `$(go env GOPATH)/bin`
(typically `~/go/bin` on Unix, `%USERPROFILE%\go\bin` on Windows). Add that
directory to your `PATH`.

### Build from a clone (macOS / Linux)

```sh
make install                            # installs to ~/.local/bin
make install GOBIN=/usr/local/bin       # or set GOBIN to install elsewhere
```

Add the install directory to your `PATH` if it isn't already. `GOBIN` from the
environment is also honored (`export GOBIN=/opt/bin && make install`).

### Build from a clone (Windows)

PowerShell or cmd.exe:

```powershell
go build -o kata.exe ./cmd/kata
# Move kata.exe to a directory on your PATH, e.g. %USERPROFILE%\.local\bin
```

The `go install` command above also works on Windows and is usually simpler.

### Development

```sh
make test          # run the test suite
make lint          # run golangci-lint
```

## Working with kata

Initialize kata in a workspace:

```sh
kata init
```

`kata init` creates or resolves the project and writes `.kata.toml` when
needed. In a git workspace, the default project name is derived from the
remote URL. For a non-git workspace or an explicit shared project name:

```sh
kata init --project product
```

Create and inspect issues:

```sh
kata create "fix login race" --body "Safari can double-submit the callback."
kata list
# Each issue prints its short_id (e.g. abc4); use it for follow-up commands.
kata show abc4
kata comment abc4 --body "Reproduced on macOS."
kata close abc4 --done --message "Fixed; verified tests pass." --commit <sha>
```

Open the TUI for human triage:

```sh
kata tui
```

Press `?` inside the TUI for keybindings.

Use `--workspace <path>` when running from outside the project directory:

```sh
kata --workspace ~/code/product list --status all
```

Set the actor for a session:

```sh
export KATA_AUTHOR=codex-wesm-laptop
kata whoami
```

Actor precedence is `--as` > `KATA_AUTHOR` > `git config user.name` >
`anonymous`.

## Core Commands

Common issue commands:

```sh
kata create <title> [--body TEXT | --body-file PATH | --body-stdin]
                  [--label LABEL] [--owner NAME] [--priority 0..4]
                  [--parent <ref>] [--blocks <ref>] [--blocked-by <ref>]
                  [--related <ref>] [--idempotency-key KEY] [--force-new]
kata list [--status open|closed|all] [--limit N]
kata show <issue-ref>
kata edit <issue-ref> [--title TEXT] [--body TEXT] [--owner NAME]
                  [--priority 0..4 | --priority -]
                  [--parent <ref>] [--blocks <ref>] [--blocked-by <ref>] [--related <ref>]
                  [--remove-parent <ref>] [--remove-blocks <ref>]
                  [--remove-blocked-by <ref>] [--remove-related <ref>]
                  [--comment TEXT]
kata comment <ref> [--body TEXT | --body-file PATH | --body-stdin]
kata close <ref> --done --message <text> [--commit|--pr|--test|--reviewed|--evidence] [--comment TEXT]
kata close <ref> [--wontfix|--duplicate-of <ref>|--superseded-by <ref>|--audit-no-change] [--comment TEXT]
kata reopen <ref> [--comment TEXT]
```

`--comment TEXT` is available on `close`, `reopen`, `edit`, `assign`,
`unassign`, and `label add`/`rm`. The mutation lands first; the comment is
appended in a follow-up call. If the comment call fails, the error names
the issue so you can retry with `kata comment <ref> --body ...`.

Refs accept a bare short_id (`abc4`), a qualified short_id (`kata#abc4`), or
a full 26-char ULID. The relationship flags on `create`/`edit` are
documented in detail in "Relationships ride on `kata create` and `kata edit`"
below.

Closing issues:

```sh
kata close abc4 --done --message "<what changed and how it was verified>" \
                --commit <sha>
kata close abc4 --duplicate-of d4ex  --message "<short pointer>"
kata close abc4 --superseded-by d4ex --message "<short pointer>"
kata close abc4 --wontfix --message "<>=60 chars of rationale>"
kata close abc4 --audit-no-change \
                --message "<scope + verification of no-change conclusion>" \
                --evidence "no-change-audit:<short rationale>" \
                --reviewed <path/to/file>
```

Closing an issue asserts that the work is complete. Close each issue
as soon as its work is verified — not at the end of a batch. Use the
`needs-review` label and a comment when the work is incomplete instead.
The daemon refuses these structurally dangerous patterns:

- closing a parent while its children remain open
- closing >3 siblings under the same parent within 5 minutes
- closing two siblings of the same parent with the same close message
  (within 30 minutes, for `done`/`audit-no-change`)

The sibling-burst and repeated-message throttles can be disabled
daemon-wide via `[close.throttle] enabled = false` in
`<KATA_HOME>/config.toml`; the parent-completeness refusal and the
substance/evidence checks on `--message`/`--evidence` always run.

The TUI close path (`x` in `kata tui`) bypasses the substance and
evidence checks — interactive humans close with a single keystroke.
Structural guards still apply.

`kata audit closes` lists close events with filters; specific lazy
closes can be undone individually with `kata reopen <ref>`.

Labels, ownership, and relationships:

```sh
kata label add <ref> <label> [--comment TEXT]
kata label rm <ref> <label> [--comment TEXT]
kata labels
kata assign <ref> <owner> [--comment TEXT]
kata unassign <ref> [--comment TEXT]
```

Relationships ride on `kata create` and `kata edit` as repeatable flags,
all framed from the operating issue's POV:

```sh
# Add (work on create + edit)
kata create "..." --parent <ref> --blocks <ref> --blocked-by <ref> --related <ref>
kata edit   <ref> --parent <ref> --blocks <ref> --blocked-by <ref> --related <ref>

# Remove (edit only)
kata edit <ref> --remove-parent <ref>        # strict: must equal current parent
kata edit <ref> --remove-blocks <ref>        # idempotent
kata edit <ref> --remove-blocked-by <ref>    # idempotent
kata edit <ref> --remove-related <ref>       # idempotent
```

`--parent` is at-most-one and replaces any existing parent on `edit`.
The other flags are repeatable. `--remove-parent <ref>` is strict: it fails
loudly if the current parent is unset or different from `<ref>` (an
optimistic-concurrency check against agents acting on stale state). All
mutations in a single `edit` call apply atomically.

Every `<ref>` above accepts a bare short_id (`abc4`), a qualified
short_id (`kata#abc4`), or a 26-char ULID. Legacy numeric `#N` refs
no longer resolve.

Search, readiness, events, and project inspection:

```sh
kata search <query> [--limit N] [--include-deleted]
kata ready [--limit N]
kata events [--after N] [--limit N]
kata events --tail [--last-event-id N]
kata digest --since 24h [--until ...] [--project-id N | --all-projects]
            [--actor NAME ...]
kata projects list
kata projects show <project>
kata projects rename <project> <name>
kata projects merge <source> <target> [--rename-target NAME]
kata projects remove <project> [--force]
kata projects restore <project>
kata export [--project NAME] [--output PATH]
kata import --input PATH --target PATH [--force]
```

`kata digest` summarizes activity over a time window. It groups events by
actor and lists per-issue actions (created, commented:N, closed:done,
labeled:bug, unblocks:abc4, ...) so you can see at a glance what each agent
or person did overnight. `--since` accepts a duration (`24h`, `7d`) or an
RFC3339 timestamp; `--until` defaults to now. The default scope is the
current workspace's project; pass `--all-projects` for a cross-project
digest, or `--project-id N` for an explicit one. `--actor` is repeatable to
limit the report to one or more actors.

Destructive operations are explicit:

```sh
kata delete <ref> --force --confirm "DELETE <qualified-id>"
kata restore <ref>
kata purge <ref> --force --confirm "PURGE <qualified-id>"
```

The confirmation string is the issue's qualified short_id, e.g.
`DELETE kata#abc4`. `delete` is reversible. `purge` is not.

Daemon, diagnostics, and agent instructions:

```sh
kata daemon status
kata daemon stop
kata daemon reload
kata daemon logs --hooks [--tail]
kata health
kata whoami
kata quickstart
kata tui
```

## Agent Quickstart

This is the short version to give any coding agent, regardless of whether that
agent supports skills, memories, or custom instructions. It is also shipped
with the CLI:

```sh
kata quickstart
kata agent-instructions   # alias
```

Session setup:

- Run from the project workspace, or pass `--workspace <path>`.
- Set `KATA_AUTHOR` once at session start.
- Prefer `--json` for reads and writes when you need to parse output.

Per-task guidelines:

- Never create a project implicitly. If the workspace is not initialized,
  report that `kata init` is needed.
- Search before creating; pass an idempotency key when you do create.
- Prefer updating existing issues over opening duplicates.
- Close only when the work is actually complete.
- Do not run `delete` or `purge` unless the user explicitly asks for that
  exact destructive action and issue ref.

Use relationships deliberately. The link types mean:

| Type | Meaning |
|---|---|
| `parent` | This issue is part of a larger issue. |
| `blocks` | The first issue must be resolved before the second can proceed. |
| `related` | Useful context, but not ordering. |

Example session (using `abc4` as a placeholder for the issue's actual
short_id, which `kata create --json` and `kata search --json` both
return):

```sh
# Search before creating
kata search "login race" --json

# If no existing issue fits, create with an idempotency key
kata create "fix login race" \
  --body "Observed double-submit in Safari callback." \
  --idempotency-key "login-race-2026-05-02" \
  --json

# Update an existing issue rather than open a duplicate
kata show abc4 --json
kata comment abc4 --body "Found another reproduction path." --json
kata label add abc4 safari --json
kata edit abc4 --blocks d4ex --json

# Close when done
kata close abc4 --done --message "Fixed; verified tests pass." --commit <sha> --json
```

For long-running agents, poll events and remember the returned cursor; resume
from it on the next call. If a response says `reset_required`, discard cached
kata state and resume from the reset cursor.

```sh
kata events --after 0 --limit 100 --json
```

For live streams, `--tail` emits newline-delimited JSON:

```sh
kata events --tail
```

## Sharing and multi-user workflows

Today kata is local-first:

- one local daemon;
- one local SQLite database;
- no authentication;
- trusted same-user CLI and TUI clients.

Multiple checkouts or repositories can share one kata project when they use
the same `.kata.toml` project name and run `kata init` in each checkout.
That shares the issue ledger — short_ids, labels, links, and events —
across those workspaces in the same local database.

If a repository rename accidentally creates a second project, merge the old
source into the surviving target, for example:

```sh
kata projects merge old-repo new-repo --rename-target new-repo
```

Future shared mode should be a distinct deployment:

- a shared kata server reachable over HTTPS, SSH tunnel, or a private network;
- authenticated users and service tokens;
- server-derived actor identity;
- server-side hooks and backups;
- the same project, issue, event, and relationship model.

The local daemon should not be exposed directly to a LAN or public network.

### Remote daemon (opt-in, no auth)

A kata daemon can serve clients on other hosts over a private network
(loopback, RFC1918, CGNAT, link-local, ULA — public addresses are rejected):

```sh
kata daemon start --listen 100.64.0.5:7777
```

Or set the address persistently in `<KATA_HOME>/config.toml`:

```toml
listen = "100.64.0.5:7777"
```

The CLI flag wins over the config file when both are present. Auto-started
daemons (the on-demand path triggered by `kata create`, `kata list`, etc.)
also pick up the config-file value, so on a host where you want every kata
invocation to use the same TCP address you only have to set it once.

Run the daemon under launchd / systemd / nohup on the host that holds the
SQLite database. Clients on other hosts target it by setting `KATA_SERVER`:

```sh
export KATA_SERVER=http://100.64.0.5:7777
kata list
```

Or by writing a per-developer, gitignored `.kata.local.toml` next to
`.kata.toml`:

```toml
version = 1

[server]
url = "http://100.64.0.5:7777"
```

`kata init` adds `.kata.local.toml` to `.gitignore` automatically.
`KATA_SERVER` wins over the file when both are set.

There is no authentication in this mode — network ACLs (firewall, VPN,
tailnet) are the access boundary. Default behavior (no flag, no env, no local
file) is unchanged: a local Unix-socket daemon is auto-started on demand.

## Backup and restore

`kata export` writes the local database as JSONL; `kata import` rebuilds
a database from that file. Together they cover backups, machine moves,
and migrations between schema versions.

Back up the local database:

```sh
kata daemon stop
kata export --output backups/kata-$(date -u +%Y%m%d).jsonl
kata daemon start
```

Without `--output`, `kata export` writes a timestamped file
(`kata-export-YYYYMMDDTHHMMSSZ.jsonl`) in the current directory. Export
refuses to run while a daemon holds the database open; pass
`--allow-running-daemon` to take a best-effort snapshot on a host where
you cannot stop the daemon.

Restore into a fresh database file:

```sh
kata import --input backups/kata-20260512.jsonl --target ~/.kata/restored.db
```

`kata import` always creates a brand-new database. The target must not
exist; `--force` deletes it first. There is no merge mode today — see
"Exporting a single project" below for what that means in practice. To
activate a restored database, point `KATA_DB` at it (or move it into
`KATA_HOME` as `kata.db`) and restart the daemon.

JSONL is plain text and diffs cleanly, so a simple versioned-backup
setup is to keep snapshots in a git repository:

```sh
mkdir -p ~/kata-backups && cd ~/kata-backups
git init -q
kata daemon stop
kata export --output snapshot.jsonl
kata daemon start
git add snapshot.jsonl
git commit -q -m "snapshot $(date -u +%FT%TZ)"
```

Run that on a schedule (cron, launchd, systemd timer) and you have
point-in-time recovery without operating a backup service. Push the repo
to a private remote if you want off-host storage.

### Exporting a single project

Use the global `--project NAME` flag to scope an export to one project:

```sh
kata daemon stop
kata --project myproj export --output backups/myproj.jsonl
kata daemon start
```

`--project-id N` works too if you prefer the numeric id from
`kata projects list --json`.

A single-project export round-trips into a **fresh** database that
contains only that project:

```sh
kata import --input backups/myproj.jsonl --target /tmp/myproj-only.db
```

This is useful for archiving one project before deleting it, handing a
project's full history to a collaborator who'll set up a fresh kata
install, or moving a project to a different host.

What does **not** work today:

- Importing a per-project snapshot **into an existing populated
  database**. `kata import` always wipes the target first; there is no
  merge or "add this project" mode. So you cannot use per-project files
  as building blocks to stitch a multi-project database together.
- Re-importing a snapshot on top of itself to refresh it incrementally.

For multi-project backups, take the full-database snapshot shown above
instead of one file per project. A per-project merge import (apply one
project's snapshot to an existing database without disturbing other
projects) is planned — tracked in
[kenn-io/kata#42](https://github.com/kenn-io/kata/issues/42).

## Configuration

Useful environment variables:

- `KATA_HOME`: data directory. Defaults to `~/.kata`.
- `KATA_DB`: explicit SQLite database path.
- `KATA_AUTHOR`: default actor for mutations.
- `KATA_HTTP_TIMEOUT`: per-request CLI timeout for non-streaming daemon calls
  (any `time.ParseDuration` string, e.g. `30s`, `2m`). Defaults to `5s`. Bump
  this for bulk imports where create requests can exceed the default.
- `KATA_SERVER`: opt-in remote daemon URL (e.g. `http://100.64.0.5:7777`). When
  set, the client skips local discovery and auto-start entirely. See "Remote
  daemon" below.
- `XDG_RUNTIME_DIR`: runtime socket parent on Unix.

The workspace binding file is intentionally secret-free:

```toml
version = 1

[project]
name = "product"
```

Commit `.kata.toml` when multiple agents, clones, or worktrees should resolve
to the same kata project.
