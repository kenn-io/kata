# Homepage Install Hero Design

## Goal

Make installation the primary action in the homepage top fold. A visitor should
see the recommended command for their operating system without scrolling, while
the page still explains what kata is and who it serves.

## Page structure

Keep the existing centered kata title and product promise, but shorten the
introductory copy to one compact description. Place a restrained terminal-style
install rail directly below that description inside the hero.

The install rail contains:

- a thin header row with a small uppercase monospace `INSTALL` label;
- compact macOS, Linux, and Windows tabs aligned to the other end of that row;
- one visible command row with a `$` prompt and the documentation theme's copy
  control; and
- two quiet monospace links beneath the rail: `QUICKSTART →` and
  `ALL INSTALL OPTIONS →`.

The commands are:

- macOS: `brew install kata`
- Linux: `curl -fsSL https://katatracker.com/install.sh | bash`
- Windows: `powershell -ExecutionPolicy ByPass -c "irm
https://katatracker.com/install.ps1 | iex"`

Remove the separate installation section lower on the homepage so the page does
not repeat the same commands. Keep the existing product overview, screenshots,
quickstart, concepts, and next-step content below the hero.

Remove the current `Install` hero button because the rail replaces both its
action and its `#install` target. Replace the rounded `Quickstart` button with
the two text links beneath the rail so installation remains the primary visual
action without adding another large control.

Remove the homepage's stability note, separate `kata version` confirmation, and
checksum explanation with the old install section. Compatibility details do not
belong in this compact acquisition path. The complete installation guide remains
the source for confirmation, checksum verification, package formats, and
alternative installation methods.

## Visual direction

The rail should feel like a compact terminal control, not a standalone product
card. Use a one-pixel border, square or nearly square corners, no shadow, no
filled card background, and tight spacing. The header should be about 40–44
pixels tall and the command body about 48–60 pixels tall.

The platform controls are text tabs rather than a segmented pill. The active tab
uses the existing accent color and a thin underline. Inactive, hover, selected,
and focus states must remain distinct and meet the dark theme's contrast
requirements. Do not use filled cyan buttons or oversized rounded controls.

Do not include the previous `Install kata` title, supporting subtitle,
detected-platform sentence, or stability claim. The command is the focal point;
supporting links remain visually quiet.

Render the `$` prompt outside the copied code text, preferably with a CSS
pseudo-element. The theme's copy control must copy only the executable command.
Keep `QUICKSTART →` pointed at the retained homepage `#quickstart` section.

## Platform selection

The generated HTML makes macOS visible by default. On every homepage load, a
small client-side script selects a platform from browser-provided platform
information:

1. Windows identifiers select Windows.
2. Apple desktop and mobile identifiers select macOS.
3. Linux, X11, and Android identifiers select Linux.
4. Missing or unknown information leaves macOS selected.

The script uses modern user-agent client hints when available and falls back to
the conventional browser platform value. Detection is a presentation hint, not
a compatibility claim.

A manual selector click immediately replaces the visible command. Manual
selection lasts only for the current rendered page. The site does not store the
selection in local storage, cookies, a query parameter, or any other persistent
state. A refresh or later visit runs detection again.

## Interaction and accessibility

Use native buttons for the three-platform selector. The selected button exposes
its state through `aria-pressed`, and each command panel uses the `hidden`
attribute when inactive. Keyboard users can tab to and activate every platform
button. Visible focus, hover, and selected states must remain clear in the
site's dark theme.

The macOS command and the full installation-guide link remain usable if the
detection script does not run. Unknown platform values and script errors fall
back to that static macOS state. The selector script must initialize both on a
normal document load and when Zensical's instant navigation renders the
homepage.

This macOS-only no-script fallback is deliberate. Rendering all three commands
before the script collapses them would cause the top fold to change shape during
startup. The static macOS command keeps first paint stable, while the complete
installation-guide link gives Linux and Windows visitors a script-independent
path to their commands.

## Responsive layout

On wide screens, the `INSTALL` label and selector share one row. The command
fills the rail width beneath them. Keep the same hierarchy on narrow screens;
stack the label and selector only when they cannot fit without crowding. Long
commands may scroll inside their code container rather than forcing the page
wider.

The rail and both supporting links should fit in the first desktop viewport
with the title and concise product description. On a phone viewport, the
selected command must be visible without requiring a horizontal page scroll.

## Test strategy

Reuse the repository's existing Vitest and jsdom harness under `web/` rather
than introduce a second JavaScript package under `docs/`. Add behavior tests
that import the public documentation script and exercise its actual DOM
interaction. Cover macOS, Linux, Windows, and unknown platform detection;
manual selection; reset on a fresh initialization; and accessible
selected/hidden state without requiring a visible status element. Write and run
these tests before the implementation. Do not replace them with source-content
assertions.

Build the documentation in strict mode after the focused tests pass. Inspect the
built homepage at desktop and phone viewport sizes, and confirm that platform
switching, command copying, the installation-guide link, keyboard focus, and
page width behave as designed. A separate committed Playwright suite is out of
scope: the DOM behavior is covered by Vitest/jsdom, and the existing docs build
plus focused browser inspection covers Zensical integration without adding a
second documentation-site server to continuous integration.

## Scope

This change affects only the public documentation homepage and its presentation
assets. It does not change installers, release packaging, the application web
UI, the daemon, or persisted data.
