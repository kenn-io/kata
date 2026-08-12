# Homepage Install Hero Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put a platform-aware installation command in the homepage top fold without duplicating the full installation guide.

**Architecture:** Add one small ES module that owns platform detection and the install-panel DOM state. The homepage supplies progressively enhanced semantic HTML, and the existing docs stylesheet supplies responsive presentation. Reuse the web workspace's Vitest/jsdom environment for behavior coverage, then validate the integration through Zensical's strict build and a real browser.

**Tech Stack:** Zensical 0.0.43, Markdown with `md_in_html`, browser ES modules, CSS, Vitest 4.1.10, jsdom 29.1.1, Bun 1.3.14

## Global Constraints

- macOS uses `brew install kata`.
- Linux uses `curl -fsSL https://katatracker.com/install.sh | bash`.
- Windows uses `powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"`.
- Detect the browser platform on every homepage render; fall back to macOS and persist nothing.
- Keep macOS as the deliberate no-JavaScript fallback and keep the complete installation-guide link available.
- Keep `Quickstart` as the secondary hero action after the panel; remove the obsolete `Install` hero action and `#install` section.
- Keep the stability trust signal in the panel; leave version confirmation, checksums, package formats, and alternatives to the complete guide.
- Do not add source-content assertion tests.
- Do not change installers, packaging, the application web UI, the daemon, or persisted data.

---

### Task 1: Platform Detection and Install-Panel Behavior

**Files:**
- Create: `docs/javascripts/install-platform.js`
- Create: `web/src/lib/docs/install-platform.test.js`
- Modify: `web/vitest.config.ts:15`

**Interfaces:**
- Produces: `detectInstallPlatform(platform: unknown): 'macos' | 'linux' | 'windows'`
- Produces: `initializeInstallPanel(root: Document | Element, browserPlatform?: unknown): void`
- Consumes from the homepage: `[data-install-panel]`, `[data-install-platform-button]`, `[data-install-platform-content]`, and `[data-install-platform-status]`

- [ ] **Step 1: Extend Vitest discovery and write the failing detection test**

Change the Vitest include pattern and create the test with only the detection cases:

```ts
// web/vitest.config.ts
include: ['src/**/*.test.{js,ts}'],
```

```js
// web/src/lib/docs/install-platform.test.js
import { describe, expect, test } from 'vitest'

import { detectInstallPlatform } from '../../../../docs/javascripts/install-platform.js'

describe('detectInstallPlatform', () => {
  test.each([
    ['macOS', 'macos'],
    ['MacIntel', 'macos'],
    ['iPhone', 'macos'],
    ['iOS', 'macos'],
    ['Win32', 'windows'],
    ['Windows', 'windows'],
    ['Linux x86_64', 'linux'],
    ['X11', 'linux'],
    ['Android', 'linux'],
    ['', 'macos'],
    [undefined, 'macos'],
    ['Plan 9', 'macos'],
  ])('maps %s to %s', (platform, expected) => {
    expect(detectInstallPlatform(platform)).toBe(expected)
  })
})
```

- [ ] **Step 2: Run the focused test and verify the red state**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
```

Expected: FAIL because `docs/javascripts/install-platform.js` does not exist.

- [ ] **Step 3: Implement only platform detection**

Create the module with the detection export:

```js
const DEFAULT_PLATFORM = 'macos'

export function detectInstallPlatform(platform) {
  const value = typeof platform === 'string' ? platform.toLowerCase() : ''

  if (value.includes('win')) return 'windows'
  if (value.includes('mac') || value.includes('iphone') || value.includes('ipad')) {
    return 'macos'
  }
  if (value.includes('linux') || value.includes('x11') || value.includes('android')) {
    return 'linux'
  }
  return DEFAULT_PLATFORM
}
```

- [ ] **Step 4: Run the focused test and verify the green state**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
```

Expected: PASS for all detection cases.

- [ ] **Step 5: Add failing DOM behavior tests**

Append the fixture and DOM tests:

```js
import {
  detectInstallPlatform,
  initializeInstallPanel,
} from '../../../../docs/javascripts/install-platform.js'

function renderPanel() {
  document.body.innerHTML = `
    <section data-install-panel>
      <button data-install-platform-button="macos" aria-pressed="true">macOS</button>
      <button data-install-platform-button="linux" aria-pressed="false">Linux</button>
      <button data-install-platform-button="windows" aria-pressed="false">Windows</button>
      <div data-install-platform-content="macos">brew</div>
      <div class="kata-install-command--fallback-hidden" data-install-platform-content="linux">curl</div>
      <div class="kata-install-command--fallback-hidden" data-install-platform-content="windows">irm</div>
      <p data-install-platform-status>macOS selected · choose another platform anytime.</p>
    </section>`
}

function button(platform) {
  return document.querySelector(`[data-install-platform-button="${platform}"]`)
}

function content(platform) {
  return document.querySelector(`[data-install-platform-content="${platform}"]`)
}

describe('initializeInstallPanel', () => {
  test('applies the detected platform to accessible DOM state', () => {
    renderPanel()
    initializeInstallPanel(document, 'Win32')

    expect(button('windows')?.getAttribute('aria-pressed')).toBe('true')
    expect(button('macos')?.getAttribute('aria-pressed')).toBe('false')
    expect(content('windows')?.hidden).toBe(false)
    expect(content('macos')?.hidden).toBe(true)
    expect(content('windows')?.classList).not.toContain(
      'kata-install-command--fallback-hidden',
    )
    expect(document.querySelector('[data-install-platform-status]')?.textContent).toBe(
      'Windows selected · choose another platform anytime.',
    )
  })

  test('manual choice lasts until a fresh initialization', () => {
    renderPanel()
    initializeInstallPanel(document, 'MacIntel')

    button('linux')?.click()
    expect(button('linux')?.getAttribute('aria-pressed')).toBe('true')
    expect(content('linux')?.hidden).toBe(false)

    initializeInstallPanel(document, 'MacIntel')
    expect(button('macos')?.getAttribute('aria-pressed')).toBe('true')
    expect(content('macos')?.hidden).toBe(false)
    expect(content('linux')?.hidden).toBe(true)
  })

  test('does nothing when the homepage panel is absent', () => {
    document.body.replaceChildren()
    expect(() => initializeInstallPanel(document, 'Linux')).not.toThrow()
  })
})
```

Replace the original one-name import with the combined import shown above.

- [ ] **Step 6: Run the focused test and verify the second red state**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
```

Expected: FAIL because `initializeInstallPanel` is not exported.

- [ ] **Step 7: Implement DOM selection, browser lookup, and page initialization**

Append the implementation below `detectInstallPlatform`:

```js
const PLATFORM_LABELS = {
  macos: 'macOS',
  linux: 'Linux',
  windows: 'Windows',
}

const initializedPanels = new WeakSet()

function selectInstallPlatform(panel, platform) {
  for (const button of panel.querySelectorAll('[data-install-platform-button]')) {
    button.setAttribute(
      'aria-pressed',
      String(button.dataset.installPlatformButton === platform),
    )
  }

  for (const content of panel.querySelectorAll('[data-install-platform-content]')) {
    content.classList.remove('kata-install-command--fallback-hidden')
    content.hidden = content.dataset.installPlatformContent !== platform
  }

  const status = panel.querySelector('[data-install-platform-status]')
  if (status) {
    status.textContent = `${PLATFORM_LABELS[platform]} selected · choose another platform anytime.`
  }
}

function readBrowserPlatform() {
  return navigator.userAgentData?.platform || navigator.platform
}

export function initializeInstallPanel(root, browserPlatform = readBrowserPlatform()) {
  const panel = root.querySelector('[data-install-panel]')
  if (!panel) return

  if (!initializedPanels.has(panel)) {
    panel.addEventListener('click', (event) => {
      if (!(event.target instanceof Element)) return
      const button = event.target.closest('[data-install-platform-button]')
      if (button && panel.contains(button)) {
        selectInstallPlatform(panel, button.dataset.installPlatformButton)
      }
    })
    initializedPanels.add(panel)
  }

  selectInstallPlatform(panel, detectInstallPlatform(browserPlatform))
}

function initializeCurrentPage() {
  initializeInstallPanel(document)
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initializeCurrentPage, { once: true })
} else {
  initializeCurrentPage()
}

globalThis.document$?.subscribe(initializeCurrentPage)
```

- [ ] **Step 8: Run the focused test, web lint, and format checks**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
./web/node_modules/.bin/eslint --config web/eslint.config.js docs/javascripts/install-platform.js web/src/lib/docs/install-platform.test.js
cd web && bun run format:check
```

Expected: all commands PASS with no warnings. If Prettier reports only the new files, run `cd web && bun x prettier --write src/lib/docs/install-platform.test.js ../docs/javascripts/install-platform.js ../docs/superpowers/plans/2026-08-11-homepage-install-hero.md`, then rerun the three checks.

- [ ] **Step 9: Commit the behavior module and tests**

Use the mandatory commit skill, then commit only these files:

```bash
git add docs/javascripts/install-platform.js web/src/lib/docs/install-platform.test.js web/vitest.config.ts
git commit -m "feat: select the homepage install platform"
```

---

### Task 2: Install-First Homepage Hero

**Files:**
- Modify: `docs/index.md:6-82`
- Modify: `docs/stylesheets/extra.css:52-72`
- Modify: `docs/zensical.toml:12`
- Test: `web/src/lib/docs/install-platform.test.js`

**Interfaces:**
- Consumes: `initializeInstallPanel` through the `data-install-*` HTML contract from Task 1
- Produces: responsive install panel markup with macOS as its static state
- Produces: module registration through `project.extra_javascript`

- [ ] **Step 1: Add the install panel markup and remove the duplicated section**

Replace the current hero body with the following content, then remove the full
`## Install` section through the `Stable` admonition. Put the command contract
directly on each SuperFence; nested `markdown` wrappers leave XML elements in
Zensical's raw HTML stash while its table-of-contents processor expects strings:

````markdown
<div class="kata-hero" markdown>

# kata カタ

<p class="kata-hero-tagline">The issue tracker built for coding agents and the humans steering them.</p>

<p class="kata-hero-summary">A local-first task ledger agents drive from the CLI and humans supervise in the terminal or browser.</p>

<section class="kata-install" data-install-panel markdown>
<div class="kata-install-header">
<div>
<h2>Install kata</h2>
<p>One binary. No runtime dependencies.</p>
</div>
<div class="kata-install-platforms" role="group" aria-label="Choose your operating system">
<button type="button" data-install-platform-button="macos" aria-pressed="true">macOS</button>
<button type="button" data-install-platform-button="linux" aria-pressed="false">Linux</button>
<button type="button" data-install-platform-button="windows" aria-pressed="false">Windows</button>
</div>
</div>
```sh { .kata-install-command data-install-platform-content="macos" }
brew install kata
```

```sh { .kata-install-command .kata-install-command--fallback-hidden data-install-platform-content="linux" }
curl -fsSL https://katatracker.com/install.sh | bash
```

```powershell { .kata-install-command .kata-install-command--fallback-hidden data-install-platform-content="windows" }
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

<div class="kata-install-meta">
<p data-install-platform-status>macOS selected · choose another platform anytime.</p>
<a href="get-started/install.md">All install options →</a>
</div>
<p class="kata-install-stability"><strong>Stable since v0.14.0:</strong> releases preserve backward compatibility across upgrades.</p>
</section>

[Quickstart](#quickstart){ .md-button }

</div>
````

- [ ] **Step 2: Register the module script**

Add this directly after `extra_css` in `docs/zensical.toml`:

```toml
extra_javascript = [
  { path = "javascripts/install-platform.js", type = "module" },
]
```

- [ ] **Step 3: Add responsive install-panel styles**

Replace the old `.kata-hero > p` rule with `.kata-hero-summary`, retain the
tagline and button rules, and append these styles:

```css
.md-typeset .kata-hero p.kata-hero-summary {
  max-width: 38rem;
  margin-inline: auto;
  text-align: center;
}

.md-typeset .kata-install {
  max-width: 52rem;
  margin: 1.5rem auto 0.5rem;
  padding: 1rem;
  border: 1px solid color-mix(in srgb, var(--md-primary-fg-color) 38%, transparent);
  border-radius: 12px;
  background: color-mix(in srgb, var(--md-default-bg-color) 82%, #102635);
  box-shadow: 0 16px 42px rgb(0 0 0 / 22%);
  text-align: left;
}

.md-typeset .kata-install-header,
.md-typeset .kata-install-meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
}

.md-typeset .kata-install-header h2 {
  margin: 0;
  padding: 0;
  border: 0;
  font-size: 1rem;
}

.md-typeset .kata-install-header p,
.md-typeset .kata-install-meta p,
.md-typeset .kata-install-stability {
  margin: 0;
}

.md-typeset .kata-install-header p,
.md-typeset .kata-install-meta,
.md-typeset .kata-install-stability {
  color: var(--md-default-fg-color--light);
  font-size: 0.68rem;
}

.md-typeset .kata-install-platforms {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  padding: 0.15rem;
  border: 1px solid color-mix(in srgb, currentColor 16%, transparent);
  border-radius: 8px;
  background: color-mix(in srgb, var(--md-default-bg-color) 88%, black);
}

.md-typeset .kata-install-platforms button {
  padding: 0.4rem 0.65rem;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--md-default-fg-color--light);
  cursor: pointer;
  font: inherit;
  font-weight: 650;
}

.md-typeset .kata-install-platforms button[aria-pressed="true"] {
  background: var(--md-primary-fg-color);
  color: var(--md-primary-bg-color);
}

.md-typeset .kata-install-platforms button:focus-visible {
  outline: 2px solid var(--md-accent-fg-color);
  outline-offset: 2px;
}

.md-typeset .kata-install-command[hidden],
.md-typeset .kata-install-command--fallback-hidden {
  display: none;
}

.md-typeset .kata-install-command.highlight {
  margin: 0.8rem 0 0.6rem;
}

.md-typeset .kata-install-command pre {
  overflow-x: auto;
}

.md-typeset .kata-install-meta a {
  flex: none;
}

.md-typeset .kata-install-stability {
  margin-top: 0.6rem;
  padding-top: 0.6rem;
  border-top: 1px solid color-mix(in srgb, currentColor 12%, transparent);
}

@media screen and (max-width: 44.984375em) {
  .md-typeset .kata-install-header,
  .md-typeset .kata-install-meta {
    align-items: stretch;
    flex-direction: column;
    gap: 0.6rem;
  }

  .md-typeset .kata-install-platforms {
    width: 100%;
  }

  .md-typeset .kata-install-meta a {
    align-self: flex-start;
  }
}
```

- [ ] **Step 4: Run focused behavior and strict documentation builds**

Run:

```bash
cd web && bun x vitest run src/lib/docs/install-platform.test.js
make docs-build
```

Expected: the focused Vitest file passes and Zensical completes a strict build without warnings or invalid links.

- [ ] **Step 5: Inspect the generated integration before committing**

Run:

```bash
rg -n "install-platform.js|data-install-panel|All install options|Quickstart" docs/site/index.html
```

Expected: the built homepage loads `install-platform.js` as a module, includes the install panel and complete-guide link, and retains Quickstart. This is a diagnostic inspection of generated integration, not an automated source-content test.

- [ ] **Step 6: Commit the homepage and presentation changes**

Use the mandatory commit skill, then commit only these files:

```bash
git add docs/index.md docs/stylesheets/extra.css docs/zensical.toml
git commit -m "docs: put installation in the homepage hero"
```

---

### Task 3: Full Verification and Preview

**Files:**
- Verify: `docs/site/index.html`
- Verify: `docs/site/javascripts/install-platform.js`
- Update issue: `6g5x`

**Interfaces:**
- Consumes: the built homepage and browser module from Tasks 1 and 2
- Produces: verified desktop and mobile behavior plus a user-visible preview URL

- [ ] **Step 1: Run repository checks relevant to every changed file**

Run:

```bash
cd web && bun run test
cd web && bun run check
make docs-check
```

Expected: all web unit tests, web static checks, and the full docs validation pass.

- [ ] **Step 2: Review the final diff and commits**

Run:

```bash
git status --short
git diff origin/main...HEAD --stat
git diff origin/main...HEAD
git log --oneline origin/main..HEAD
```

Expected: no uncommitted repository changes; only the approved spec, plan, behavior module/test, Zensical config, homepage, and stylesheet differ from `origin/main`.

- [ ] **Step 3: Serve the built site without rebuilding it**

Run a local static server from `docs/site` on an available loopback port:

```bash
cd docs/site && python3 -m http.server 8765 --bind 127.0.0.1
```

Expected: the process stays running and `http://127.0.0.1:8765/` returns the built homepage.

- [ ] **Step 4: Inspect desktop and mobile behavior in a real browser**

At 1440×900 and 390×844 viewports, verify:

- title, concise summary, and selected install command are in the top fold;
- browser detection selects the expected platform and refresh resets selection;
- all three buttons work by pointer and keyboard and update `aria-pressed`;
- only the selected command is visible and its copy control copies that command;
- the full-guide and Quickstart links reach their destinations;
- the stability note remains visible;
- no horizontal page overflow appears at the phone width; and
- instant navigation away from and back to the homepage reinitializes the panel.

Expected: every item passes with no browser console errors.

- [ ] **Step 5: Close the tracked issue with evidence**

After the final implementation commit is known, run:

```bash
kata close 6g5x --done --message "Moved platform-aware installation guidance into the homepage top fold; verified Vitest, web checks, strict docs validation, and desktop/mobile browser behavior." --commit "$(git rev-parse HEAD)" --test "cd web && bun run test" --test "cd web && bun run check" --test "make docs-check" --agent
```

Expected: issue `6g5x` closes as done with the final commit and typed evidence.

- [ ] **Step 6: Open the preview for the user**

Open `http://127.0.0.1:8765/` in the in-app browser and provide the same URL as a fallback. Keep the server running so the user can inspect the completed homepage.
