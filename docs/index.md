# kata カタ

kata is a local-first issue tracker for humans and coding agents. It gives
agents a durable task ledger for issues, comments, links, ownership, and state
changes without making GitHub Issues, markdown plans, or chat transcripts the
coordination system.

The public shape is intentionally small:

- a single `kata` CLI for automation and agent workflows;
- a local daemon with an HTTP API over a Unix socket by default;
- a SQLite database under `KATA_HOME`;
- a TUI for human triage and supervision;
- JSON, concise agent text, and human output modes;
- export/import paths for backup and schema cutover;
- opt-in private-network remote daemon and federation workflows.

kata is in early public preview. The CLI, daemon, and TUI are usable, but
command contracts and UI details can still change before a stable release.

## Why kata exists

Coding agents need more than a chat thread. They need a place to discover
available work, claim it, record decisions, preserve context across compaction,
and close only when the work has actually been verified. Humans need the same
state in a form they can review without reading raw JSON.

kata treats issue state as operational data adjacent to workspaces, not as
repository content. A repository usually commits only a small, secret-free
`.kata.toml` binding. The actual ledger lives in the user's kata home behind
the daemon.

## What kata does today

You can:

- create, list, edit, close, reopen, comment, label, assign, and claim issues,
  and relate them with `--parent`, `--blocks`, `--blocked-by`, and `--related`;
- use short issue refs derived from ULIDs, such as `abc4` or `kata#abc4`;
- search before creating and use idempotency keys for safe retries;
- stream durable events for polling, live tailing, hooks, and TUI updates;
- browse and edit issues in `kata tui`;
- back up or migrate the local database with JSONL export/import;
- run private-network remote daemon setups with bearer-token protection;
- opt projects into hub-and-spoke federation when local-first replication is
  more important than single-copy reads.

## Architecture

The CLI resolves a project from the current workspace, `.kata.toml`, or an
explicit `--project` value. It talks to a daemon, starting one automatically for
local use when needed. The daemon owns the SQLite connection, applies
mutations, records events, and serves both CLI/TUI reads and event streams.

Local mode uses a Unix socket under `KATA_HOME/runtime` on Unix platforms. TCP
listeners are explicit and guarded because bearer tokens over plaintext
non-loopback HTTP are only allowed for trusted private-network targets.

## When to use it

Use kata when you want a small, inspectable issue ledger that agents and humans
can both operate:

- multi-agent work in one repo or across worktrees;
- local project planning that should survive chat compaction;
- issue close discipline with evidence and audit trails;
- private team or lab workflows where a full SaaS issue tracker is unnecessary;
- experimental federation where each participant keeps a local daemon.

Avoid treating kata as a full project-management suite, a CI scheduler, an
agent worker pool, or an authorization platform. Shared-server authorization is
still deliberately narrow.

## Start here

Read the [Quickstart](get-started/quickstart.md), then skim
[Concepts](guide/concepts.md) and the [CLI reference](reference/cli.md). Agents
should use the [Agent workflows](workflows/agents.md) page as their operating
contract. Coming from Beads? See
[Migrating from Beads](guide/migrating-from-beads.md).
