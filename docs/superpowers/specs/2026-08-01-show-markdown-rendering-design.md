# `kata show` Markdown Rendering Design

## Problem

Issue descriptions and comments contain Markdown, but human `kata show` output
prints those fields verbatim. Piping the full command through `glow`, `leaf`, or
another Markdown renderer is not equivalent to native support: the renderer
also reinterprets kata's record structure, including the issue header, claims,
labels, links, metadata, and comment separators.

`kata show --render` will render only the fields that contain Markdown. The
surrounding record remains under kata's control, so references and status remain
easy to scan while headings, lists, links, task lists, and code in descriptions
and comments become readable in a terminal.

## Command Contract

The `show` command gains one local boolean flag:

```sh
kata show <issue-ref> --render
```

Without `--render`, human output remains byte-for-byte compatible with the
current format. The flag is valid only for human output. Combining it with
`--json`, `--agent`, or equivalent `--format` values is a usage error, even when
stdout is redirected.

The flag applies only to `kata show`. It does not implicitly change `kata next
--full`, the TUI, list output, API responses, or stored issue content.

## Field-Scoped Rendering

The issue description and every non-empty comment body are separate Markdown
documents. kata renders each document independently, then places the result
back into the existing human record. It never sends the full `kata show` stream
to a Markdown parser.

The following content remains plain kata output:

- issue reference, title, status, author, owner, and priority;
- claim and claim-violation lines;
- section labels and comment identity prefixes;
- labels, relationships, and metadata; and
- all blank-line and section ordering decisions.

The issue description stays in its current position after the header and claim
lines. A rendered comment stays attached to its existing `<uid> <author>:`
prefix. Continuation rows align beneath the body rather than beneath the start
of the prefix. Empty bodies do not invoke a renderer.

User-authored Markdown is sanitized with the existing `textsafe.Block` boundary
before it reaches either renderer. This removes terminal control sequences from
stored content while preserving Markdown syntax. Renderer-produced ANSI is
then treated as styled output, not sanitized user input.

Reinsertion and wrapping must be ANSI-aware. Display width, wrapping, and
continuation indentation use the helpers already used by
`internal/tui/markdown_render.go` from `github.com/charmbracelet/x/ansi`.
Implementation must not slice rendered strings by byte or rune count, trim them
to a raw string length, or split an escape sequence while adding a comment
prefix.

Before ANSI-aware wrapping, kata normalizes renderer line endings: CRLF and
lone CR become LF. It then removes only outer rows whose ANSI-stripped content
is exactly empty, without `TrimSpace`, even when those rows contain ANSI styles
or resets. The removed leading ANSI bytes are prepended to the first visible
row; removed trailing ANSI bytes are appended to the last visible row. That
preserves the renderer's style state while keeping kata's record spacing under
kata's control. A space-only row is visible to this outer-row detection, and
internal blank rows remain. The final visible row is trimmed before trailing
ANSI-only state is appended, so existing per-line trailing ASCII-space trimming
still applies; literal trailing spaces are not a preserved output contract.
Output with or without a final newline reinserts identically.

## Terminal And Color Behavior

Rendering is useful only when the selected output stream is an actual terminal.
`--render` therefore uses the existing terminal check in `cmd/kata/tty.go`; it
must not introduce another `term.IsTerminal` call or an independent TTY policy.
The check applies to `cmd.OutOrStdout()`, not stdin or the process-wide stdout
when Cobra has been given another writer.

The existing renderer in `cmd/kata/render.go` remains the source of the output
color profile. Built-in Markdown styles and final ANSI downsampling use that
profile, including its `NO_COLOR` handling. Tests force a profile through the
existing `newRowRendererFor` seam rather than adding Markdown-specific color
environment variables or a second profile resolver.

When stdout is not a terminal, `--render` deliberately falls back to today's
plain human output. It does not read renderer configuration or start a child
process. This makes redirection safe:

```sh
kata show abc4 --render > issue.txt
```

The same rule means `kata show abc4 --render | less -R` receives plain output.
There is no force-render or force-color CLI path in this first version. This
limitation must appear beside `--render` in command help and the CLI reference;
it must not remain an implicit consequence of implementation details.

## Built-In Renderer

When no override is configured, `--render` uses Glamour in process. Glamour is
already a direct dependency and powers the TUI's Markdown rendering, so a fresh
kata installation works without another executable on every supported
platform.

The CLI and TUI share the Markdown style definition and ANSI-safe wrapping
logic rather than maintaining visually similar copies. The CLI supplies the
terminal width and the profile resolved by `cmd/kata/render.go`. If terminal
width cannot be read, the built-in renderer uses 80 columns. A comment body is
given the width remaining after its visible prefix; other bodies use the full
record width.

If Glamour cannot render a field, kata returns an error before printing any of
the record. It does not emit a mixture of rendered and raw fields.

## External Renderer Override

A client may replace the built-in renderer in its local
`<KATA_HOME>/config.toml`:

```toml
[display]
markdown_renderer = ["leaf"]
```

Arguments that make another renderer behave as a stdin/stdout filter belong in
the same array:

```toml
[display]
markdown_renderer = ["glow", "-", "-s", "dark", "-w", "80"]
```

An absent or empty `markdown_renderer` selects the built-in renderer. A
non-empty array must have a non-empty executable as its first element.

kata passes the configured argv verbatim to `exec.CommandContext`. It does not
invoke a shell, split array elements, expand variables, append stdin markers,
inject a width argument, reorder flags, or special-case known renderers. The
child inherits kata's environment unchanged by leaving `Cmd.Env` unset. Users
may therefore configure renderer-specific variables such as `CLICOLOR_FORCE`
without kata learning renderer-specific policy.

Each non-empty Markdown field starts one renderer invocation. The sanitized
field is the child's complete stdin and is closed at end of input. stdout is
captured as that field's rendered value. stderr is discarded rather than
forwarded or included in errors because an arbitrary renderer may echo its
stdin there. All fields are rendered successfully before kata prints the first
byte, so a later comment failure cannot leave a partial record on stdout.

Each invocation has its own 10-second timeout. On Unix, the renderer starts as
the leader of a new process group. Timeout or command-context cancellation
sends SIGTERM to the whole group, allows a two-second grace period, then sends
SIGKILL to any remaining group members. This reuses or extracts the process
group lifecycle already used by the hook runner instead of creating different
subprocess cleanup rules. On Windows, kata's current process abstraction can
guarantee termination only of the renderer process, not descendants; an
external renderer that starts its own pager may therefore leave that descendant
running. The built-in renderer has no subprocess or timeout.

A missing executable, non-zero exit, timeout, cancellation, or stdout read
failure returns a human CLI error naming the configured executable and the
field kind (`description` or `comment`). The error includes the exit or context
cause but no child stderr or Markdown body. A user who needs renderer-specific
diagnostics can run the configured argv directly.

The override owns renderer-specific color and width behavior. Because kata
captures each child stdout before reinsertion, the child observes a pipe rather
than a TTY. Users who want color or a particular width must include the
renderer-specific flags or inherited environment variables in their own
configuration. kata still applies its resolved output profile when the
captured ANSI is written to the terminal.

## Client-Side Ownership

`[display]` is a client preference. `kata show` reads it from the invoking
client's own `<KATA_HOME>/config.toml` only after confirming that `--render` is
active in human mode on a terminal.

Every shared config reader that otherwise rejects unknown keys—currently
`ReadDaemonConfig` and `ReadAuthConfig`—recognizes the `[display]` subtree only
as an opaque client section. Neither reader decodes or validates
`markdown_renderer`: daemon resolution and auth-only clients must remain able
to start or authenticate even when a client-only renderer preference is
semantically invalid. After the human-output and terminal gates pass, a
separate display reader decodes and validates that subtree. A renderer value
with the wrong TOML type or an unknown display key therefore cannot break
plain, redirected, daemon-serving, or auth-only commands, but does fail an
active render request. Malformed TOML syntax and unknown non-display keys still
fail normal config parsing.

No display setting, renderer argv, rendered text, terminal width, or color
profile is sent in the HTTP request. A remote daemon returns the same raw issue
and comment data as before and cannot select or run the client's renderer. The
daemon process does not consult `[display]` when serving requests.

## Verification

Tests will cover behavior rather than configuration-file text:

- plain `kata show` output remains unchanged;
- built-in rendering changes Markdown fields while leaving record chrome
  literal;
- multiple comments render as independent documents and retain their prefixes;
- ANSI-bearing results wrap and indent without corrupting escape sequences,
  while ANSI-only outer rows fold their state into visible rows;
- `--json` and `--agent` combinations fail as usage errors;
- a non-TTY ignores both built-in rendering and an external override, including
  no child invocation;
- forced output profiles produce deterministic styled and no-color snapshots;
- an external helper receives each field on stdin with argv and environment
  unchanged;
- external output with and without a final newline reinserts identically while
  preserving internal blank lines;
- external failure produces no partial stdout and does not expose child stderr
  or field content; and
- after a helper has started a descendant, timeout and cancellation both
  terminate that descendant with the renderer process group on Unix.

The CLI reference and `show --help` will document the flag, field-scoped
behavior, local configuration, external renderer examples, and non-TTY
fallback.

## Out Of Scope

- Rendering the complete human record as Markdown.
- Rendering titles, labels, links, metadata, or other record chrome.
- Changing JSON, agent, API, or persisted issue representations.
- Adding a renderer choice to the daemon or remote protocol.
- Detecting or configuring known renderer programs automatically.
- Adding `--render` to `next --full` or other commands.
- Adding a force-render flag for pipelines.
- Adding a database migration or persisted schema change.
