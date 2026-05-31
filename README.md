# kata カタ

Local-first issue tracking for humans and coding agents.

kata gives agents a structured place to record tasks, decisions, links,
comments, and state changes without turning GitHub Issues, markdown plans, or
chat transcripts into the source of truth. The CLI is built for agents and
automation: stable commands, JSON output, predictable failure modes. The TUI is
built for people: browse, triage, edit, and supervise agent-written work without
reading raw JSON. Both talk to the same local daemon and SQLite database.

The documentation in [`docs/`](docs/) is the definitive guide,
published with Zensical at <https://katatracker.com/>.

Status: early public preview. The CLI, daemon, and TUI are usable, but command
contracts and UI details may still change before a stable release.

## Quick start

```sh
go install go.kenn.io/kata/cmd/kata@latest   # or see Install below

cd your-repo
kata init                                    # bind this workspace to a kata project
kata create "fix login race"                 # returns the issue's short_id, e.g. abc4
kata list                                    # list open issues
kata show abc4                               # inspect by short_id
kata close abc4 --done --message "Fixed the login race and verified the relevant tests pass." --commit <sha>
kata tui                                     # browse and triage interactively
```

`kata create` returns each issue's short_id; use it in later commands. Press `?`
inside `kata tui` for keybindings.

## What kata does

- Track issues per project, with short IDs derived from each issue's ULID
  (`kata#abc4`).
- Create, list, edit, close, reopen, comment, label, assign, and claim issues;
  relate them with `--parent`/`--blocks`/`--blocked-by`/`--related`; search,
  soft-delete, restore, and purge.
- Browse and triage in a TUI (`kata tui`) over the same data.
- Stream durable events for polling, live tailing, hooks, and TUI updates.
- Back up and migrate with a git-friendly JSONL export and import.
- Run a private-network remote daemon or opt-in federation when one local
  daemon is not enough.

The priorities are agent ergonomics (stable commands, JSON-first workflows,
search-before-create, idempotency keys), human oversight (a TUI over the same
event stream), and auditability (append-only comments, event history, actor
attribution, explicit destructive operations).

Issue state lives in a user-local SQLite database under `KATA_HOME`, behind a
daemon. A repository commits only a small, secret-free `.kata.toml` binding, so
task state stays out of code history.

## How kata compares

kata is intentionally small. It is not a project-management suite, a git
workflow engine, or an agent worker pool. It is a durable task ledger that
humans and agents can both operate.

[Beads](https://github.com/gastownhall/beads) keeps issue state in a
project-local `.beads/` Dolt database with native history, branching, and
push/pull. [git-bug](https://github.com/git-bug/git-bug) stores issues as git
objects under custom refs and syncs them over `git push` and `git pull`. kata
makes a different bet: the ledger is a local service next to your workspaces,
not data carried in the repository. That keeps the workspace clean, works the
same in non-git directories, and keeps issue history out of code history. The
trade-off is that kata does not ride git remotes for sharing; the remote daemon
and federation cover that instead.

Moving from Beads? See
[Migrating from Beads](docs/guide/migrating-from-beads.md).
`kata import --source-format beads` drives the `bd` CLI and merges your issues
into a kata project.

## Install

kata is a single Go binary with no runtime dependencies, and builds on macOS,
Linux, and Windows. It needs **Go 1.26 or later**. Pre-built binaries are not
published yet.

```sh
go install go.kenn.io/kata/cmd/kata@latest
```

Go installs to `$(go env GOBIN)`, falling back to `$(go env GOPATH)/bin` (often
`~/go/bin`); put that directory on your `PATH`. To build from a clone, run
`make install` (it defaults to `~/.local/bin`). See
[Install](docs/get-started/install.md) for build-from-source and Windows
steps.

## Documentation

The [docs site](docs/) is the definitive reference:

- Get started: [Quickstart](docs/get-started/quickstart.md) ·
  [Install](docs/get-started/install.md)
- Guide: [Concepts](docs/guide/concepts.md) ·
  [Workspaces and projects](docs/guide/workspaces-projects.md) ·
  [Migrating from Beads](docs/guide/migrating-from-beads.md)
- Reference: [CLI](docs/reference/cli.md) ·
  [Configuration](docs/reference/configuration.md)
- Workflows: [Agent workflows](docs/workflows/agents.md) ·
  [Sharing models](docs/workflows/sharing.md)
- Operations: [Remote daemon](docs/operations/remote-daemon.md) ·
  [Federation](docs/operations/federation.md) ·
  [Hosted mode](docs/operations/hosted-mode.md) ·
  [Backup and restore](docs/operations/backup-restore.md)

## For coding agents

Run `kata quickstart` (alias `kata agent-instructions`) for the operating
contract: search before creating, pass an idempotency key on create, prefer
`--agent` output, claim work with `kata claim`, and close only when the work is
verified. [Agent workflows](docs/workflows/agents.md) is the same contract
in long form.

## Contributing

See [Contributing](docs/development/contributing.md) for the repository
layout and local checks (`make test`, `make lint`, `make vet`, `make nilaway`).
Licensed under the terms in [LICENSE](LICENSE).
