# Shell-Baseline Install Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the homepage install prompt and command read as one authentic shell line while keeping the prompt out of copied text.

**Architecture:** Keep the existing Zensical SuperFence markup and copy control. Move the decorative prompt from the outer highlight container to the generated `code` element's inline formatting context. Verify the contract against the built page with a temporary real-browser assertion and visual inspection.

**Tech Stack:** Zensical 0.0.43, CSS, Playwright's installed Chromium, Bun 1.3.14

## Global Constraints

- Render `$` through `code::before`; do not add it to Markdown or DOM text.
- The prompt and command use the same font family, font size, line height, and baseline.
- Keep one normal space between `$` and the command.
- Copying returns the exact executable command without `$` or leading whitespace.
- Keep the copy control at the far right of the command row.
- Preserve OS detection, native platform buttons, hidden panels, responsive containment, instant navigation, and the macOS no-JavaScript fallback.
- Do not add a committed Playwright suite or source-content assertion test.
- Do not change installers, packaging, the application web UI, the daemon, or persisted data.

---

### Task 1: Put the Prompt on the Command Baseline

**Files:**

- Modify: `docs/stylesheets/extra.css`
- Verify: `docs/site/index.html`

**Interfaces:**

- Consumes: Zensical's `.kata-install-command.highlight > pre > code` output and theme copy button
- Produces: an inline, non-selectable `$ ` prefix through `code::before`
- Preserves: copied `code.textContent` as the bare executable command

- [ ] **Step 1: Build the current page**

Run:

```bash
make docs-build
```

Expected: the strict Zensical build succeeds and refreshes `docs/site`.

- [ ] **Step 2: Run a rendered assertion and verify the red state**

Run:

```bash
node --input-type=module -e '
import { chromium } from "./web/node_modules/playwright/index.mjs";
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage();
await page.goto("http://127.0.0.1:8765/", { waitUntil: "networkidle" });
const result = await page.locator("[data-install-platform-content=macos] code").evaluate((code) => {
  const prompt = getComputedStyle(code, "::before");
  const command = getComputedStyle(code);
  return {
    promptContent: prompt.content,
    promptDisplay: prompt.display,
    sameFontFamily: prompt.fontFamily === command.fontFamily,
    sameFontSize: prompt.fontSize === command.fontSize,
    sameLineHeight: prompt.lineHeight === command.lineHeight,
  };
});
await browser.close();
console.log(JSON.stringify(result, null, 2));
if (
  result.promptContent !== "\"$ \"" ||
  result.promptDisplay !== "inline" ||
  !result.sameFontFamily ||
  !result.sameFontSize ||
  !result.sameLineHeight
) process.exit(1);
'
```

Expected: FAIL because the current `$` pseudo-element belongs to the outer
highlight and the `code` element reports no prompt content.

- [ ] **Step 3: Move the prompt into the code line**

Replace the visible command and prompt rules with:

```css
.md-typeset
  .kata-install-command.highlight:not([hidden]):not(
    .kata-install-command--fallback-hidden
  ) {
  min-height: 3.25rem;
  margin: 0;
  border-radius: 0;
  background: var(--md-code-bg-color);
}

.md-typeset .kata-install-command code::before {
  color: var(--md-primary-fg-color);
  content: "$ ";
  display: inline;
  user-select: none;
  vertical-align: baseline;
}

.md-typeset .kata-install-command pre {
  min-height: 3.25rem;
  margin: 0;
  border-radius: 0;
  overflow-x: auto;
}
```

Delete the outer `.kata-install-command.highlight::before` rule and its phone
padding override. Do not add font declarations to the pseudo-element; inheriting
from `code` is what guarantees identical metrics.

- [ ] **Step 4: Rebuild and verify the green rendered state**

Run:

```bash
make docs-build
node --input-type=module -e '
import { chromium } from "./web/node_modules/playwright/index.mjs";
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage();
await page.goto("http://127.0.0.1:8765/", { waitUntil: "networkidle" });
const result = await page.locator("[data-install-platform-content=macos] code").evaluate((code) => {
  const prompt = getComputedStyle(code, "::before");
  const command = getComputedStyle(code);
  return {
    promptContent: prompt.content,
    promptDisplay: prompt.display,
    sameFontFamily: prompt.fontFamily === command.fontFamily,
    sameFontSize: prompt.fontSize === command.fontSize,
    sameLineHeight: prompt.lineHeight === command.lineHeight,
  };
});
await browser.close();
console.log(JSON.stringify(result, null, 2));
if (
  result.promptContent !== "\"$ \"" ||
  result.promptDisplay !== "inline" ||
  !result.sameFontFamily ||
  !result.sameFontSize ||
  !result.sameLineHeight
) process.exit(1);
'
```

Expected: the build succeeds and the rendered assertion passes with `$ ` on the
`code::before` pseudo-element and identical font metrics.

- [ ] **Step 5: Verify exact copying and visual alignment**

At 1440×900 and 390×844:

- capture the homepage top fold with macOS selected;
- confirm `$ brew install kata` reads as one line on one baseline;
- select Windows and confirm the longer command remains contained;
- click the copy control and require clipboard text to equal the selected code
  element's trimmed `textContent` exactly;
- confirm no horizontal page overflow or console errors; and
- confirm JavaScript-disabled rendering shows only the macOS command.

- [ ] **Step 6: Run the full relevant checks**

Run:

```bash
cd web && bun run test
cd web && bun run check
make docs-check
```

Expected: all web tests, static checks, and docs validation pass.

- [ ] **Step 7: Commit the implementation**

Use the mandatory commit skill and stage only the stylesheet:

```bash
git add docs/stylesheets/extra.css
git commit -m "fix: align the homepage shell prompt"
```

- [ ] **Step 8: Close the issue and relaunch the preview**

Close `6g5x` with the implementation commit and the three test commands as typed
evidence. Rebuild and open `http://127.0.0.1:8765/`, keeping the static server
running for user inspection.
