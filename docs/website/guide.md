# The kata: a nine-stop tour of the operating loop

A kata is a form you practice until it disappears. These nine moves are the
loop that humans and coding agents run against the ledger until they become
habits. Every stop links to the [documentation](https://katatracker.com/docs/)
for exact commands and configuration.

## 01 / Bind the workspace

`kata init` writes a small, secret-free `.kata.toml` that binds your repo,
including every clone and worktree of it, to one kata project. Issue state
lives in SQLite under `KATA_HOME`, so your code history stays clean of tracker
churn.

`kata init --with-agents` also writes kata's operating contract into
`AGENTS.md`/`CLAUDE.md`, and `kata quickstart` prints it on demand. Every
agent that opens the repo already knows the form.

→ [Quickstart](https://katatracker.com/docs/get-started/quickstart/),
[Workspaces and projects](https://katatracker.com/docs/guide/workspaces-projects/)

## 02 / Put the plan in the ledger

Work that only exists in a conversation disappears with the conversation.
Search before creating an issue, then create it with an idempotency key so a
retried run resumes instead of duplicating.

Store structure as data. Use `--parent` to group child issues under a parent,
`--blocks` and `--blocked-by` for ordering, and `--related` for context.

→ [Agent workflows](https://katatracker.com/docs/workflows/agents/),
[Concepts](https://katatracker.com/docs/guide/concepts/)

## 03 / Claim one issue, loudly

In a multi-agent workspace, `kata next --unowned` applies the shared priority
rules and returns at most one candidate. `kata claim` takes ownership and fails
if someone got there first. Treat a failed claim as a signal to run `next`
again and pick different work.

Claim with `--ttl` and renew before expiry by claiming again, without
`--if-unowned`.

A claim costs about 20ms end to end, so agents check and re-check constantly.
The ledger stays truthful because staying truthful is cheap.

→ [The claim flow](https://katatracker.com/docs/workflows/agents/#claim-work)

## 04 / Write notes the next session can use

Comment with findings a later reader needs. Before a long pause, a context
compaction, or a handoff, put what remains in `work.attention_msg`, where the
next actor will actually look. Planning state is native: `kata schedule` parks
until a date, `kata deadline` presses without parking, `someday` shelves an
idea without losing it.

→ [Durable notes](https://katatracker.com/docs/workflows/agents/#keep-durable-notes)

## 05 / Fan out, then join on the event stream

An orchestrator, agent or human, creates child issues under a tracking parent
and launches workers on their own branches. Workers claim, comment on, and
close issues against the same ledger, while the orchestrator follows
`kata events --tail` and returns the moment a sub-task closes or needs a human.

Every mutation lands on a durable, cursor-addressable stream, so a supervisor
that was away replays exactly what it missed. Session hooks keep attention
truthful, and the ledger knows which issues have an agent on them right now.

Give workers distinct teammate handles to record their contributions under the
coordinating actor. Use `kata notify` to request attention and `kata inbox` to
read requests, clearing each request after you handle it. When a worker should
access only its assigned issue and descendants, create an expiring issue-scoped
credential on a daemon in token identity mode.

→ [Agent orchestration](https://katatracker.com/docs/operations/agent-orchestration/)
· [Teammate requests](https://katatracker.com/docs/workflows/agents/#teammate-heads-up)
· [Worker credentials](https://katatracker.com/docs/operations/remote-daemon/#identity-tokens)

## 06 / Close only what you can prove

Closing is a completion claim, and claims need evidence: real prose plus a
commit, a test command, or typed evidence a reviewer can check later.
Incomplete work stays open, with `work.attention` and `work.attention_msg`
updated and a `needs-review` label, instead of a close. `kata audit closes` replays close activity; `kata reopen` undoes what
doesn't hold up.

→ [Close discipline](https://katatracker.com/docs/guide/concepts/#close-discipline)

## 07 / Steer from the terminal or the browser

Run `kata tui` to triage issues in the terminal, or run `kata ui` to open the
daemon's own browser workspace with projects, collections, editing,
recurrences, and a relationship graph of how the work connects.

Both tools read the same database that agents write to, so there is nothing to
sync and nothing to export.

→ [Web UI guide](https://katatracker.com/docs/guide/web-ui/)

## 08 / Sync daemons, not keystrokes

Federation keeps a team of agents in sync without giving up the local loop.
Each machine runs its own daemon, and agents and humans only talk to the daemon
beside them. Spoke daemons enroll with a hub and replicate shared projects in
the background, so reads never wait on the network.

The other sharing models are deliberate too. You can serve one daemon over a
private network with bearer auth, mirror GitHub issues into the ledger, or run
hosted behind `$PORT`.

→ [Federation](https://katatracker.com/docs/operations/federation/),
[Sharing models](https://katatracker.com/docs/workflows/sharing/)

## 09 / Own the ledger outright

Your entire record, including issues, comments, events, and attribution, lives
in a SQLite database under `KATA_HOME` on hardware you control. Run
`kata export` to write it as JSONL for backup, and run `kata import` to restore
or merge it anywhere, with no vendor in the loop.

kata is MIT-licensed, ships as a single Go binary, and belongs to you. The
tracker your agents depend on should not live behind someone else's API key.

→ [Backup and restore](https://katatracker.com/docs/operations/backup-restore/)

## After the tour

Install kata, bind a repo, and run `kata init --with-agents` to teach the loop
to every agent that opens it. The documentation covers the exact commands,
configuration, and architecture.

→ [Start in five minutes](https://katatracker.com/docs/get-started/quickstart/)
