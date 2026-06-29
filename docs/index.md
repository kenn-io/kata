---
title: kata カタ
description: Local-first issue tracking for humans and coding agents.
---

<div class="kata-hero" markdown>

# kata カタ

<p class="kata-hero-tagline">The issue tracker built for coding agents and the humans steering them.</p>

Coding agents need somewhere durable to track work: not a chat thread, not a
markdown to-do list. kata gives them a local task ledger they can drive from the
CLI: create, claim, relate, and close issues with evidence. Humans supervise the
same work in a terminal UI. By default, issue state lives in a local SQLite
database, so your repo stays clean and no hosted tracker is required. When a team
of humans and agents needs to share, you can opt into a remote daemon or
federation.

[Install](#install){ .md-button .md-button--primary }
[Quickstart](#quickstart){ .md-button }

</div>

![kata TUI showing a simulated issue hierarchy](/assets/screenshots/tui/hero.svg)

The image above is generated from disposable simulated data by the docs
screenshot workflow.

## Install

=== "macOS / Linux"

    ```sh
    curl -fsSL https://katatracker.com/install.sh | bash
    ```

=== "Windows (PowerShell)"

    ```powershell
    powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
    ```

The installer detects your OS and CPU architecture, downloads the latest GitHub
release archive, and verifies it against `SHA256SUMS` before installing. Confirm
the install with:

```sh
kata version
```

Prefer `go install`, `.deb`/`.rpm` packages, or building from source? See
[Install](get-started/install.md).

!!! note "Pre-1.0"
    kata publishes versioned pre-1.0 releases. The CLI, daemon, and TUI are
    usable, but command contracts and UI details can still change before a
    stable release.

## Why kata

<div class="grid cards" markdown>

-   __Built for agents__

    Stable short refs, `--json` and `--agent` output, idempotent creates, a
    claim flow, semantic-aware search, and predictable failure modes agents can
    script against.

-   __Made for humans too__

    `kata tui` browses, triages, and supervises agent-written work over the same
    data. No raw JSON required.

-   __Local-first, repo-clean__

    One Go binary, no runtime dependencies. Issue state lives in SQLite under
    `KATA_HOME`; your repo commits only a small, secret-free `.kata.toml`.

-   __Auditable by design__

    Closing an issue is an explicit completion claim with a reason, message,
    evidence, and actor attribution, on top of editable comments and durable
    events.

</div>

## Quickstart

```sh
cd your-repo
kata init                              # bind this workspace to a kata project
kata create "fix login race"           # prints a short id, e.g. abc4
kata list                              # see open work

# close only when the work is verified
kata close abc4 --done \
  --message "Fixed the login race; tests pass." --commit <sha>

kata tui                               # browse and triage interactively
```

`kata create` prints each issue's short id; use it in later commands. Working
with coding agents? `kata init --with-agents` drops kata's operating contract
into `AGENTS.md`/`CLAUDE.md`, and `kata quickstart` prints the full agent
contract. See the [Quickstart](get-started/quickstart.md) for the complete
walkthrough.

## How it works

The `kata` CLI resolves a project from your workspace, `.kata.toml`, or
`--project`, then talks to a local daemon, starting one automatically when
needed. The daemon owns a SQLite database under `KATA_HOME`, applies mutations,
and records an event stream that both the CLI/TUI and hooks read. Search is
lexical by default and can opt into [semantic search](guide/semantic-search.md)
with a local or hosted OpenAI-compatible embeddings endpoint. Optional
[GitHub sync](operations/github-sync.md) can mirror upstream GitHub issues into
kata, and federation can replicate selected projects through a hub. Your repo
commits only the small `.kata.toml` binding, so issue history stays out of code
history. Private-network remote daemon modes are explicit: operators can use
bearer auth on trusted private HTTP or opt a single-user private IP into
tokenless writes. See [Concepts](guide/concepts.md) and
[Architecture](design/architecture.md) for the full model.

## When to use kata

Reach for kata when work should stay close to the machine doing it:

- coding agents need to discover, claim, update, and close work from the CLI;
- you want an instant terminal loop instead of a browser session;
- work spans local clones, worktrees, experiments, or non-git directories;
- task state should survive chat compaction without becoming a markdown plan;
- closes should carry evidence and an audit trail.

kata is not a SaaS issue tracker. Linear, Jira, GitHub Issues, and ClickUp are
shared online systems for roadmaps, dashboards, and cross-team reporting; kata
is a local ledger for the work itself. They coexist. See
[Comparisons](guide/comparisons.md) for the trade-offs.

## Next steps

<div class="grid cards" markdown>

-   [__Concepts__](guide/concepts.md). The data model and how the pieces fit.
-   [__CLI reference__](reference/cli.md). Every command and flag.
-   [__Semantic search__](guide/semantic-search.md). Improve issue discovery
    with opt-in embeddings.
-   [__GitHub sync__](operations/github-sync.md). Bring GitHub issues into kata.
-   [__Agent workflows__](workflows/agents.md). The operating contract for agents.
-   [__Comparisons__](guide/comparisons.md). kata vs. SaaS issue trackers.

</div>
