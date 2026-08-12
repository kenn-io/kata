# Vertically Centered Install Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Vertically center the complete inline shell command within the homepage install row.

**Architecture:** Preserve the current inline `$ command` treatment and Zensical copy control. Make the generated installer `pre` a flex row whose sole in-flow `code` child is centered. Validate the visible command text against the rendered command-body bounds with a real-browser red-green assertion.

**Tech Stack:** Zensical 0.0.43, CSS, Playwright's installed Chromium, Bun 1.3.14

## Global Constraints

- Keep `$ ` and the command in one inline formatting context and on one baseline.
- Vertically center the complete inline shell line with a flex row and `align-items: center`.
- The visible command text's top and bottom whitespace may differ by no more than two pixels.
- Do not simulate centering with fixed line height or asymmetric padding.
- Copying returns the exact executable command without `$` or leading whitespace.
- Preserve the right-aligned copy control, OS selection, hidden panels, responsive containment, instant navigation, and macOS no-JavaScript fallback.
- Do not add a committed Playwright suite or source-content assertion test.
- Do not change installers, packaging, the application web UI, the daemon, or persisted data.

---

### Task 1: Center the Inline Shell Line

**Files:**

- Modify: `docs/stylesheets/extra.css`
- Verify: `docs/site/index.html`

**Interfaces:**

- Consumes: Zensical's `.kata-install-command.highlight > pre > code` output
- Produces: a flex-centered `code` child within the 65px command body
- Preserves: inline prompt baseline and bare-command clipboard text

- [ ] **Step 1: Build the current page**

Run:

```bash
make docs-build
```

Expected: the strict Zensical build succeeds and refreshes `docs/site`.

- [ ] **Step 2: Run the geometry assertion and verify the red state**

Run:

```bash
node --input-type=module -e '
import { chromium } from "./web/node_modules/playwright/index.mjs";
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
await page.goto("http://127.0.0.1:8765/", { waitUntil: "networkidle" });
const result = await page.locator("[data-install-platform-content=macos]").evaluate((highlight) => {
  const pre = highlight.querySelector("pre");
  const text = highlight.querySelector("code span[id^=__span]");
  const preRect = pre.getBoundingClientRect();
  const textRect = text.getBoundingClientRect();
  const top = textRect.top - preRect.top;
  const bottom = preRect.bottom - textRect.bottom;
  return {
    top,
    bottom,
    difference: Math.abs(top - bottom),
    preDisplay: getComputedStyle(pre).display,
    alignItems: getComputedStyle(pre).alignItems,
  };
});
await browser.close();
console.log(JSON.stringify(result, null, 2));
if (result.difference > 2) process.exit(1);
'
```

Expected: FAIL with roughly 3px above, 46px below, and a difference greater
than 40px because normal preformatted flow pins the line near the top.

- [ ] **Step 3: Center the code child**

Add two declarations to the existing installer `pre` rule:

```css
.md-typeset .kata-install-command pre {
  display: flex;
  min-height: 3.25rem;
  align-items: center;
  margin: 0;
  border-radius: 0;
  overflow-x: auto;
}
```

Do not change the `code::before` prompt, command height, padding, or copy-control
markup.

- [ ] **Step 4: Rebuild and verify the green geometry**

Run:

```bash
make docs-build
node --input-type=module -e '
import { chromium } from "./web/node_modules/playwright/index.mjs";
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
await page.goto("http://127.0.0.1:8765/", { waitUntil: "networkidle" });
const result = await page.locator("[data-install-platform-content=macos]").evaluate((highlight) => {
  const pre = highlight.querySelector("pre");
  const text = highlight.querySelector("code span[id^=__span]");
  const preRect = pre.getBoundingClientRect();
  const textRect = text.getBoundingClientRect();
  const top = textRect.top - preRect.top;
  const bottom = preRect.bottom - textRect.bottom;
  return {
    top,
    bottom,
    difference: Math.abs(top - bottom),
    preDisplay: getComputedStyle(pre).display,
    alignItems: getComputedStyle(pre).alignItems,
  };
});
await browser.close();
console.log(JSON.stringify(result, null, 2));
if (result.difference > 2) process.exit(1);
'
```

Expected: PASS with `preDisplay: "flex"`, `alignItems: "center"`, and no more
than a two-pixel top/bottom difference.

- [ ] **Step 5: Verify the rendered interaction**

At 1440×900 and 390×844:

- capture the macOS command row and confirm it is visibly centered;
- select Windows and confirm the long command scrolls inside the row;
- copy macOS and Windows and require clipboard text to equal each code element's
  trimmed `textContent`;
- confirm no horizontal page overflow or console errors; and
- confirm JavaScript-disabled rendering shows only macOS.

- [ ] **Step 6: Run all relevant checks**

Run:

```bash
cd web && bun run test
cd web && bun run check
make docs-check
```

Expected: all web tests, static checks, and docs validation pass.

- [ ] **Step 7: Commit and preview**

Use the mandatory commit skill, stage the stylesheet, and commit:

```bash
git add docs/stylesheets/extra.css
git commit -m "fix: center the homepage install command"
```

Close `6g5x` with the implementation commit and the three check commands as
typed evidence. Rebuild and open `http://127.0.0.1:8765/`, keeping the server
running for user inspection.
