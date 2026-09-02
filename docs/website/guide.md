# The kata — a nine-stop tour of the operating loop

A kata is a form you practice until it disappears. This is the operating loop
humans and coding agents run against the ledger — nine moves, each a habit
worth building. Every stop links into the
[documentation](https://katatracker.com/docs/) for exact commands.

## 01 / Bind the workspace

`kata init` writes a small, secret-free `.kata.toml` that binds your repo —
and every clone and worktree of it — to one kata project. Issue state lives in
SQLite under `KATA_HOME`, so code history stays clean of tracker churn.
`kata init --with-agents` drops the operating contract into
`AGENTS.md`/`CLAUDE.md`; `kata quickstart` prints it on demand.

→ [Quickstart](https://katatracker.com/docs/get-started/quickstart/),
[Workspaces and projects](https://katatracker.com/docs/guide/workspaces-projects/)

## 02 / Seed the ledger, not the chat

Search before creating; create with an idempotency key so a retried run
resumes instead of duplicating. Structure is data: `--parent` for hierarchy,
`--blocks`/`--blocked-by` for ordering, `--related` for context.

→ [Agent workflows](https://katatracker.com/docs/workflows/agents/),
[Concepts](https://katatracker.com/docs/guide/concepts/)

## 03 / Claim one issue, loudly

`kata next --unowned` applies shared priority rules and returns at most one
candidate. `kata claim` takes ownership — and fails if someone got there
first. That failure is the coordination protocol telling you to pick different
work.

→ [The claim flow](https://katatracker.com/docs/workflows/agents/#claim-work)

## 04 / Write notes the next session can use

Before a pause, compaction, or handoff, record the decision, the partial
attempt, and what remains in a comment on the issue. Planning state is native:
`kata schedule` parks until a date, `kata deadline` presses without parking,
`someday` shelves without losing.

→ [Durable notes](https://katatracker.com/docs/workflows/agents/#keep-durable-notes)

## 05 / Fan out, then join on the event stream

An orchestrator creates child issues under a tracking parent and launches
workers. Workers claim, comment, and close against the same ledger; the
orchestrator follows `kata events --tail` and returns the moment a sub-task
closes or needs a human. Session hooks keep attention truthful.

→ [Agent orchestration](https://katatracker.com/docs/operations/agent-orchestration/)

## 06 / Close only what you can prove

Closing is a completion claim: substantive prose plus `--commit`, `--test`, or
typed `--evidence`. Attempted is not done — incomplete work gets a
`needs-review` label and an honest comment. `kata audit closes` replays close
activity; `kata reopen` undoes what doesn't hold up.

→ [Close discipline](https://katatracker.com/docs/guide/concepts/#close-discipline)

## 07 / Steer from the terminal or the browser

`kata tui` keeps triage in the terminal; `kata ui` opens the daemon's own
browser workspace — projects, collections, editing, recurrences, and a
relationship graph — over the same ledger the agents write.

→ [Web UI guide](https://katatracker.com/docs/guide/web-ui/)

## 08 / Sync daemons, not keystrokes

Federation keeps a team of agents in sync without giving up the instantaneous
local-first loop: each machine runs its own daemon, agents only ever talk to
the daemon beside them, and spoke daemons replicate shared projects through a
hub in the background. The network serves the daemons; nobody's read waits on
it. Other reach — private-network daemons, GitHub sync, hosted mode — is just
as deliberate.

→ [Federation](https://katatracker.com/docs/operations/federation/),
[Sharing models](https://katatracker.com/docs/workflows/sharing/)

## 09 / Own the ledger outright

The whole record is a SQLite database under `KATA_HOME` on hardware you
control. `kata export` writes it as JSONL; `kata import` restores or merges it
anywhere. MIT-licensed, one Go binary, yours.

→ [Backup and restore](https://katatracker.com/docs/operations/backup-restore/)

## After the tour

Install kata, bind a repo, and let `kata init --with-agents` teach the loop to
every agent that opens it.

→ [Start in five minutes](https://katatracker.com/docs/get-started/quickstart/)
