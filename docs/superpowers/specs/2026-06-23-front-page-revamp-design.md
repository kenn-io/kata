# Front-page revamp: marketing pitch + quickstart

- **Date:** 2026-06-23
- **Status:** Approved (pending spec review)
- **Scope:** `docs/index.md` (katatracker.com homepage) and `README.md`;
  small additive CSS in `docs/stylesheets/extra.css`.

## Problem

The current front page (`docs/index.md`) explains kata's *shape* (CLI, daemon,
SQLite, TUI) before it tells a new reader what kata is *for* or why they'd want
it, and it has **no install commands**. A reader has to assemble the value from
architecture bullets. kata now has one-line installers for macOS, Linux, and
Windows, but neither the homepage nor the top of the page surfaces them.

## Goal

Turn the homepage into a marketing pitch + quickstart that:

1. States what kata is and who it's for in the first screen (agent-first hook).
2. Surfaces install commands for all three platforms near the top of the page.
3. Gives a minimal end-to-end quickstart on the page itself.
4. Answers the two first objections ("where does state live?", "is this
   replacing Linear/Jira/GitHub Issues?") tightly, without becoming a guide.
5. Points to the deeper docs.

`README.md` is brought into the same story order so the repo and site feel
intentional.

## Positioning

Lead with the **agent-first hook**, immediately pulling in humans and
local-first. Plain, factual language. No "powerful / seamless / comprehensive".

## Page structure (`docs/index.md`, top → bottom)

### Frontmatter

```yaml
---
title: kata カタ
description: Local-first issue tracking for humans and coding agents.
hide:
  - toc
---
```

`hide: [toc]` removes the right-hand in-page table of contents so the landing
reads cleanly. Left nav stays. **Verify it renders in the modern variant; drop
the `hide` block if unsupported.** Keep the `title`/`description` either way
(matches `changelog.md`).

### 1. Hero

Wrapped in `<div class="kata-hero" markdown>` (uses `md_in_html`) so the small
CSS can center the headline + button row and constrain the pitch width.

```markdown
# kata カタ

### The issue tracker built for coding agents and the humans steering them.

Coding agents need somewhere durable to track work: not a chat thread, not a
markdown to-do list. kata gives them a local task ledger they can drive from the
CLI: create, claim, relate, and close issues with evidence. Humans supervise the
same work in a terminal UI. By default, issue state lives in a local SQLite
database, so your repo stays clean and no hosted tracker is required.

[Install](#install){ .md-button .md-button--primary }
[Quickstart](#quickstart){ .md-button }
```

Followed by the existing TUI hero screenshot and its provenance caption:

```markdown
![kata TUI showing a simulated issue hierarchy](/assets/screenshots/tui/hero.svg)

The image above is generated from disposable simulated data by the docs
screenshot workflow.
```

Note the precise local-first claim ("By default … no hosted tracker is
required"). kata has opt-in remote-daemon and federation paths, so we do not
claim state is *always* local.

### 2. Install (`#install`)

Heading `## Install` (auto-anchors to `#install`, matching the hero button).
Content tabs via `pymdownx.tabbed`:

````markdown
=== "macOS / Linux"

    ```sh
    curl -fsSL https://katatracker.com/install.sh | bash
    ```

=== "Windows (PowerShell)"

    ```powershell
    powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
    ```
````

Then, tight prose + verify (no hard-coded version number, `kata version` only,
confirmed to be the correct command; `kata --version` is not a valid flag):

````markdown
The installer detects your OS and CPU architecture, downloads the latest release
archive, and verifies it against `SHA256SUMS` before installing.

```sh
kata version
```

Prefer `go install`, `.deb`/`.rpm` packages, or building from source? See
[Install](get-started/install.md).
````

### 3. Why kata (4 grid cards)

`## Why kata`, then a `grid cards` block (`attr_list` + `md_in_html`). Four
cards, mapping to the promise (agents, humans, local storage, evidence/audit):

```markdown
<div class="grid cards" markdown>

-   __Built for agents__

    Stable short refs, `--json` and `--agent` output, idempotent creates, a
    claim flow, and predictable failure modes agents can script against.

-   __Made for humans too__

    `kata tui` browses, triages, and supervises agent-written work over the same
    data. No raw JSON required.

-   __Local-first, repo-clean__

    One Go binary, no runtime dependencies. Issue state lives in SQLite under
    `KATA_HOME`; your repo commits only a small, secret-free `.kata.toml`.

-   __Auditable by design__

    Closing an issue is an explicit completion claim with a reason, message,
    evidence, and actor attribution, on top of append-only comments and durable
    events.

</div>
```

### 4. Quickstart (`#quickstart`)

`## Quickstart`, minimal end-to-end flow on the page:

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

Followed by the agent pointer + link to the full page:

```markdown
`kata create` prints each issue's short id; use it in later commands. Working
with coding agents? `kata init --with-agents` drops kata's operating contract
into `AGENTS.md`/`CLAUDE.md`, and `kata quickstart` prints the full agent
contract. See the [Quickstart](get-started/quickstart.md) for the complete walk
through.
```

### 5. How it works (tight)

`## How it works`. 2-3 sentences answering "where does state live?", preserving
the substance of today's "Architecture" section without the full detail:

```markdown
The `kata` CLI resolves a project from your workspace, `.kata.toml`, or
`--project`, then talks to a local daemon, starting one automatically when
needed. The daemon owns a SQLite database under `KATA_HOME`, applies mutations,
and records an event stream that both the CLI/TUI and hooks read. Your repo
commits only the small `.kata.toml` binding, so issue history stays out of code
history. See [Concepts](guide/concepts.md) and
[Architecture](design/architecture.md) for the full model.
```

### 6. When to use kata (tight)

`## When to use kata`. Condensed bullets answering "is this replacing
Linear/Jira/GitHub Issues?", ending in the Comparisons link:

```markdown
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
```

### 7. Next steps (4 link cards)

`## Next steps`, grid cards linking out:

```markdown
<div class="grid cards" markdown>

-   [__Concepts__](guide/concepts.md). The data model and how the pieces fit.
-   [__CLI reference__](reference/cli.md). Every command and flag.
-   [__Agent workflows__](workflows/agents.md). The operating contract for agents.
-   [__Comparisons__](guide/comparisons.md). kata vs. SaaS issue trackers.

</div>
```

### 8. Status note

Keep the pre-1.0 honesty as a short admonition near the bottom (or directly
under Install):

```markdown
!!! note "Pre-1.0"
    kata publishes versioned pre-1.0 releases. The CLI, daemon, and TUI are
    usable, but command contracts and UI details can still change before a
    stable release.
```

## README alignment

Restructure `README.md` to the same story order: agent-first pitch → install
(macOS/Linux + Windows) → short quickstart → why bullets → docs links → agent
note → contributing. README is plain GitHub markdown. **No tabs, cards, or
buttons** (they don't render on GitHub). Install commands, the hero paragraph,
and positioning match the site. Keep the existing "How kata compares" and Beads
paragraphs (they're already good); reorder so install sits high and the
agent-first hook leads. Use `kata version` for the verify step.

## CSS (additive, in `docs/stylesheets/extra.css`)

Small and scoped: spacing, readable line length, CTA layout only. No heavy hero
graphics.

```css
.md-typeset .kata-hero {
  text-align: center;
}

.md-typeset .kata-hero > p {
  max-width: 42rem;
  margin-inline: auto;
  text-align: left;          /* keep the long pitch scannable */
}

.md-typeset .kata-hero .md-button {
  margin: 0.4rem 0.3rem 0;   /* space the two CTAs as a centered row */
}
```

Headline (`h1`) and tagline (`h3`) center via the wrapper; the pitch paragraph
stays left-aligned inside a centered, width-constrained box; the two buttons
center as a spaced row. If the centered headline does not scan well at build
time, fall back to default left alignment; the rest of the page is unaffected.

## Technical notes

- All primitives are pure markdown on **already-enabled** Zensical 0.0.43
  extensions, verified rendering to correct Material classes via a build probe:
  `pymdownx.tabbed` (`tabbed-set`), `attr_list` + `md_in_html` (`grid cards`,
  `md-button`), `admonition`. **No custom templates, no new dependencies.**
- CTA buttons link to on-page anchors generated from the `## Install` and
  `## Quickstart` headings.
- The spec lives under `docs/superpowers/`, which `zensical-docs.sh` excludes
  from the build, so it will not appear on the site or trip the strict build.

## Verification

- `make docs-build`: strict Zensical build passes (no broken links, no orphan
  pages, valid nav).
- `make docs-check`: repo docs-structure check + strict build pass.
- Confirm the two hero buttons resolve to the on-page `#install` / `#quickstart`
  anchors, and that all internal links resolve.
- README: confirm relative links resolve; it is plain markdown.
- Optional visual pass with `make docs-serve`.

## Out of scope (YAGNI)

- No new screenshots or asset generation (reuse `tui/hero.svg`).
- No nav restructure, logo/branding, or analytics.
- No per-distro install tabs (the curl command is identical for macOS & Linux).
- No heavy hero/banner CSS beyond the spacing/width/CTA rules above.
