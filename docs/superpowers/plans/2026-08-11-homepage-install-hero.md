# Compact Homepage Install Rail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the homepage's large install card with the approved compact terminal rail while preserving platform detection, accessibility, copying, and the no-JavaScript fallback.

**Architecture:** Keep the existing platform detector and semantic `data-install-*` contract. Remove its obsolete status-message responsibility, reduce the homepage markup to a rail plus two text links, and use CSS decoration for the non-copyable prompt. Validate logic with the existing Vitest/jsdom harness and validate the rendered Zensical integration in a real browser.

**Tech Stack:** Zensical 0.0.43, Markdown with `md_in_html`, browser ES modules, CSS, Vitest 4.1.10, jsdom 29.1.1, Bun 1.3.14

## Global Constraints

- macOS uses `brew install kata`.
- Linux uses `curl -fsSL https://katatracker.com/install.sh | bash`.
- Windows uses `powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"`.
- Detect the browser platform on every homepage render; fall back to macOS and persist nothing.
- Keep macOS as the deliberate no-JavaScript fallback and keep the complete installation-guide link available.
- Render `$` as decoration outside the copied code text; copying must return only the executable command.
- Keep `QUICKSTART →` linked to the homepage `#quickstart` section.
- Remove the stability claim, install title and subtitle, detected-platform sentence, shadow, filled card background, and pill controls.
- Keep native platform buttons with distinct hover, focus, and selected states that meet dark-theme contrast requirements.
- Do not add source-content assertion tests or a committed Playwright suite.
- Do not change installers, packaging, the application web UI, the daemon, or persisted data.

---

### Task 1: Remove Obsolete Status Behavior

**Files:**

- Modify: `web/src/lib/docs/install-platform.test.js`
- Modify: `docs/javascripts/install-platform.js`

**Interfaces:**

- Consumes: `initializeInstallPanel(root: Document | Element, browserPlatform?: unknown): void`
- Preserves: button `aria-pressed`, content `hidden`, manual selection, per-render detection, and instant-navigation initialization
- Removes: mutation of `[data-install-platform-status]`

- [ ] **Step 1: Write the failing status-independence test**

Keep a status-shaped element in the test fixture as legacy markup, then replace
the status assertion in `applies the detected platform to accessible DOM state`
with:

```js
expect(
  document.querySelector("[data-install-platform-status]")?.textContent,
).toBe("legacy status");
```

Set the fixture text to `legacy status`. This defines the smaller behavior
contract: platform selection owns only buttons and command panels.

- [ ] **Step 2: Run the focused test and verify the red state**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
```

Expected: one failure because the current module rewrites the legacy status to
`Windows selected · choose another platform anytime.`

- [ ] **Step 3: Remove status-message production logic**

Delete `PLATFORM_LABELS` and the status lookup/update from
`selectInstallPlatform`. Do not change detection, selection, or initialization.

- [ ] **Step 4: Run the focused test and verify the green state**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
```

Expected: all 15 tests pass.

- [ ] **Step 5: Commit the smaller behavior contract**

Use the mandatory commit skill, then stage only the script and its test:

```bash
git add docs/javascripts/install-platform.js web/src/lib/docs/install-platform.test.js
git commit -m "refactor: limit the install selector to platform state"
```

---

### Task 2: Build the Compact Terminal Rail

**Files:**

- Modify: `docs/index.md`
- Modify: `docs/stylesheets/extra.css`

**Interfaces:**

- Consumes: `[data-install-panel]`, `[data-install-platform-button]`, and `[data-install-platform-content]`
- Produces: one thin install rail, CSS-only `$` prompt, and quiet links to `#quickstart` and `get-started/install.md`
- Preserves: SuperFence command blocks so Zensical supplies its copy control

- [ ] **Step 1: Replace the card markup with the compact rail**

Use this hero structure, keeping raw HTML flush-left so Zensical does not turn it
into a code block:

````markdown
<section class="kata-install" data-install-panel markdown>
<div class="kata-install-header">
<span class="kata-install-label">Install</span>
<div class="kata-install-platforms" role="group" aria-label="Choose your operating system">
<button type="button" data-install-platform-button="macos" aria-pressed="true">macOS</button>
<button type="button" data-install-platform-button="linux" aria-pressed="false">Linux</button>
<button type="button" data-install-platform-button="windows" aria-pressed="false">Windows</button>
</div>
</div>
```sh { .kata-install-command data-install-platform-content="macos" }
brew install kata
```

```sh { .kata-install-command .kata-install-command--fallback-hidden data-install-platform-content="linux" hidden="hidden" }
curl -fsSL https://katatracker.com/install.sh | bash
```

```powershell { .kata-install-command .kata-install-command--fallback-hidden data-install-platform-content="windows" hidden="hidden" }
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

</section>

<div class="kata-hero-actions">
<a href="#quickstart">Quickstart →</a>
<a href="get-started/install.md">All install options →</a>
</div>
````

Remove the card title, subtitle, detected-platform status, stability claim, and
rounded Quickstart button. Do not put `$` inside any fenced command.

- [ ] **Step 2: Replace card CSS with rail CSS**

Delete the existing `.kata-install*` card rules and its 44.984375em media query.
Add this complete rail treatment:

```css
.md-typeset .kata-install {
  max-width: 52rem;
  margin: 1.25rem auto 0;
  padding: 0;
  border: 1px solid
    color-mix(in srgb, var(--md-default-fg-color) 22%, transparent);
  border-radius: 2px;
  background: transparent;
  text-align: left;
}

.md-typeset .kata-install-header {
  display: flex;
  min-height: 2.5rem;
  align-items: stretch;
  justify-content: space-between;
  border-bottom: 1px solid
    color-mix(in srgb, var(--md-default-fg-color) 16%, transparent);
}

.md-typeset .kata-install-label {
  display: flex;
  align-items: center;
  padding: 0 0.9rem;
  color: var(--md-default-fg-color--light);
  font-family: var(--md-code-font);
  font-size: 0.62rem;
  font-weight: 700;
  letter-spacing: 0.12em;
  text-transform: uppercase;
}

.md-typeset .kata-install-platforms {
  display: flex;
  align-items: stretch;
}

.md-typeset .kata-install-platforms button {
  position: relative;
  min-width: 4.2rem;
  padding: 0 0.7rem;
  border: 0;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: var(--md-default-fg-color--light);
  cursor: pointer;
  font-family: var(--md-code-font);
  font-size: 0.62rem;
}

.md-typeset .kata-install-platforms button[aria-pressed="true"] {
  border-bottom-color: var(--md-primary-fg-color);
  color: var(--md-primary-fg-color);
}

.md-typeset .kata-install-platforms button[aria-pressed="false"]:hover {
  color: var(--md-default-fg-color);
}

.md-typeset .kata-install-platforms button:focus-visible {
  outline: 2px solid var(--md-accent-fg-color);
  outline-offset: -3px;
}

.md-typeset .kata-install-command[hidden],
.md-typeset .kata-install-command--fallback-hidden {
  display: none;
}

.md-typeset .kata-install-command.highlight {
  display: flex;
  min-height: 3.25rem;
  align-items: stretch;
  margin: 0;
  border-radius: 0;
  background: var(--md-code-bg-color);
}

.md-typeset .kata-install-command.highlight::before {
  display: flex;
  align-items: center;
  padding-left: 0.9rem;
  color: var(--md-primary-fg-color);
  content: "$";
  font-family: var(--md-code-font);
  font-size: 0.72rem;
  user-select: none;
}

.md-typeset .kata-install-command pre {
  min-width: 0;
  flex: 1;
  margin: 0;
  border-radius: 0;
  overflow-x: auto;
}

.md-typeset .kata-hero-actions {
  display: flex;
  justify-content: center;
  gap: 1.3rem;
  margin: 0.65rem auto 0;
  font-family: var(--md-code-font);
  font-size: 0.62rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
}

.md-typeset .kata-hero-actions a {
  color: var(--md-default-fg-color--light);
}

.md-typeset .kata-hero-actions a:hover,
.md-typeset .kata-hero-actions a:focus-visible {
  color: var(--md-primary-fg-color);
}

@media screen and (max-width: 30em) {
  .md-typeset .kata-install-header {
    flex-direction: column;
  }

  .md-typeset .kata-install-label {
    min-height: 2.2rem;
    border-bottom: 1px solid
      color-mix(in srgb, var(--md-default-fg-color) 16%, transparent);
  }

  .md-typeset .kata-install-platforms button {
    min-height: 2.35rem;
    flex: 1;
    padding-inline: 0.35rem;
  }

  .md-typeset .kata-install-command.highlight::before {
    padding-left: 0.65rem;
  }

  .md-typeset .kata-hero-actions {
    flex-wrap: wrap;
    gap: 0.45rem 1rem;
  }
}
```

- [ ] **Step 3: Run the focused behavior test and strict docs build**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
make docs-build
```

Expected: all focused tests pass and Zensical completes a strict build without
warnings or invalid links.

- [ ] **Step 4: Inspect the generated integration**

Run:

```bash
rg -n "kata-install-label|data-install-panel|All install options|Quickstart" docs/site/index.html
```

Expected: the built homepage contains the compact rail, both links, and all
platform controls. This is a diagnostic check of generated integration, not a
committed source-content test.

- [ ] **Step 5: Commit the homepage presentation**

Use the mandatory commit skill, then stage only the homepage and stylesheet:

```bash
git add docs/index.md docs/stylesheets/extra.css
git commit -m "docs: make the homepage install control compact"
```

---

### Task 3: Verify and Relaunch the Preview

**Files:**

- Verify: `docs/site/index.html`
- Verify: `docs/site/javascripts/install-platform.js`
- Update issue: `6g5x`

**Interfaces:**

- Consumes: the built homepage and browser module from Tasks 1 and 2
- Produces: verified desktop, phone, keyboard, copy, and no-JavaScript behavior plus a user-visible preview

- [ ] **Step 1: Run all relevant repository checks**

Run:

```bash
cd web && bun run test
cd web && bun run check
make docs-check
```

Expected: the full web test suite, static checks, and docs validation pass.

- [ ] **Step 2: Serve the built site on loopback**

Run:

```bash
cd docs/site && python3 -m http.server 8765 --bind 127.0.0.1
```

Expected: `http://127.0.0.1:8765/` returns the built homepage.

- [ ] **Step 3: Inspect desktop and phone behavior in a real browser**

At 1440×900 and 390×844, verify:

- the title, summary, complete rail, and both links are in the desktop top fold;
- all three platform tabs work by pointer and keyboard and update `aria-pressed`;
- only the selected command is visible;
- the copy control copies the bare command without the decorative `$`;
- Quickstart reaches `#quickstart` and all install options reaches its guide;
- macOS alone is visible when JavaScript is disabled;
- no horizontal page overflow or browser console error appears; and
- instant navigation away from and back to `/` reinitializes the rail.

- [ ] **Step 4: Review repository scope and commit history**

Run:

```bash
git status --short
git diff origin/main...HEAD --stat
git log --oneline origin/main..HEAD
```

Expected: no uncommitted changes and only the approved design, plan, platform
behavior, Zensical registration, homepage, and stylesheet differ from main.

- [ ] **Step 5: Close the tracked issue with evidence**

Run with the final implementation commit:

```bash
kata close 6g5x --done \
  --message "Replaced the oversized homepage install card with the compact terminal rail; verified exact command copying, platform selection, responsive layout, and repository checks." \
  --commit "$(git rev-parse HEAD)" \
  --test "cd web && bun run test" \
  --test "cd web && bun run check" \
  --test "make docs-check" \
  --agent
```

- [ ] **Step 6: Relaunch the visual preview**

Open `http://127.0.0.1:8765/` in the visual companion and provide the preview URL
to the user. Keep the server running for inspection.
