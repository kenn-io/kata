# kata カタ — the issue tracker that runs inside the loop

kata is a local-first issue ledger for coding agents and the humans steering
them. Every operation is a round trip to a daemon on your own machine —
milliseconds, not a SaaS API away — so the tracker stops being the place where
work gets filed and becomes the way work gets coordinated.

```sh
# the operating loop, at local speed
kata next --unowned --agent      # OK next issue=abc4 priority=1 title="fix login race"
kata claim abc4 --agent          # OK claim abc4
kata comment abc4 --body "root cause: double submit in Safari callback"
kata close abc4 --done --message "Fixed the race; tests pass." --commit 4f2a91c
```

Install:

- macOS: `brew install kata`
- Linux: `curl -fsSL https://katatracker.com/install.sh | bash`
- Windows: `irm https://katatracker.com/install.ps1 | iex`

One Go binary, no runtime dependencies. See
[all install options](https://katatracker.com/docs/get-started/install/).

## 01 / Speed — milliseconds change what a tracker is for

A SaaS tracker is an API call away: TLS, auth, someone else's datacenter, a
rate limit. kata is a daemon on your machine with a SQLite ledger behind a Unix
socket — a round trip that costs about as much as a file read, more than 100×
closer than any hosted tracker. At that price, agents stop batching tracker
updates at the end of a task and start consulting the ledger at every decision.
That is not a faster version of the same tool. It is a different tool.

- **Round trip: <1ms.** Daemon round trips over the local socket; full CLI
  invocations in tens of milliseconds.
- **Network: 0.** Ordinary issue operations never leave the machine. Works
  offline, on a plane, in a lab.
- **Rate limits: none.** Poll, stream, and fan out as hard as your hardware
  allows.
- **Binary: 1.** A single Go binary; the daemon auto-starts on first use.
- **Ownership: 100%.** State lives in SQLite under `KATA_HOME`; your repo
  commits only a small, secret-free `.kata.toml`.

## 02 / Memory — working memory that survives the session

Chat context gets compacted. Markdown plans go stale in abandoned worktrees.
kata is the durable store agents write their state into as they work — cheap
enough to update constantly, structured enough to pick up cold.

- **Durable notes.** Comments capture decisions, partial attempts, and
  remaining work before a pause, compaction, or handoff.
- **Planning state.** `kata schedule` gates work until a date, `deadline`
  presses without parking, `someday` shelves without losing.
- **Structure as data.** Parent/child hierarchy, `--blocks`/`--blocked-by`
  ordering, and `--related` context make the plan a queryable graph.
- **Events.** Every mutation lands on a durable, cursor-addressable stream;
  replay what happened while you were away.

## 03 / Coordination — fan out, claim, join

Several agents on one machine, coordinating through the ledger at loop speed:
an orchestrator fans work out as child issues, workers claim and close them,
and the event stream joins results back.

- **Claim flow.** `kata next --unowned` returns at most one candidate under
  shared priority rules; a lost claim race fails loudly — that is the protocol
  working.
- **Fan out / join.** Create children under a tracking issue, launch agents,
  follow `kata events --tail` to return the moment a sub-task closes or needs
  a human.
- **Handoff.** `kata unassign` releases ownership with a comment; labels like
  `needs-review` route work between agents and sessions.
- **Attention hooks.** `kata init --with-hooks` installs session lifecycle
  hooks for Claude Code and Codex so the ledger knows which issues have an
  agent on them right now.

## 04 / Discipline — a close is a completion claim

- **Evidence.** `kata close --done` takes `--commit`, `--test`, and typed
  `--evidence`; "done" points at a verifiable artifact.
- **Attribution.** Humans and agents write under distinct identities.
- **Review.** `kata audit closes` replays close activity; `kata reopen` undoes
  what doesn't survive review. Parents cannot close over open children.
- **Idempotency.** Search-before-create plus idempotency keys make retries
  resume instead of duplicating.

## 05 / Supervision — humans steer over the same ledger

`kata tui` keeps triage in the terminal; `kata ui` opens the daemon's own
browser workspace — projects, collections, relationships, recurrences, and
issue editing — over the same data.

## 06 / Interfaces — one ledger across every surface

CLI (`--agent`/`--json`), TUI, Web UI, MCP server (stdio or HTTP, with
on-demand tool loading), a versioned HTTP API, and `go.kenn.io/kata` for
running the whole service in-process.

## 07 / Sync — in sync everywhere, local-first everywhere

With federation, every machine runs its own daemon, and every agent works
against that daemon at millisecond speed. Daemons replicate shared projects
through a hub in the background — daemon to daemon, off the critical path. A
SaaS tracker puts the network between the agent and every single read; kata
puts it between daemons, where nobody is waiting on it.

## 08 / Reach — local first, shared when you choose

Private-network daemons with bearer auth, GitHub issue sync, shared local
projects with per-actor attribution, and hosted mode behind the `$PORT`
convention with PostgreSQL when SQLite isn't enough.

## 09 / Boundary — not a Linear replacement, a different layer

SaaS trackers coordinate companies. kata coordinates the machine doing the
work. Keep roadmaps and reporting in the hosted tracker; give the working loop
a ledger that answers in a millisecond.

## 10 / Start

- [The guide](https://katatracker.com/guide.md): the nine-move operating loop.
- [Documentation](https://katatracker.com/docs/): commands, configuration,
  architecture.
- [GitHub](https://github.com/kenn-io/kata): source, issues, releases. MIT.
