# The kata: a nine-stop tour of the operating loop

A kata is a form you practice until it disappears. These are the nine moves
humans and coding agents run against the ledger until they turn into habits.
Every stop links into the [documentation](https://katatracker.com/docs/) for
exact commands.

## 01 / Bind the workspace

`kata init` writes a small, secret-free `.kata.toml` that binds your repo,
including every clone and worktree of it, to one kata project. Issue state
lives in SQLite under `KATA_HOME`, so code history stays clean of tracker
churn. `kata init --with-agents` drops the operating contract into
`AGENTS.md`/`CLAUDE.md`; `kata quickstart` prints it on demand.

→ [Quickstart](https://katatracker.com/docs/get-started/quickstart/),
[Workspaces and projects](https://katatracker.com/docs/guide/workspaces-projects/)

## 02 / Put the plan in the ledger

Work that only exists in a conversation dies with the conversation. Search
before creating, then create with an idempotency key so a retried run resumes
instead of duplicating. Structure goes in as data: `--parent` for hierarchy,
`--blocks`/`--blocked-by` for ordering, `--related` for context.

→ [Agent workflows](https://katatracker.com/docs/workflows/agents/),
[Concepts](https://katatracker.com/docs/guide/concepts/)

## 03 / Claim one issue, loudly

`kata next --unowned` applies shared priority rules and returns at most one
candidate. `kata claim` takes ownership, and fails if someone got there first.
Treat the failure as a signal: run `next` again and take different work. A
claim costs about 20ms end to end, so agents check and re-check constantly.

→ [The claim flow](https://katatracker.com/docs/workflows/agents/#claim-work)

## 04 / Write notes the next session can use

Before a long pause, a context compaction, or a handoff, record the decision,
the partial attempt, and what remains in a comment on the issue, where the
next actor will actually look. Planning state is native: `kata schedule` parks
until a date, `kata deadline` presses without parking, `someday` shelves an
idea without losing it.

→ [Durable notes](https://katatracker.com/docs/workflows/agents/#keep-durable-notes)

## 05 / Fan out, then join on the event stream

An orchestrator creates child issues under a tracking parent and launches
workers. Workers claim, comment, and close against the same ledger; the
orchestrator follows `kata events --tail` and returns the moment a sub-task
closes or needs a human. Session hooks keep attention truthful.

→ [Agent orchestration](https://katatracker.com/docs/operations/agent-orchestration/)

## 06 / Close only what you can prove

Closing is a completion claim, and claims need evidence: real prose plus a
commit, a test command, or typed evidence a reviewer can check later.
Incomplete work gets a `needs-review` label and an honest comment instead of a
close. `kata audit closes` replays close activity; `kata reopen` undoes what
doesn't hold up.

→ [Close discipline](https://katatracker.com/docs/guide/concepts/#close-discipline)

## 07 / Steer from the terminal or the browser

`kata tui` keeps triage in the terminal. `kata ui` opens the daemon's own
browser workspace with projects, collections, editing, recurrences, and a
relationship graph. The supervision surfaces read the same database the
agents write, so there is nothing to sync and nothing to export.

→ [Web UI guide](https://katatracker.com/docs/guide/web-ui/)

## 08 / Sync daemons, not keystrokes

Federation keeps a team of agents in sync without giving up the local loop.
Each machine runs its own daemon, and agents and humans only ever talk to the
daemon beside them. Spoke daemons enroll with a hub and replicate shared
projects in the background, so nobody's read ever waits on the network. The
other sharing models are just as deliberate: private-network daemons with
bearer auth, GitHub issue sync, hosted mode behind `$PORT`.

→ [Federation](https://katatracker.com/docs/operations/federation/),
[Sharing models](https://katatracker.com/docs/workflows/sharing/)

## 09 / Own the ledger outright

The whole record, issues, comments, events, and attribution alike, is a
SQLite database under `KATA_HOME` on hardware you control. `kata export`
writes it as JSONL for backup. `kata import` restores or merges it anywhere,
no vendor in the loop. kata is MIT-licensed, a single Go binary, and yours.

→ [Backup and restore](https://katatracker.com/docs/operations/backup-restore/)

## After the tour

Install kata, bind a repo, and let `kata init --with-agents` teach the loop to
every agent that opens it.

→ [Start in five minutes](https://katatracker.com/docs/get-started/quickstart/)
