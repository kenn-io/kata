# Front-Page Revamp Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the docs homepage (`docs/index.md`) and `README.md` into a marketing pitch + quickstart with per-platform install, an agent-first hook, feature cards, and an on-page quickstart.

**Architecture:** Pure-markdown landing page on Zensical's already-enabled extensions (`pymdownx.tabbed`, `attr_list` + `md_in_html`, `admonition`) plus a small additive `.kata-hero` block in `docs/stylesheets/extra.css`. No custom templates, no new dependencies. README mirrors the same story in plain GitHub markdown. Verification is the strict Zensical build plus rendered-HTML and link checks; styling gets a manual visual pass.

**Tech Stack:** Markdown, Zensical 0.0.43 (Material-derived), CSS, `make docs-build` / `make docs-check`.

## Global Constraints

Every task's requirements implicitly include this section.

- Spec: `docs/superpowers/specs/2026-06-23-front-page-revamp-design.md`.
- Voice: plain, factual. Do not use "powerful / seamless / comprehensive / robust / elegant / critical".
- Verify command is exactly `kata version` (NOT `kata --version`, which is an invalid flag). No hard-coded version number anywhere.
- Install commands, verbatim:
  - macOS / Linux: `curl -fsSL https://katatracker.com/install.sh | bash`
  - Windows (PowerShell): `powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"`
- Source-install floor is **Go 1.26 or later**.
- `docs/index.md` may use Material features (tabs, grid cards, buttons, admonitions). `README.md` MUST NOT use them; it is plain GitHub markdown (no `=== "`, no `grid cards`, no `{ .md-button }`, no `!!!`).
- Reuse the existing `/assets/screenshots/tui/hero.svg`. Do not generate new assets. Do not restructure nav.
- Hero pitch paragraph copy is fixed (see Task 2). Do not reword.
- `docs/site/` is build output and is NOT gitignored. NEVER `git add` it or `git add -A`. Stage only the named files. Remove `docs/site/` with `trash` after verification.
- Commit at the end of every task that changes files. Always a NEW commit (never `--amend`). End commit messages with:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
- The verification greps below are one-time implementation checks run against build output; do NOT commit them as repository tests (the project bans brittle content-assertion tests).

---

### Task 1: Hero CSS in `docs/stylesheets/extra.css`

Small additive rules that center the hero headline + CTA row and constrain the pitch width. Task 2 consumes the `.kata-hero` class.

**Files:**
- Modify: `docs/stylesheets/extra.css` (append at end, after the `.md-grid` rule)

**Interfaces:**
- Consumes: nothing.
- Produces: CSS contract for a `<div class="kata-hero">` wrapper. It styles a centered block, its `> p` (pitch paragraph, left-aligned + width-capped), and `.md-button` children (spaced CTA row). Task 2 wraps the hero in exactly `<div class="kata-hero" markdown>`.

- [ ] **Step 1: Ensure the docs toolchain is installed**

The build needs `docs/.venv`. If it is missing, install it.

Run:
```bash
cd /Users/wesm/code/kata
test -x docs/.venv/bin/zensical && echo "toolchain present" || make docs-install
```
Expected: `toolchain present` (or a successful `uv sync`).

- [ ] **Step 2: Confirm the class is not styled yet (red)**

Run:
```bash
cd /Users/wesm/code/kata
rg -n "kata-hero" docs/stylesheets/extra.css || echo "absent (red) ✓"
```
Expected: `absent (red) ✓`.

- [ ] **Step 3: Append the hero rules**

Append exactly this to the end of `docs/stylesheets/extra.css`:
```css

.md-typeset .kata-hero {
  text-align: center;
}

.md-typeset .kata-hero > p {
  max-width: 42rem;
  margin-inline: auto;
  text-align: left;
}

.md-typeset .kata-hero .md-button {
  margin: 0.4rem 0.3rem 0;
}
```

- [ ] **Step 4: Build and confirm the rule ships (green)**

Run:
```bash
cd /Users/wesm/code/kata
make docs-build
rg -c "kata-hero" docs/site/stylesheets/extra.css
```
Expected: `make docs-build` ends with `Build finished`; the `rg -c` prints `3` (three rules reference `.kata-hero`). The visual effect is verified in Task 4, once the hero markup exists.

- [ ] **Step 5: Commit**

Stage only the stylesheet (never `docs/site/`):
```bash
cd /Users/wesm/code/kata
git add docs/stylesheets/extra.css
git commit -m "$(printf '%s\n' 'Add hero block styles for the landing page' '' 'Center the headline and CTA row and cap the pitch paragraph width' 'for the new docs front page.' '' 'Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```
Expected: one new commit; `git status` shows no staged `docs/site/`.

---

### Task 2: Rewrite `docs/index.md` as the landing page

The full marketing pitch + quickstart. Replaces the entire current file.

**Files:**
- Modify (replace whole file): `docs/index.md`

**Interfaces:**
- Consumes: `.kata-hero` CSS from Task 1; existing asset `/assets/screenshots/tui/hero.svg`; existing pages `get-started/quickstart.md`, `get-started/install.md`, `guide/concepts.md`, `guide/comparisons.md`, `design/architecture.md`, `reference/cli.md`, `workflows/agents.md`.
- Produces: on-page anchors `#install` and `#quickstart` (from the `## Install` and `## Quickstart` headings) that the hero buttons target.

- [ ] **Step 1: Capture the "before" state (red)**

Build the current site and confirm the homepage has none of the new structures yet.

Run:
```bash
cd /Users/wesm/code/kata
make docs-build
echo "tabbed-set: $(rg -c 'tabbed-set' docs/site/index.html || echo 0)"
echo "grid cards: $(rg -c 'class="grid cards"' docs/site/index.html || echo 0)"
echo "install anchor: $(rg -c 'id="install"' docs/site/index.html || echo 0)"
```
Expected (red): `tabbed-set: 0`, `grid cards: 0`, `install anchor: 0`.

- [ ] **Step 2: Replace `docs/index.md` with the landing page**

Write `docs/index.md` with exactly this content:
`````markdown
---
title: kata カタ
description: Local-first issue tracking for humans and coding agents.
hide:
  - toc
---

<div class="kata-hero" markdown>

# kata カタ

### The issue tracker built for coding agents and the humans steering them.

Coding agents need somewhere durable to track work: not a chat thread, not a
markdown to-do list. kata gives them a local task ledger they can drive from the
CLI: create, claim, relate, and close issues with evidence. Humans supervise the
same work in a terminal UI. By default, issue state lives in a local SQLite
database, so your repo stays clean and no hosted tracker is required.

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

The installer detects your OS and CPU architecture, downloads the latest release
archive, and verifies it against `SHA256SUMS` before installing.

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
and records an event stream that both the CLI/TUI and hooks read. Your repo
commits only the small `.kata.toml` binding, so issue history stays out of code
history. See [Concepts](guide/concepts.md) and
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
-   [__Agent workflows__](workflows/agents.md). The operating contract for agents.
-   [__Comparisons__](guide/comparisons.md). kata vs. SaaS issue trackers.

</div>
`````

- [ ] **Step 3: Build and confirm the structures rendered (green)**

Run:
```bash
cd /Users/wesm/code/kata
make docs-build
echo "tabbed-set:    $(rg -c 'tabbed-set' docs/site/index.html)"
echo "grid cards:    $(rg -c 'class="grid cards"' docs/site/index.html)"
echo "md-button:     $(rg -c 'md-button' docs/site/index.html)"
echo "admonition:    $(rg -c 'admonition' docs/site/index.html)"
echo "install anchor:  $(rg -c 'id="install"' docs/site/index.html)"
echo "quickstart anchor: $(rg -c 'id="quickstart"' docs/site/index.html)"
```
Expected: `make docs-build` ends with `Build finished` and `No issues found` (strict build, so links resolved). `tabbed-set` ≥ 1; `grid cards` = 2; `md-button` ≥ 2; `admonition` ≥ 1; `install anchor` ≥ 1; `quickstart anchor` ≥ 1.

If the build FAILS on the `hide:` frontmatter, see Step 4.

- [ ] **Step 4: Verify the TOC is hidden; fall back if unsupported**

Run:
```bash
cd /Users/wesm/code/kata
rg -c 'md-nav--secondary' docs/site/index.html || echo "no secondary toc nav (hidden) ✓"
```
Expected: `no secondary toc nav (hidden) ✓` (or `0`).

If the strict build in Step 3 errored on the `hide:` block, OR the count above is non-zero (TOC still renders) and that is unwanted: remove the three frontmatter lines
```text
hide:
  - toc
```
from `docs/index.md`, keep `title`/`description`, and re-run Step 3 (it must still pass, minus the TOC check).

- [ ] **Step 5: Commit**

```bash
cd /Users/wesm/code/kata
git add docs/index.md
git commit -m "$(printf '%s\n' 'Rewrite the docs front page as a landing page' '' 'Lead with the agent-first pitch, add per-platform install tabs, four' 'feature cards, an on-page quickstart, and tightened how-it-works and' 'when-to-use sections.' '' 'Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```
Expected: one new commit; `git status` shows no staged `docs/site/`.

---

### Task 3: Restructure `README.md` to the same story

Reorder the README to lead agent-first, put install high, and add `kata version`, while keeping the existing "How kata compares" and Beads paragraphs. Plain GitHub markdown only.

**Deliberate change to accept:** the old detailed "What kata does" capability list is replaced by the four "Why kata" bullets; the full capability surface stays discoverable via the Documentation links. Confirm this is acceptable when reviewing.

**Files:**
- Modify (replace whole file): `README.md`

**Interfaces:**
- Consumes: existing files referenced by relative links (`docs/...`, `LICENSE`).
- Produces: nothing downstream.

- [ ] **Step 1: Confirm the new hook is not present yet (red)**

Run:
```bash
cd /Users/wesm/code/kata
rg -n "built for coding agents" README.md || echo "absent (red) ✓"
```
Expected: `absent (red) ✓`.

- [ ] **Step 2: Replace `README.md` with the aligned version**

Write `README.md` with exactly this content:
`````markdown
# kata カタ

The issue tracker built for coding agents and the humans steering them.

Coding agents need somewhere durable to track work: not a chat thread, not a
markdown to-do list. kata gives them a local task ledger they can drive from the
CLI: create, claim, relate, and close issues with evidence. Humans supervise the
same work in a terminal UI. By default, issue state lives in a local SQLite
database, so your repo stays clean and no hosted tracker is required.

The documentation in [`docs/`](docs/) is the definitive guide, published with
Zensical at <https://katatracker.com/>.

> **Pre-1.0:** kata publishes versioned pre-1.0 releases. The CLI, daemon, and
> TUI are usable, but command contracts and UI details can still change before a
> stable release.

## Install

macOS or Linux:

```sh
curl -fsSL https://katatracker.com/install.sh | bash
```

Windows PowerShell:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

The installer detects your OS and CPU architecture, downloads the latest GitHub
release archive, and verifies it against `SHA256SUMS` before installing. Confirm
the install with:

```sh
kata version
```

Release builds update themselves with `kata update`. Linux `.deb` and `.rpm`
packages are published for `amd64` and `arm64`. Prefer to build from source?
kata needs **Go 1.26 or later**:

```sh
go install go.kenn.io/kata/cmd/kata@latest
```

Go installs to `$(go env GOBIN)`, falling back to `$(go env GOPATH)/bin` (often
`~/go/bin`); put that directory on your `PATH`. See
[Install](docs/get-started/install.md) for package downloads, manual release
downloads, and build-from-source steps.

## Quickstart

```sh
cd your-repo
kata init                              # bind this workspace to a kata project
kata create "fix login race"           # prints a short id, e.g. abc4
kata list                              # see open work
kata show abc4                         # inspect by short id
kata tui                               # browse and triage interactively
```

`kata create` prints each issue's short id; use it in later commands. Close only
when the work is complete and verified:

```sh
kata close abc4 --done \
  --message "Fixed the login race and verified the relevant tests pass." \
  --commit <sha>
```

For agent-heavy workspaces, `kata init --with-agents` also writes a managed kata
briefing into agent guidance files. It refreshes existing real `AGENTS.md` and
`CLAUDE.md` files, or creates `AGENTS.md` when neither exists, without
overwriting the rest of either file.

## Why kata

- **Built for agents.** Stable short refs, `--json` and `--agent` output,
  idempotent creates, a claim flow, and predictable failure modes agents can
  script against.
- **Made for humans too.** `kata tui` browses, triages, and supervises
  agent-written work over the same data. No raw JSON required.
- **Local-first, repo-clean.** One Go binary, no runtime dependencies. Issue
  state lives in SQLite under `KATA_HOME`; your repo commits only a small,
  secret-free `.kata.toml`.
- **Auditable by design.** Closing an issue is an explicit completion claim with
  a reason, message, evidence, and actor attribution, on top of append-only
  comments and durable events.

## How kata compares

kata is intentionally small. It is not a project-management suite, a git
workflow engine, or an agent worker pool. It is a durable task ledger that
humans and agents can both operate.

It is also not a SaaS issue tracker. Linear, Jira, GitHub Issues, ClickUp, and
similar tools are shared online systems for planning, dashboards, assignment,
and cross-team reporting. kata is local-first, instant from the CLI/TUI, and
designed around agent-first ergonomics: stable refs, predictable output,
idempotent creates, claim flows, and evidence-based closes. See
[Comparisons with SaaS issue trackers](docs/guide/comparisons.md) for the
matrix.

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

## Documentation

The [docs site](docs/) is the definitive reference:

- Get started: [Quickstart](docs/get-started/quickstart.md) ·
  [Install](docs/get-started/install.md) ·
  [Changelog](docs/changelog.md)
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
verified. Close each verified issue promptly with valid evidence and a
substantive message. [Agent workflows](docs/workflows/agents.md) is the same
contract in long form.

## Contributing

See [Contributing](docs/development/contributing.md) for the repository layout
and local checks (`make test`, `make lint`, `make vet`, `make nilaway`).
Licensed under the terms in [LICENSE](LICENSE).
`````

- [ ] **Step 3: Verify content, absence of Material-only syntax, and links (green)**

Run:
```bash
cd /Users/wesm/code/kata
echo "--- required content present ---"
rg -q "built for coding agents" README.md && echo "hook ✓"
rg -q "curl -fsSL https://katatracker.com/install.sh \| bash" README.md && echo "unix install ✓"
rg -q "irm https://katatracker.com/install.ps1 \| iex" README.md && echo "windows install ✓"
rg -q "^kata version$" README.md && echo "verify cmd ✓"
echo "--- Material-only syntax must be ABSENT ---"
if rg -nq '^=== "|class="grid cards"|\{ \.md-button|^!!!' README.md; then
  echo "FOUND Material-only syntax ✗"; else echo "none ✓"; fi
echo "--- local links resolve ---"
miss=0
while IFS= read -r t; do
  [ -e "$t" ] || { echo "MISSING: $t"; miss=1; }
done < <(rg -o '\]\(([^)]+)\)' -r '$1' README.md | grep -Ev '^https?:|^#' | sort -u)
[ "$miss" -eq 0 ] && echo "all local links resolve ✓"
```
Expected: every `✓` line prints; `none ✓` for Material syntax; `all local links resolve ✓` with no `MISSING` lines.

- [ ] **Step 4: Commit**

```bash
cd /Users/wesm/code/kata
git add README.md
git commit -m "$(printf '%s\n' 'Align README with the new front-page story' '' 'Lead agent-first, move install up with per-platform commands and' 'kata version, and summarize value as why bullets while keeping the' 'comparison and Beads notes.' '' 'Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```
Expected: one new commit.

---

### Task 4: Full-site verification and cleanup

Final gate over the combined change. Verification only. Commits nothing unless a fix is needed.

**Files:** none (runs builds, then removes build output).

**Interfaces:** consumes Tasks 1-3.

- [ ] **Step 1: Strict build of the final state**

Run:
```bash
cd /Users/wesm/code/kata
make docs-build
```
Expected: ends with `No issues found` and `Build finished`.

- [ ] **Step 2: Comprehensive repo docs check (CI-equivalent)**

Run:
```bash
cd /Users/wesm/code/kata
make docs-check
```
Expected: exits 0. This also asserts `docs/site/superpowers` is never generated and that the design pages still build. If `docs/screenshots/hydrate-assets.sh` cannot run in this environment (needs the assets branch / network), skip this step and rely on Step 1's strict build; note the skip in the task report.

- [ ] **Step 3: Manual visual pass**

Run:
```bash
cd /Users/wesm/code/kata
make docs-serve
```
Open the served homepage and confirm by eye: the headline and the two CTA buttons are centered as a spaced row; the pitch paragraph is left-aligned and not full-width; the Install tabs switch between macOS/Linux and Windows; the "Why kata" section shows four cards; "Next steps" shows four link cards; the Install and Quickstart buttons jump to their sections; there is no right-hand table of contents. Stop the server (Ctrl-C) when done.

- [ ] **Step 4: Remove build output and confirm a clean tree**

Run:
```bash
cd /Users/wesm/code/kata
trash docs/site 2>/dev/null || true
git status --short
```
Expected: `git status --short` prints nothing (no stray `docs/site/`, no uncommitted changes). No commit is made in this task.

---

## Self-Review

- **Spec coverage:** Positioning/hero → Task 2 Step 2 hero block. Install tabs + `kata version` + source pointer → Task 2 (Install) and Task 3 (README). 4 feature cards → Task 2 "Why kata". On-page quickstart → Task 2 "Quickstart". How it works / When to use → Task 2. Next steps cards → Task 2. Pre-1.0 note → Task 2 admonition + README blockquote. `hide: [toc]` with fallback → Task 2 Step 4. CSS → Task 1. README alignment → Task 3. Verification (`make docs-build`, `make docs-check`, link/anchor resolution, visual) → Tasks 2-4. No new assets / no nav change / no new deps → Global Constraints. All spec sections map to a task.
- **Placeholders:** none. Every file's full content and every command is inline.
- **Type/name consistency:** the `.kata-hero` class (Task 1) matches the `<div class="kata-hero" markdown>` wrapper (Task 2); anchor ids `#install`/`#quickstart` match the `## Install`/`## Quickstart` headings the buttons link to; install commands and `kata version` are identical across `docs/index.md` and `README.md`.
