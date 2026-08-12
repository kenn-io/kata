# Homepage Install Hero Design

## Goal

Make installation the primary action in the homepage top fold. A visitor should
see the recommended command for their operating system without scrolling, while
the page still explains what kata is and who it serves.

## Page structure

Keep the existing centered kata title and product promise, but shorten the
introductory copy to one compact description. Place a high-contrast install
panel directly below that description inside the hero.

The install panel contains:

- the heading `Install kata` and the supporting line `One binary. No runtime
  dependencies.`;
- an always-visible macOS, Linux, and Windows selector;
- one visible command block with the documentation theme's copy control;
- a short status line that identifies the selected platform; and
- a link to the complete installation guide.

The commands are:

- macOS: `brew install kata`
- Linux: `curl -fsSL https://katatracker.com/install.sh | bash`
- Windows: `powershell -ExecutionPolicy ByPass -c "irm
  https://katatracker.com/install.ps1 | iex"`

Remove the separate installation section lower on the homepage so the page does
not repeat the same commands. Keep the existing product overview, screenshots,
quickstart, concepts, and next-step content below the hero.

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

A manual selector click immediately replaces the visible command and status
line. Manual selection lasts only for the current rendered page. The site does
not store the selection in local storage, cookies, a query parameter, or any
other persistent state. A refresh or later visit runs detection again.

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

## Responsive layout

On wide screens, the install heading and selector share one row. The command
fills the panel width beneath them. On narrow screens, the heading, selector,
command, and footer stack without horizontal page overflow. The selector uses
the available width, and long commands may scroll inside their code container
rather than forcing the page wider.

The panel should fit in the first desktop viewport with the title and concise
product description. On a phone viewport, the selected command must be visible
without requiring a horizontal page scroll.

## Test strategy

Add behavior tests before the implementation. Cover macOS, Linux, Windows, and
unknown platform detection; manual selection; reset on a fresh initialization;
and accessible selected/hidden state. Exercise the rendered interaction instead
of asserting that source files contain specific strings.

Build the documentation in strict mode after the focused tests pass. Inspect the
built homepage at desktop and phone viewport sizes, and confirm that platform
switching, command copying, the installation-guide link, keyboard focus, and
page width behave as designed.

## Scope

This change affects only the public documentation homepage and its presentation
assets. It does not change installers, release packaging, the application web
UI, the daemon, or persisted data.
