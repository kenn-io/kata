# `kata show` Markdown Rendering Implementation Plan

<!-- markdownlint-disable MD013 -->

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `kata show --render`, which renders only issue and comment
Markdown while preserving kata's human record structure, with built-in Glamour
rendering and an optional client-local external renderer.

**Architecture:** Extract the TUI's Markdown engine into a small shared package,
then let `show` pre-render its Markdown fields before writing any output. Human
record formatting stays in `show.go`; renderer selection, external process
lifecycle, client configuration, terminal detection, and ANSI-safe field layout
remain separate units with narrow interfaces.

**Tech Stack:** Go, Cobra, BurntSushi TOML, Glamour,
`github.com/charmbracelet/colorprofile`, `github.com/charmbracelet/x/ansi`, and
`golang.org/x/term`.

## Global Constraints

- Follow red-green-refactor for every behavior change. Observe each focused
  test fail for the intended reason before writing production code.
- Invoke the repository's test-scope-discipline before adding tests. Test kata's
  boundaries, not Glamour, TOML, `os/exec`, or terminal-library behavior.
- Invoke the isolation skill before the first build or test command. Tests and
  manual checks must use temporary `KATA_HOME` and workspace paths and must not
  contact an installed daemon or live database.
- `--render` is valid only for human `kata show` output. `--json`, `--agent`,
  and equivalent non-human `--format` values must return a usage error.
- Render only the issue description and each non-empty comment body. Never pass
  the complete human record, title, header, claims, labels, links, metadata, or
  separators to a Markdown renderer.
- Without `--render`, preserve current human output byte for byte.
- A non-TTY output stream silently uses current plain output, does not read
  `[display]`, and does not start a renderer. Document that this includes
  `kata show <ref> --render | less -R`; do not add a force path.
- Resolve TTY state through `cmd/kata/tty.go`'s existing `isTTY` seam and color
  capability through `cmd/kata/render.go`'s `newRowRenderer` /
  `newRowRendererFor` path. Do not add another `term.IsTerminal` call or color
  resolver.
- Built-in rendering uses Glamour and the TUI's shared style definition. The
  terminal-width fallback is exactly 80 columns.
- `[display].markdown_renderer` is a client-local argv array. Pass every element
  verbatim, invoke no shell, append no flags, and leave `exec.Cmd.Env` nil so the
  child inherits the client's environment unchanged.
- Each external field invocation has a 10-second timeout and two-second
  termination grace period. Discard child stderr so a renderer cannot expose a
  field by echoing stdin in its diagnostics. Never include field content in an
  error.
- On Unix, timeout and cancellation terminate the full renderer process group;
  on Windows, the current process abstraction guarantees only termination of
  the renderer process.
- Renderer output is ANSI-bearing trusted presentation. Use
  `github.com/charmbracelet/x/ansi` for display width and hard wrapping; never
  byte-slice, rune-slice, or raw-length-trim it.
- Normalize CRLF and lone CR to LF, then remove only outer rows for which
  `ansi.Strip(row) == ""`, without `TrimSpace`. Fold their ANSI state into the
  first or last visible row; a space-only row remains visible to that detection,
  and internal blank rows remain. Keep the inherited per-line
  `strings.TrimRight(line, " ")` behavior, so literal trailing ASCII spaces are
  not preserved. Output with and without a final newline reinserts identically.
- `[display]` is read from the invoking client's `<KATA_HOME>/config.toml` and
  never crosses the API boundary. No daemon, HTTP, generated-client, database,
  migration, or persisted-schema change is allowed.
- Use neutral examples and fixtures such as `example-workspace`, `user-a`, and
  `abc4`. Run the privacy scrub before every public commit.
- Before every commit, invoke the mandatory commit skill. Do not amend, squash,
  or otherwise rewrite the existing design commit.

---

## File Map

### New files

- `internal/markdownrender/render.go` — shared Glamour style, sanitization, and
  ANSI-safe line wrapping used by the TUI and CLI.
- `internal/markdownrender/render_test.go` — tests kata's shared sanitization and
  ANSI layout boundary.
- `internal/processtree/tree.go` — platform-neutral graceful process-tree stop.
- `internal/processtree/tree_unix.go` — Unix process-group configuration,
  signaling, and liveness.
- `internal/processtree/tree_windows.go` — Windows leader-process fallback.
- `internal/processtree/tree_test.go` — platform-neutral no-process behavior.
- `cmd/kata/show_markdown.go` — built-in and external field renderer backends,
  timeout handling, stderr discard, and backend selection.
- `cmd/kata/show_markdown_test.go` — renderer-backend contract tests and shared
  helper-process modes.
- `cmd/kata/show_markdown_unix_test.go` — Unix-only external-renderer
  process-group cleanup tests.

### Modified files

- `internal/tui/markdown_render.go` — delegate Glamour rendering and ANSI
  wrapping to `internal/markdownrender` while preserving TUI fallback behavior.
- `internal/tui/detail_document_test.go` and existing TUI snapshots — verify the
  extraction preserves the current visual contract; update no golden unless a
  test proves an intended difference.
- `internal/config/daemon_config.go` — add the client-side `DisplayConfig` shape
  and focused `ReadDisplayConfig` validation.
- `internal/config/daemon_config_test.go` — cover exact argv preservation and
  empty-executable validation.
- `internal/hooks/runner.go` — use the shared process-tree lifecycle without
  changing hook outcomes.
- `internal/hooks/proc_unix.go` and `internal/hooks/proc_windows.go` — remove
  after their logic moves to `internal/processtree`.
- `internal/hooks/runner_unix_test.go` — keep the existing orphan cleanup
  regression as the extraction's end-to-end witness.
- `cmd/kata/tty.go` — identify whether the selected Cobra output writer is an
  actual terminal and obtain its width through the existing TTY seam.
- `cmd/kata/render.go` — expose Markdown style/profile inputs on `rowRenderer`
  and use its existing profile-aware writer for final output.
- `cmd/kata/show.go` — bind `--render`, reject structured modes, pre-render
  fields, and print the existing record around rendered values.
- `cmd/kata/show_test.go` — cover field scoping, mode rejection, non-TTY
  fallback, no partial output, and ANSI-safe comment reinsertion.
- `cmd/kata/next.go` — pass zero-value show options so `next --full` stays
  unchanged.
- `docs/reference/cli.md` — document `--render`, field scope, and pipeline
  fallback.
- `docs/reference/configuration.md` — document client-side `[display]` and
  verbatim external-renderer ownership.

---

### Task 1: Extract Shared Markdown Rendering

**Files:**

- Create: `internal/markdownrender/render.go`
- Create: `internal/markdownrender/render_test.go`
- Modify: `internal/tui/markdown_render.go`
- Verify: `internal/tui/detail_document_test.go`
- Verify: `internal/tui/testdata/golden/detail-document-markdown.txt`

**Interfaces:**

- Produces:
  `markdownrender.Options{Width int, CodeBlockBackground *string}`.
- Produces:
  `markdownrender.Render(markdown string, opts Options) (string, error)`.
- Produces:
  `markdownrender.RenderLines(markdown string, opts Options) ([]string, error)`.
- Produces:
  `markdownrender.ANSIWrappedLines(rendered string, width int) []string`.
- Preserves the TUI-local
  `renderMarkdownLines(markdown string, width int) []string` interface.

- [ ] **Step 1: Write the failing shared-package tests**

Create `internal/markdownrender/render_test.go` with focused tests for kata's
sanitization and wrapping boundaries:

```go
package markdownrender

import (
    "strings"
    "testing"

    "github.com/charmbracelet/x/ansi"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "go.kenn.io/kata/internal/textsafe"
)

func TestRenderLinesSanitizesStoredControlSequences(t *testing.T) {
    lines, err := RenderLines(
        "## Steps\n\n- run `kata show`\x1b]2;unsafe\x07",
        Options{Width: 40},
    )
    require.NoError(t, err)
    got := strings.Join(lines, "\n")
    assert.Contains(t, textsafe.StripANSI(got), "Steps")
    assert.Contains(t, textsafe.StripANSI(got), "kata show")
    assert.NotContains(t, got, "unsafe")
    assert.NotContains(t, got, "## Steps")
}

func TestANSIWrappedLinesPreservesVisibleContent(t *testing.T) {
    rendered := "\x1b[31mカタabcdef\x1b[0m"
    lines := ANSIWrappedLines(rendered, 4)
    require.NotEmpty(t, lines)
    for _, line := range lines {
        assert.LessOrEqual(t, ansi.StringWidth(line), 4)
    }
    assert.Equal(t, "カタabcdef", textsafe.StripANSI(strings.Join(lines, "")))
}

func TestANSIWrappedLinesNormalizesOnlyOuterLineEndings(t *testing.T) {
    want := []string{"first", "", "second"}
    assert.Equal(t, want, ANSIWrappedLines("\nfirst\r\n\r\nsecond\r\n", 80))
    assert.Equal(t, want, ANSIWrappedLines("first\n\nsecond", 80))
}

func TestANSIWrappedLinesFoldsANSIOnlyOuterRowsIntoVisibleRows(t *testing.T) {
    got := ANSIWrappedLines("\x1b[31m\nfirst\r\n\r\nsecond\n\x1b[0m\n", 80)

    assert.Equal(t, []string{"\x1b[31mfirst", "", "second\x1b[0m"}, got)
}

func TestANSIWrappedLinesTrimsLastVisibleRowBeforeTrailingANSIState(t *testing.T) {
    assert.Equal(t, []string{"text\x1b[0m"}, ANSIWrappedLines("text   \n\x1b[0m", 80))
}
```

- [ ] **Step 2: Run the tests and confirm the missing package/API failure**

Run:

```sh
go test ./internal/markdownrender
```

Expected: FAIL because `Options`, `RenderLines`, and `ANSIWrappedLines` do not
exist.

- [ ] **Step 3: Move the shared renderer into its focused package**

Create `internal/markdownrender/render.go` with this public shape:

```go
package markdownrender

import (
    "fmt"
    "strings"

    "charm.land/glamour/v2"
    glamansi "charm.land/glamour/v2/ansi"
    "github.com/charmbracelet/x/ansi"
    "go.kenn.io/kata/internal/textsafe"
)

type Options struct {
    Width               int
    CodeBlockBackground *string
}

func Render(markdown string, opts Options) (out string, err error) {
    defer func() {
        if recovered := recover(); recovered != nil {
            out = ""
            err = fmt.Errorf("render markdown: %v", recovered)
        }
    }()
    width := max(1, opts.Width)
    renderer, err := glamour.NewTermRenderer(
        glamour.WithStyles(styleConfig(opts.CodeBlockBackground)),
        glamour.WithPreservedNewLines(),
        glamour.WithWordWrap(width),
        glamour.WithTableWrap(true),
    )
    if err != nil {
        return "", err
    }
    return renderer.Render(textsafe.Block(markdown))
}

func RenderLines(markdown string, opts Options) ([]string, error) {
    if strings.TrimSpace(markdown) == "" {
        return nil, nil
    }
    rendered, err := Render(markdown, opts)
    if err != nil {
        return nil, err
    }
    return ANSIWrappedLines(rendered, opts.Width), nil
}

func ANSIWrappedLines(rendered string, width int) []string {
    rendered = strings.ReplaceAll(rendered, "\r\n", "\n")
    rendered = strings.ReplaceAll(rendered, "\r", "\n")
    raw := strings.Split(rendered, "\n")
    first := 0
    for first < len(raw) && ansi.Strip(raw[first]) == "" {
        first++
    }
    if first == len(raw) {
        return nil
    }
    last := len(raw) - 1
    for last > first && ansi.Strip(raw[last]) == "" {
        last--
    }
    raw[first] = strings.Join(raw[:first+1], "")
    raw[last] = strings.TrimRight(raw[last], " ") + strings.Join(raw[last+1:], "")
    raw = raw[first : last+1]

    width = max(1, width)
    lines := make([]string, 0, len(raw))
    for _, line := range raw {
        line = strings.TrimRight(line, " ")
        // Glamour word-wraps prose to width but leaves preformatted content
        // (code blocks, long URLs, table cells, and stack traces) at its natural
        // width. Hardwrap preserves all content across display rows.
        lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
    }
    return lines
}
```

The outer-row scan intentionally uses `ansi.Strip(row) == ""` rather than
`TrimSpace`: a row containing a literal space is visible for normalization.
Only outer ANSI-only rows fold state into visible content; internal blank rows
remain. Trim the final visible row before appending trailing ANSI-only state so
the inherited `strings.TrimRight(line, " ")` policy still removes its trailing
ASCII spaces. Tests must not promise literal trailing ASCII spaces survive.

Move the current `markdownStyleConfig` body into an unexported
`styleConfig(codeBackground *string) glamansi.StyleConfig`. Keep every current
style value, margin, task marker, list indent, link treatment, and image label
unchanged.

- [ ] **Step 4: Run the shared tests and verify green**

Run:

```sh
go test ./internal/markdownrender
```

Expected: PASS.

- [ ] **Step 5: Refactor the TUI wrapper onto the shared package**

Replace the Glamour setup and hard-wrap loop in
`internal/tui/markdown_render.go` with:

```go
func renderMarkdownLines(markdown string, width int) []string {
    lines, err := markdownrender.RenderLines(markdown, markdownrender.Options{
        Width:               width,
        CodeBlockBackground: markdownCodeBlockBackground(),
    })
    if err != nil {
        return wrapBody(sanitizeForDisplay(markdown), width)
    }
    if activeColorMode == colorNone {
        for i := range lines {
            lines[i] = stripANSI(lines[i])
        }
    }
    return lines
}
```

Remove the TUI-local `renderMarkdown` and `markdownStyleConfig` functions. Keep
`markdownCodeBlockBackground` because it is the TUI's adaptive style input.

- [ ] **Step 6: Prove the TUI behavior did not change**

Run:

```sh
go test ./internal/tui -run 'TestDetailDocument_MarkdownRenderingDropsSourceFences|TestSnapshot_Detail_DocumentMarkdown|TestMarkdownCodeBlockBackground_RespectsAutoDetectedBackground' -count=1
```

Expected: PASS with no golden-file diff.

- [ ] **Step 7: Commit the shared renderer extraction**

Invoke the commit and privacy-scrub skills, then run:

```sh
git add internal/markdownrender internal/tui/markdown_render.go
git commit -m "Share Markdown rendering between CLI and TUI"
```

The commit body should explain that field-scoped CLI rendering needs the TUI's
existing style and ANSI wrapping without maintaining a second implementation.

---

### Task 2: Add Client Display Configuration

**Files:**

- Modify: `internal/config/daemon_config.go`
- Modify: `internal/config/daemon_config_test.go`

**Interfaces:**

- Produces:
  `config.DisplayConfig{MarkdownRenderer []string}`.
- Produces:
  `config.ReadDisplayConfig() (DisplayConfig, error)`.
- Both strict shared config readers, `ReadDaemonConfig` and `ReadAuthConfig`,
  recognize `[display]` as an opaque client subtree but do not decode or
  validate its values.
- `ReadDisplayConfig` separately decodes and validates only `[display]` after
  the command's human-output and terminal gates pass.

- [ ] **Step 1: Write failing display-config tests**

Add these cases to `internal/config/daemon_config_test.go`:

```go
func TestReadDisplayConfigPreservesRendererArgv(t *testing.T) {
    home := t.TempDir()
    t.Setenv("KATA_HOME", home)
    require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[display]
markdown_renderer = ["renderer", "--style", "dark theme", "-"]
`), 0o600))

    got, err := config.ReadDisplayConfig()
    require.NoError(t, err)
    assert.Equal(t, []string{"renderer", "--style", "dark theme", "-"}, got.MarkdownRenderer)
}

func TestReadDisplayConfigRejectsEmptyExecutable(t *testing.T) {
    home := t.TempDir()
    t.Setenv("KATA_HOME", home)
    require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[display]
markdown_renderer = [""]
`), 0o600))

    _, err := config.ReadDisplayConfig()
    require.Error(t, err)
    assert.Contains(t, err.Error(), "display.markdown_renderer executable must not be empty")
}

func TestReadDisplayConfigRejectsUnknownDisplayKey(t *testing.T) {
    home := t.TempDir()
    t.Setenv("KATA_HOME", home)
    require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[display]
markdown_renderer = ["renderer"]
future_option = true
`), 0o600))

    _, err := config.ReadDisplayConfig()
    require.Error(t, err)
    assert.Contains(t, err.Error(), "display.future_option")
}

func TestReadDaemonConfigTreatsDisplayAsOpaque(t *testing.T) {
    home := t.TempDir()
    t.Setenv("KATA_HOME", home)
    require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[display]
markdown_renderer = "not-an-array"
future_option = true
`), 0o600))

    _, err := config.ReadDaemonConfig()
    require.NoError(t, err)
}

func TestReadAuthConfigTreatsDisplayAsOpaque(t *testing.T) {
    tests := []struct {
        name    string
        display string
    }{
        {
            name: "valid renderer argv",
            display: `
[display]
markdown_renderer = ["renderer", "--style", "dark theme", "-"]
`,
        },
        {
            name: "malformed renderer and unknown display key",
            display: `
[display]
markdown_renderer = "not-an-array"
future_option = true
`,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            home := t.TempDir()
            t.Setenv("KATA_HOME", home)
            require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[auth]
token = "client-token"
trust_private_network = true
`+tt.display), 0o600))

            got, err := config.ReadAuthConfig()
            require.NoError(t, err)
            assert.Equal(t, "client-token", got.Token)
            assert.True(t, got.TrustPrivateNetwork)
        })
    }
}

func TestReadAuthConfigRejectsUnknownNonDisplayKey(t *testing.T) {
    home := t.TempDir()
    t.Setenv("KATA_HOME", home)
    require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[auth]
token = "client-token"

[display_typo]
markdown_renderer = ["renderer"]
`), 0o600))

    _, err := config.ReadAuthConfig()
    require.Error(t, err)
    assert.Contains(t, err.Error(), "display_typo.markdown_renderer")
}
```

The display-reader tests keep active rendering strict. The daemon and auth
tests protect the other half of the client-only boundary: valid and
semantically malformed display content must not stop daemon resolution or an
auth-only client from reading its auth settings, while an unknown non-display
key still fails.

- [ ] **Step 2: Run the focused tests and confirm the missing API failure**

Run:

```sh
go test ./internal/config -run 'TestRead(DisplayConfig|DaemonConfigTreatsDisplayAsOpaque|AuthConfig(TreatsDisplayAsOpaque|RejectsUnknownNonDisplayKey))' -count=1
```

Expected: FAIL because `DisplayConfig`, `ReadDisplayConfig`, and the
opaque-display exception in both shared readers do not exist.

- [ ] **Step 3: Make the common decoder recognize an opaque client section**

Keep `[display]` out of `DaemonConfig`: the daemon must not gain a renderer
setting. In both `ReadDaemonConfig` and `ReadAuthConfig`, retain strict
unknown-key rejection for every other section but exclude keys whose first TOML
path segment is exactly `display`:

```go
func isDisplayConfigKey(key toml.Key) bool {
    return len(key) > 0 && key[0] == "display"
}
```

Apply this predicate only when filtering `meta.Undecoded()` in each reader:

```go
if u := meta.Undecoded(); len(u) > 0 {
    keys := make([]string, 0, len(u))
    for _, k := range u {
        if !isDisplayConfigKey(k) {
            keys = append(keys, k.String())
        }
    }
    if len(keys) > 0 {
        return nil, fmt.Errorf("parse %s: unknown key(s): %s", path, strings.Join(keys, ", "))
    }
}
```

Use each reader's own zero-value return in place of `nil` where required. The
file still has to be syntactically valid TOML. Do not ignore similarly named
sections such as `[display_typo]`. This duplicate filter is deliberate:
auth-only commands call `ReadAuthConfig` without `ReadDaemonConfig`, so leaving
the former strict on `[display]` would let a client-only preference block
authentication.

- [ ] **Step 4: Add the separate focused display reader**

Add the type and reader in `daemon_config.go`:

```go
type DisplayConfig struct {
    MarkdownRenderer []string `toml:"markdown_renderer"`
}

func ReadDisplayConfig() (DisplayConfig, error) {
    path, err := DaemonConfigPath()
    if err != nil {
        return DisplayConfig{}, err
    }
    data, err := os.ReadFile(path)
    if errors.Is(err, os.ErrNotExist) {
        return DisplayConfig{}, nil
    }
    if err != nil {
        return DisplayConfig{}, fmt.Errorf("read %s: %w", path, err)
    }

    var file struct {
        Display DisplayConfig `toml:"display"`
    }
    meta, err := toml.Decode(string(data), &file)
    if err != nil {
        return DisplayConfig{}, fmt.Errorf("parse %s: %w", path, err)
    }
    var unknown []string
    for _, key := range meta.Undecoded() {
        if isDisplayConfigKey(key) {
            unknown = append(unknown, key.String())
        }
    }
    if len(unknown) > 0 {
        return DisplayConfig{}, fmt.Errorf(
            "parse %s: unknown display key(s): %s", path, strings.Join(unknown, ", "),
        )
    }
    if len(file.Display.MarkdownRenderer) > 0 && file.Display.MarkdownRenderer[0] == "" {
        return DisplayConfig{}, fmt.Errorf("display.markdown_renderer executable must not be empty")
    }
    return file.Display, nil
}
```

Do not trim, split, normalize, or append to `MarkdownRenderer`; exact argv
preservation is the contract. This decoder ignores unrelated top-level
sections semantically, although invalid TOML syntax anywhere in the shared file
still fails parsing.

- [ ] **Step 5: Run config tests and verify green**

Run:

```sh
go test ./internal/config -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the client configuration boundary**

Invoke the commit and privacy-scrub skills, then run:

```sh
git add internal/config/daemon_config.go internal/config/daemon_config_test.go
git commit -m "Add client Markdown renderer preference"
```

The commit body should explain why renderer validation occurs only when a
client requests rendering and why argv is preserved exactly.

---

### Task 3: Share Process-Tree Cleanup

**Files:**

- Create: `internal/processtree/tree.go`
- Create: `internal/processtree/tree_unix.go`
- Create: `internal/processtree/tree_windows.go`
- Create: `internal/processtree/tree_test.go`
- Modify: `internal/hooks/runner.go`
- Delete: `internal/hooks/proc_unix.go`
- Delete: `internal/hooks/proc_windows.go`
- Verify: `internal/hooks/runner_unix_test.go`

**Interfaces:**

- Produces: `processtree.Prepare(cmd *exec.Cmd)`.
- Produces:
  `processtree.TerminateWithGrace(cmd *exec.Cmd, grace time.Duration) error`.
- Preserves hook-runner timeout and shutdown classifications.

- [ ] **Step 1: Write the failing no-process lifecycle test**

Create `internal/processtree/tree_test.go`:

```go
package processtree

import (
    "os/exec"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestTerminateWithGraceNoProcess(t *testing.T) {
    cmd := exec.Command("unused")
    require.NoError(t, TerminateWithGrace(cmd, time.Millisecond))
}
```

- [ ] **Step 2: Run the focused test and confirm the missing API failure**

Run:

```sh
go test ./internal/processtree
```

Expected: FAIL because `TerminateWithGrace` does not exist.

- [ ] **Step 3: Extract the process-tree implementation**

Create `internal/processtree/tree.go`:

```go
package processtree

import (
    "errors"
    "os/exec"
    "time"
)

func Prepare(cmd *exec.Cmd) {
    prepare(cmd)
}

func TerminateWithGrace(cmd *exec.Cmd, grace time.Duration) error {
    if cmd.Process == nil {
        return nil
    }
    var errs []error
    if err := terminate(cmd); err != nil {
        errs = append(errs, err)
    }
    deadline := time.Now().Add(grace)
    for time.Now().Before(deadline) && alive(cmd) {
        time.Sleep(10 * time.Millisecond)
    }
    if alive(cmd) {
        if err := kill(cmd); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(errs...)
}
```

Move the current Unix `Setpgid`, negative-PID SIGTERM/SIGKILL, and signal-zero
liveness logic into `tree_unix.go` as `prepare`, `terminate`, `kill`, and
`alive`. Move the current Windows no-op preparation, leader kill, and
leader-state fallback into `tree_windows.go` under the same private names.

- [ ] **Step 4: Run the new package test and verify green**

Run:

```sh
go test ./internal/processtree -count=1
```

Expected: PASS.

- [ ] **Step 5: Route the hook runner through the shared package**

In `internal/hooks/runner.go`, replace:

```go
applyProcessGroupAttrs(cmd)
```

with:

```go
processtree.Prepare(cmd)
```

Replace the body of `killTreeWithGrace` with:

```go
func killTreeWithGrace(cmd *exec.Cmd, grace time.Duration, daemonLog *log.Logger) {
    if err := processtree.TerminateWithGrace(cmd, grace); err != nil {
        daemonLog.Printf("hooks: terminate process tree: %v", err)
    }
}
```

Remove `waitGroupGone` and delete the two hook-local platform files only after
the shared calls compile.

- [ ] **Step 6: Verify hook cleanup remains unchanged**

Run:

```sh
go test ./internal/hooks -run 'TestRunner_KillTree_OrphanedChildrenDieToo|TestRunner' -count=1
```

Expected: PASS. On Unix, the orphan-cleanup regression must complete well
before its 30-second helper sleep.

- [ ] **Step 7: Commit the process-tree extraction**

Invoke the commit and privacy-scrub skills, then run:

```sh
git add internal/processtree internal/hooks/runner.go internal/hooks/proc_unix.go internal/hooks/proc_windows.go
git commit -m "Share subprocess tree cleanup"
```

The commit body should explain that external Markdown renderers need the same
no-orphan timeout behavior already proven by hooks.

---

### Task 4: Implement Built-In And External Field Renderers

**Files:**

- Create: `cmd/kata/show_markdown.go`
- Create: `cmd/kata/show_markdown_test.go`
- Create: `cmd/kata/show_markdown_unix_test.go`
- Modify: `cmd/kata/render.go`

**Interfaces:**

- Produces:
  `type markdownFieldKind string` with `markdownDescription` and
  `markdownComment`.
- Produces:
  `type showMarkdownRenderer interface { Render(context.Context,
markdownFieldKind, string, int) (string, error) }`.
- Produces:
  `newBuiltinShowMarkdownRenderer(profile colorprofile.Profile)
showMarkdownRenderer`.
- Produces:
  `newExternalShowMarkdownRenderer(argv []string) showMarkdownRenderer`.
- Produces:
  `configuredShowMarkdownRenderer(display config.DisplayConfig,
rows *rowRenderer) showMarkdownRenderer`.
- Adds `rowRenderer.markdownRenderer() showMarkdownRenderer` without changing
  row rendering.

- [ ] **Step 1: Add the helper-process test entrypoint**

Create `cmd/kata/show_markdown_test.go` with one helper shared by the external
renderer tests:

```go
func TestShowMarkdownRendererHelperProcess(t *testing.T) {
    if os.Getenv("GO_WANT_SHOW_MARKDOWN_HELPER") != "1" {
        return
    }
    marker := slices.Index(os.Args, "--")
    if marker < 0 || marker+1 >= len(os.Args) {
        os.Exit(20)
    }
    mode := os.Args[marker+1]
    switch mode {
    case "echo", "echo-newline":
        payload, err := io.ReadAll(os.Stdin)
        if err != nil {
            os.Exit(21)
        }
        fmt.Printf("arg=%s env=%s input=%s", os.Args[marker+2], os.Getenv("SHOW_RENDER_ENV"), payload)
        if mode == "echo-newline" {
            fmt.Println()
        }
    case "fail":
        payload, _ := io.ReadAll(os.Stdin)
        _, _ = fmt.Fprintf(os.Stderr, "renderer rejected %s", payload)
        os.Exit(9)
    case "wait":
        select {}
    case "spawn-descendant":
        readyPath := os.Args[marker+2]
        child := exec.Command(
            os.Args[0], "-test.run=TestShowMarkdownRendererHelperProcess", "--", "wait",
        )
        child.Env = os.Environ()
        if err := child.Start(); err != nil {
            os.Exit(23)
        }
        if err := os.WriteFile(readyPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
            os.Exit(24)
        }
        select {}
    default:
        os.Exit(22)
    }
    os.Exit(0)
}

func helperRenderer(mode string, extra ...string) *externalShowMarkdownRenderer {
    argv := []string{os.Args[0], "-test.run=TestShowMarkdownRendererHelperProcess", "--", mode}
    argv = append(argv, extra...)
    return &externalShowMarkdownRenderer{
        argv: argv, timeout: 500 * time.Millisecond, grace: 50 * time.Millisecond,
    }
}
```

This helper is justified by the shared backend tests and the Unix process-group
tests; do not add a one-off helper for any single assertion.

- [ ] **Step 2: Write failing renderer-backend tests**

Add these behaviors to `show_markdown_test.go`:

```go
func TestExternalShowMarkdownRendererPassesArgvEnvAndStdin(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    t.Setenv("SHOW_RENDER_ENV", "inherited")
    renderer := helperRenderer("echo", "argument with spaces")

    got, err := renderer.Render(context.Background(), markdownComment, "**hello**", 80)
    require.NoError(t, err)
    assert.Equal(t, "arg=argument with spaces env=inherited input=**hello**", got)
}

func TestExternalShowMarkdownRendererNormalizesFinalNewlineAtReinsertion(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    var got [][]string
    for _, mode := range []string{"echo", "echo-newline"} {
        renderer := helperRenderer(mode, "argument")
        rendered, err := renderer.Render(context.Background(), markdownComment, "body", 80)
        require.NoError(t, err)
        got = append(got, markdownrender.ANSIWrappedLines(rendered, 80))
    }
    assert.Equal(t, got[0], got[1])
}

func TestExternalShowMarkdownRendererDiscardsStderr(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    renderer := helperRenderer("fail")

    _, err := renderer.Render(context.Background(), markdownComment, "private body", 80)
    require.Error(t, err)
    assert.Contains(t, err.Error(), "renderer")
    assert.Contains(t, err.Error(), "comment")
    assert.NotContains(t, err.Error(), "private body")
    assert.NotContains(t, err.Error(), "renderer rejected")
}

func TestExternalShowMarkdownRendererNamesMissingExecutable(t *testing.T) {
    executable := filepath.Join(t.TempDir(), "missing-renderer")
    renderer := newExternalShowMarkdownRenderer([]string{executable})

    _, err := renderer.Render(context.Background(), markdownDescription, "private body", 80)
    require.Error(t, err)
    assert.Contains(t, err.Error(), executable)
    assert.Contains(t, err.Error(), "description")
    assert.NotContains(t, err.Error(), "private body")
}

func TestExternalShowMarkdownRendererTimesOutPerInvocation(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    renderer := helperRenderer("wait")
    renderer.timeout = 50 * time.Millisecond

    started := time.Now()
    _, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
    require.Error(t, err)
    assert.ErrorIs(t, err, context.DeadlineExceeded)
    assert.Less(t, time.Since(started), time.Second)
}

func TestExternalShowMarkdownRendererPreservesParentCancellation(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    renderer := helperRenderer("wait")
    ctx, cancel := context.WithCancel(context.Background())
    cancel()

    _, err := renderer.Render(ctx, markdownDescription, "body", 80)
    require.Error(t, err)
    assert.ErrorIs(t, err, context.Canceled)
}

func TestBuiltinShowMarkdownRendererUsesSharedStyle(t *testing.T) {
    rows := newRowRendererFor(colorprofile.ANSI256)
    got, err := rows.markdownRenderer().Render(
        context.Background(), markdownDescription, "## Steps", 80,
    )
    require.NoError(t, err)
    assert.Contains(t, textsafe.StripANSI(got), "Steps")
    assert.NotContains(t, got, "## Steps")

    var styled bytes.Buffer
    _, err = fmt.Fprint(rows.downsample(&styled), got)
    require.NoError(t, err)
    assert.Contains(t, styled.String(), "\x1b[")

    noColorRows := newRowRendererFor(colorprofile.NoTTY)
    plain, err := noColorRows.markdownRenderer().Render(
        context.Background(), markdownDescription, "## Steps", 80,
    )
    require.NoError(t, err)
    var noColor bytes.Buffer
    _, err = fmt.Fprint(noColorRows.downsample(&noColor), plain)
    require.NoError(t, err)
    assert.NotContains(t, noColor.String(), "\x1b[")
    assert.Equal(t, textsafe.StripANSI(styled.String()), noColor.String())
}

func TestConfiguredShowMarkdownRendererSelectsOverride(t *testing.T) {
    rows := newRowRendererFor(colorprofile.ANSI256)
    got := configuredShowMarkdownRenderer(config.DisplayConfig{
        MarkdownRenderer: []string{"renderer", "--flag"},
    }, rows)
    external, ok := got.(*externalShowMarkdownRenderer)
    require.True(t, ok)
    assert.Equal(t, []string{"renderer", "--flag"}, external.argv)
}
```

Add `cmd/kata/show_markdown_unix_test.go` with `//go:build !windows`. It tests
the external renderer's process-group wiring directly, rather than treating the
hook runner's existing coverage as a proxy. The helper starts a `wait`
descendant and writes its PID to a readiness file only after `child.Start`
succeeds. The tests wait for that signal before exercising cancellation, so a
fast cancellation cannot pass without ever creating a descendant:

```go
//go:build !windows

package main

import (
    "context"
    "errors"
    "io/fs"
    "os"
    "path/filepath"
    "strconv"
    "syscall"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestExternalShowMarkdownRendererTimeoutKillsDescendant(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    renderer := helperRenderer("spawn-descendant", filepath.Join(t.TempDir(), "ready"))
    renderer.timeout = 100 * time.Millisecond
    renderer.grace = 50 * time.Millisecond

    errCh := make(chan error, 1)
    go func() {
        _, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
        errCh <- err
    }()

    pid := waitForHelperPID(t, renderer.argv[len(renderer.argv)-1])
    err := <-errCh
    require.ErrorIs(t, err, context.DeadlineExceeded)
    requireProcessGone(t, pid)
}

func TestExternalShowMarkdownRendererCancellationKillsDescendant(t *testing.T) {
    t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
    renderer := helperRenderer("spawn-descendant", filepath.Join(t.TempDir(), "ready"))
    renderer.grace = 50 * time.Millisecond
    ctx, cancel := context.WithCancel(context.Background())
    t.Cleanup(cancel)

    errCh := make(chan error, 1)
    go func() {
        _, err := renderer.Render(ctx, markdownDescription, "body", 80)
        errCh <- err
    }()

    pid := waitForHelperPID(t, renderer.argv[len(renderer.argv)-1])
    cancel()
    err := <-errCh
    require.ErrorIs(t, err, context.Canceled)
    requireProcessGone(t, pid)
}

func waitForHelperPID(t *testing.T, readyPath string) int {
    t.Helper()
    deadline := time.Now().Add(2 * time.Second)
    for time.Now().Before(deadline) {
        data, err := os.ReadFile(readyPath)
        if err == nil {
            pid, err := strconv.Atoi(string(data))
            require.NoError(t, err)
            return pid
        }
        require.ErrorIs(t, err, fs.ErrNotExist)
        time.Sleep(10 * time.Millisecond)
    }
    t.Fatalf("renderer helper did not signal readiness")
    return 0
}

func requireProcessGone(t *testing.T, pid int) {
    t.Helper()
    deadline := time.Now().Add(2 * time.Second)
    for time.Now().Before(deadline) {
        if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
            return
        }
        time.Sleep(10 * time.Millisecond)
    }
    t.Fatalf("renderer descendant %d survived cancellation", pid)
}
```

The short timeout and grace values are test seams only; production keeps the
exact 10-second per-invocation timeout and two-second grace contract.

- [ ] **Step 3: Run the focused tests and confirm the missing API failure**

Run:

```sh
go test ./cmd/kata -run 'Test(External|Builtin|Configured)ShowMarkdownRenderer' -count=1
```

Expected: FAIL because the renderer types, constants, and constructors do not
exist. On Unix, the descendant tests also fail until the external backend wires
its command into the shared process-group lifecycle.

- [ ] **Step 4: Implement the renderer interface and built-in backend**

Create `cmd/kata/show_markdown.go` with:

```go
const (
    rendererTimeout = 10 * time.Second
    rendererGrace   = 2 * time.Second
)

type markdownFieldKind string

const (
    markdownDescription markdownFieldKind = "description"
    markdownComment     markdownFieldKind = "comment"
)

type showMarkdownRenderer interface {
    Render(context.Context, markdownFieldKind, string, int) (string, error)
}

type builtinShowMarkdownRenderer struct {
    codeBlockBackground *string
}

func newBuiltinShowMarkdownRenderer(profile colorprofile.Profile) showMarkdownRenderer {
    var background *string
    switch profile {
    case colorprofile.ANSI, colorprofile.ANSI256, colorprofile.TrueColor:
        value := "236"
        background = &value
    }
    return &builtinShowMarkdownRenderer{codeBlockBackground: background}
}

func (r *builtinShowMarkdownRenderer) Render(
    _ context.Context,
    _ markdownFieldKind,
    markdown string,
    width int,
) (string, error) {
    return markdownrender.Render(markdown, markdownrender.Options{
        Width: width, CodeBlockBackground: r.codeBlockBackground,
    })
}
```

Add to `rowRenderer`:

```go
func (r *rowRenderer) markdownRenderer() showMarkdownRenderer {
    return newBuiltinShowMarkdownRenderer(r.profile)
}
```

- [ ] **Step 5: Implement the external backend without stderr exposure**

Implement `externalShowMarkdownRenderer` with explicit test seams:

```go
type externalShowMarkdownRenderer struct {
    argv    []string
    timeout time.Duration
    grace   time.Duration
}

func newExternalShowMarkdownRenderer(argv []string) showMarkdownRenderer {
    return &externalShowMarkdownRenderer{
        argv: append([]string(nil), argv...),
        timeout: rendererTimeout,
        grace: rendererGrace,
    }
}
```

Its `Render` method must:

1. derive a per-call timeout with `context.WithTimeout`;
2. call `exec.CommandContext(renderCtx, argv[0], argv[1:]...)`;
3. leave `cmd.Env` nil;
4. set stdin to the sanitized field and stdout to a `bytes.Buffer`;
5. set stderr to `io.Discard` so renderer diagnostics cannot echo a field into
   kata's error path;
6. call `processtree.Prepare(cmd)`;
7. set `cmd.Cancel` to call
   `processtree.TerminateWithGrace(cmd, renderer.grace)`;
8. call `cmd.Run()`; and
9. classify `renderCtx.Err()` before the process error so timeouts and parent
   cancellation preserve `errors.Is` against their context cause.

Use this error builder for process failures:

```go
func rendererError(executable string, kind markdownFieldKind, cause error) error {
    return fmt.Errorf("markdown renderer %q failed for %s: %w", executable, kind, cause)
}
```

Do not forward, capture, or include child stderr. The executable, field kind,
and exit or context cause are the complete kata-owned diagnostic. Users can run
the configured argv directly when they need renderer-specific stderr.

- [ ] **Step 6: Implement configured backend selection**

Add:

```go
func configuredShowMarkdownRenderer(
    display config.DisplayConfig,
    rows *rowRenderer,
) showMarkdownRenderer {
    if len(display.MarkdownRenderer) == 0 {
        return rows.markdownRenderer()
    }
    return newExternalShowMarkdownRenderer(display.MarkdownRenderer)
}
```

- [ ] **Step 7: Run renderer-backend tests and verify green**

Run:

```sh
go test ./cmd/kata -run 'Test(External|Builtin|Configured)ShowMarkdownRenderer' -count=1
```

Expected: PASS. The timeout test must finish in under one second.
On Unix, both the timeout and cancellation cases must first observe helper
readiness and then observe that descendant exit.

- [ ] **Step 8: Commit renderer backends**

Invoke the commit and privacy-scrub skills, then run:

```sh
git add cmd/kata/show_markdown.go cmd/kata/show_markdown_test.go cmd/kata/show_markdown_unix_test.go cmd/kata/render.go
git commit -m "Add Markdown field renderer backends"
```

The commit body should explain that Glamour is the portable default and that
external argv, environment, width policy, and process lifetime remain
user-owned while kata bounds each invocation's lifetime.

---

### Task 5: Integrate Field-Scoped Rendering Into `kata show`

**Files:**

- Modify: `cmd/kata/show.go`
- Modify: `cmd/kata/show_test.go`
- Modify: `cmd/kata/next.go`
- Modify: `cmd/kata/tty.go`

**Interfaces:**

- Produces: `showRunOptions{Render bool}`.
- Changes:
  `runShow(cmd *cobra.Command, issueRef, agentOperation string, opts showRunOptions) error`.
- Produces:
  `renderShowFields(context.Context, showResponseForCLI,
showMarkdownRenderer, int) (renderedShowFields, error)`.
- Produces:
  `printShowHuman(io.Writer, showResponseForCLI, string,
*renderedShowFields) error`.
- Produces: `outputTerminal(io.Writer) (*os.File, bool)` and
  `terminalWidth(*os.File) int` in `tty.go`.
- `next --full` calls `runShow` with zero-value options and retains current
  behavior.

- [ ] **Step 1: Write failing mode and non-TTY command tests**

Add a fixture helper used by the non-TTY and forced-TTY tests to
`cmd/kata/show_test.go`. Add `go.kenn.io/kata/internal/testenv` to that file's
imports because the helper is shared by both tests:

```go
func createShowMarkdownFixture(
    t *testing.T,
    env *testenv.Env,
    dir string,
    projectID int64,
) string {
    t.Helper()
    type createResponse struct {
        Issue struct {
            ShortID string `json:"short_id"`
        } `json:"issue"`
    }
    created := postJSON[createResponse](
        t,
        env.URL+"/api/v1/projects/"+itoa(projectID)+"/issues",
        map[string]any{
            "actor": "user-a",
            "title": "rendered fields",
            "body": "## Description",
        },
    )
    runCLI(t, env, dir, "--as", "user-b", "comment", created.Issue.ShortID,
        "--body", "- comment item")
    return created.Issue.ShortID
}
```

Then add:

```go
func TestShowRenderRejectsStructuredModes(t *testing.T) {
    env, dir, pid := setupCLIWorkspace(t)
    ref := createIssue(t, env, pid, "rendered issue")
    for _, mode := range []string{"--json", "--agent"} {
        t.Run(mode, func(t *testing.T) {
            _, err := runCLICapture(t, env, dir, mode, "show", ref, "--render")
            cliErr := requireCLIError(t, err, ExitUsage)
            assert.Equal(t, kindUsage, cliErr.Kind)
            assert.Contains(t, cliErr.Message, "--render requires human output")
        })
    }
}

func TestShowRenderNonTTYKeepsPlainOutputAndSkipsDisplayConfig(t *testing.T) {
    env, dir, pid := setupCLIWorkspace(t)
    ref := createShowMarkdownFixture(t, env, dir, pid)
    home := t.TempDir()
    t.Setenv("KATA_HOME", home)
    require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[display]
markdown_renderer = "not-an-array"
future_option = true
`), 0o600))

    plain := runCLI(t, env, dir, "show", ref)
    rendered := runCLI(t, env, dir, "show", ref, "--render")
    assert.Equal(t, plain, rendered)
    assert.Contains(t, rendered, "## Description")
}
```

- [ ] **Step 2: Run the new command tests and confirm the unknown-flag failure**

Run:

```sh
go test ./cmd/kata -run 'TestShowRender(RejectsStructuredModes|NonTTYKeepsPlainOutputAndSkipsDisplayConfig)' -count=1
```

Expected: FAIL because `show` does not define `--render`.

- [ ] **Step 3: Bind the flag and preserve `next --full`**

Change `newShowCmd` to bind a local boolean and pass options:

```go
func newShowCmd() *cobra.Command {
    var render bool
    cmd := &cobra.Command{
        Use:   "show <issue-ref>",
        Short: "show issue + comments",
        Long: `Show an issue and its comments.

--render renders only description and comment Markdown when stdout is a terminal.
Redirects and pipelines, including "| less -R", keep plain output.`,
        Args: cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            return runShow(cmd, args[0], "show", showRunOptions{Render: render})
        },
    }
    cmd.Flags().BoolVar(&render, "render", false, "render description and comment Markdown on a terminal")
    return cmd
}
```

At the top of `runShow`, before project resolution or an HTTP request, reject a
render request outside human mode:

```go
if opts.Render && currentOutputMode() != outputHuman {
    return &cliError{
        Message: "kata show --render requires human output",
        Kind: kindUsage,
        ExitCode: ExitUsage,
    }
}
```

Update `next.go` to call:

```go
return runShow(cmd, issueRef, "next", showRunOptions{})
```

- [ ] **Step 4: Add output-terminal and width helpers through the existing seam**

Add to `cmd/kata/tty.go` without another `term.IsTerminal` call:

```go
const fallbackTerminalWidth = 80

func outputTerminal(w io.Writer) (*os.File, bool) {
    f, ok := w.(*os.File)
    if !ok || !isTTY(f) {
        return nil, false
    }
    return f, true
}

func terminalWidth(f *os.File) int {
    width, _, err := term.GetSize(int(f.Fd()))
    if err != nil || width < 1 {
        return fallbackTerminalWidth
    }
    return width
}
```

The second `term` call is `GetSize`, not another terminal-policy resolver.

- [ ] **Step 5: Run mode and non-TTY tests and verify green**

Run:

```sh
go test ./cmd/kata -run 'TestShowRender(RejectsStructuredModes|NonTTYKeepsPlainOutputAndSkipsDisplayConfig)|TestNext_' -count=1
```

Expected: PASS. The non-TTY test must succeed despite the wrong renderer type
and unknown display key, proving the focused display decoder was not called.

- [ ] **Step 6: Write the failing field-scope renderer test**

Add a `recordingShowMarkdownRenderer` used by multiple show tests:

```go
type recordedMarkdownField struct {
    kind markdownFieldKind
    body string
    width int
}

type recordingShowMarkdownRenderer struct {
    calls []recordedMarkdownField
    failAt int
}

func (r *recordingShowMarkdownRenderer) Render(
    _ context.Context,
    kind markdownFieldKind,
    body string,
    width int,
) (string, error) {
    r.calls = append(r.calls, recordedMarkdownField{kind: kind, body: body, width: width})
    if r.failAt > 0 && len(r.calls) == r.failAt {
        return "", errors.New("render failed")
    }
    return "\x1b[1m" + strings.TrimPrefix(body, "## ") + "\x1b[0m", nil
}
```

Build `showResponseForCLI` fixtures by unmarshalling compact JSON inside each
test. Add these behaviors:

```go
func TestRenderShowFieldsScopesRendererToBodyAndComments(t *testing.T) {
    var response showResponseForCLI
    require.NoError(t, json.Unmarshal([]byte(`{
      "issue":{"short_id":"abc4","uid":"01TEST","title":"# literal title","body":"## Body","status":"open","author":"user-a","metadata":{"#key":"- value"}},
      "comments":[
        {"uid":"01COMMENT1","author":"user-b","body":"- comment"},
        {"uid":"01COMMENT2","author":"user-c","body":"## Second"},
        {"uid":"01COMMENT3","author":"user-d","body":""}
      ],
      "labels":[{"label":"- literal label"}]
    }`), &response))
    renderer := &recordingShowMarkdownRenderer{}

    _, err := renderShowFields(context.Background(), response, renderer, 80)
    require.NoError(t, err)
    require.Len(t, renderer.calls, 3)
    assert.Equal(t, markdownDescription, renderer.calls[0].kind)
    assert.Equal(t, "## Body", renderer.calls[0].body)
    assert.Equal(t, markdownComment, renderer.calls[1].kind)
    assert.Equal(t, "- comment", renderer.calls[1].body)
    assert.Equal(t, markdownComment, renderer.calls[2].kind)
    assert.Equal(t, "## Second", renderer.calls[2].body)
    assert.Less(t, renderer.calls[1].width, 80)
    assert.Less(t, renderer.calls[2].width, 80)
}

```

- [ ] **Step 7: Run the field tests and confirm the missing orchestration failure**

Run:

```sh
go test ./cmd/kata -run TestRenderShowFieldsScopesRendererToBodyAndComments -count=1
```

Expected: FAIL because `renderShowFields` and `renderedShowFields` do not exist.

- [ ] **Step 8: Implement pre-rendered field preparation**

Add to `show.go`:

```go
type renderedShowFields struct {
    body     []string
    comments [][]string
}

func renderShowFields(
    ctx context.Context,
    response showResponseForCLI,
    renderer showMarkdownRenderer,
    width int,
) (renderedShowFields, error) {
    fields := renderedShowFields{comments: make([][]string, len(response.Comments))}
    if response.Issue.Body != "" {
        rendered, err := renderer.Render(
            ctx, markdownDescription, textsafe.Block(response.Issue.Body), width,
        )
        if err != nil {
            return renderedShowFields{}, err
        }
        fields.body = markdownrender.ANSIWrappedLines(rendered, width)
    }
    for i, comment := range response.Comments {
        if comment.Body == "" {
            continue
        }
        prefix := showCommentPrefix(comment.UID, comment.Author)
        fieldWidth := max(1, width-ansi.StringWidth(prefix))
        rendered, err := renderer.Render(
            ctx, markdownComment, textsafe.Block(comment.Body), fieldWidth,
        )
        if err != nil {
            return renderedShowFields{}, err
        }
        fields.comments[i] = markdownrender.ANSIWrappedLines(rendered, fieldWidth)
    }
    return fields, nil
}
```

Define `showCommentPrefix(uid, author string) string` once using
`textsafe.Line`, and use it in raw and rendered printing so the chrome cannot
drift between paths.

- [ ] **Step 9: Run the field tests and verify green**

Run:

```sh
go test ./cmd/kata -run TestRenderShowFieldsScopesRendererToBodyAndComments -count=1
```

Expected: PASS.

- [ ] **Step 10: Write the failing ANSI-safe comment-layout test**

Add:

```go
func TestPrintShowHumanIndentsRenderedCommentWithANSIWidth(t *testing.T) {
    var response showResponseForCLI
    require.NoError(t, json.Unmarshal([]byte(`{
      "issue":{"short_id":"abc4","uid":"01TEST","title":"title","status":"open","author":"user-a"},
      "comments":[{"uid":"01COMMENT","author":"user-b","body":"source"}]
    }`), &response))
    fields := &renderedShowFields{
        comments: [][]string{{"\x1b[31mfirst\x1b[0m", "\x1b[31msecond\x1b[0m"}},
    }
    var out bytes.Buffer

    require.NoError(t, printShowHuman(&out, response, "example-workspace", fields))
    plain := textsafe.StripANSI(out.String())
    prefix := "01COMMENT user-b: "
    assert.Contains(t, plain, prefix+"first\n"+strings.Repeat(" ", ansi.StringWidth(prefix))+"second\n")
    assert.Contains(t, out.String(), "\x1b[31mfirst\x1b[0m")
    assert.Contains(t, out.String(), "\x1b[31msecond\x1b[0m")
}
```

- [ ] **Step 11: Run the layout test and confirm the missing printer failure**

Run:

```sh
go test ./cmd/kata -run TestPrintShowHumanIndentsRenderedCommentWithANSIWidth -count=1
```

Expected: FAIL because `printShowHuman` does not yet accept rendered fields or
indent continuation lines.

- [ ] **Step 12: Extract the current human printer and add rendered insertion**

Move the current human-output block from `runShow` into:

```go
func printShowHuman(
    out io.Writer,
    response showResponseForCLI,
    subjectProject string,
    rendered *renderedShowFields,
) error
```

When `rendered == nil`, preserve every existing `fmt` call and raw-body byte.
When it is non-nil:

- write body lines after the same single blank line;
- write a comment's first rendered line directly after its existing prefix;
- prefix each continuation line with
  `strings.Repeat(" ", ansi.StringWidth(prefix))`;
- write an empty comment as its existing prefix plus an empty body; and
- leave labels, links, metadata, claim lines, headings, and ordering unchanged.

Use a focused helper:

```go
func writeRenderedPrefixedLines(w io.Writer, prefix string, lines []string) error {
    if len(lines) == 0 {
        _, err := fmt.Fprintln(w, prefix)
        return err
    }
    if _, err := fmt.Fprintln(w, prefix+lines[0]); err != nil {
        return err
    }
    indent := strings.Repeat(" ", ansi.StringWidth(prefix))
    for _, line := range lines[1:] {
        if _, err := fmt.Fprintln(w, indent+line); err != nil {
            return err
        }
    }
    return nil
}
```

- [ ] **Step 13: Run layout and existing show tests and verify green**

Run:

```sh
go test ./cmd/kata -run 'Test(PrintShowHumanIndentsRenderedCommentWithANSIWidth|Show_)' -count=1
```

Expected: PASS, including all existing raw human, JSON, agent, claim, link, and
metadata assertions.

- [ ] **Step 14: Write the failing atomic-output test**

Add:

```go
func TestRenderAndPrintShowHumanDoesNotPrintPartialRecord(t *testing.T) {
    var response showResponseForCLI
    require.NoError(t, json.Unmarshal([]byte(`{
      "issue":{"short_id":"abc4","uid":"01TEST","title":"title","body":"body","status":"open","author":"user-a"},
      "comments":[{"uid":"01COMMENT","author":"user-b","body":"comment"}]
    }`), &response))
    renderer := &recordingShowMarkdownRenderer{failAt: 2}
    var out bytes.Buffer

    err := renderAndPrintShowHuman(
        context.Background(), &out, response, "example-workspace", renderer, 80,
    )
    require.Error(t, err)
    assert.Empty(t, out.String())
}
```

- [ ] **Step 15: Run the atomic-output test and confirm the missing helper failure**

Run:

```sh
go test ./cmd/kata -run TestRenderAndPrintShowHumanDoesNotPrintPartialRecord -count=1
```

Expected: FAIL because `renderAndPrintShowHuman` does not exist.

- [ ] **Step 16: Add the render-then-print orchestration boundary**

Add:

```go
func renderAndPrintShowHuman(
    ctx context.Context,
    out io.Writer,
    response showResponseForCLI,
    subjectProject string,
    renderer showMarkdownRenderer,
    width int,
) error {
    rendered, err := renderShowFields(ctx, response, renderer, width)
    if err != nil {
        return err
    }
    return printShowHuman(out, response, subjectProject, &rendered)
}
```

Run:

```sh
go test ./cmd/kata -run TestRenderAndPrintShowHumanDoesNotPrintPartialRecord -count=1
```

Expected: PASS with empty stdout after the second field fails.

- [ ] **Step 17: Wire terminal-gated renderer selection into `runShow`**

After decoding the human response, use:

```go
out := cmd.OutOrStdout()
terminal, shouldRender := outputTerminal(out)
if !opts.Render || !shouldRender {
    return printShowHuman(out, response, ref.ProjectName, nil)
}

display, err := config.ReadDisplayConfig()
if err != nil {
    return err
}
rows := newRowRenderer(out)
width := terminalWidth(terminal)
renderer := configuredShowMarkdownRenderer(display, rows)
return renderAndPrintShowHuman(
    cmd.Context(), rows.downsample(out), response, ref.ProjectName, renderer, width,
)
```

This order is required: structured-mode rejection, fetch/decode, terminal gate,
local config read, complete field rendering, then the first output byte.

- [ ] **Step 18: Add a forced-TTY built-in integration test**

Use an `*os.File` output and the existing `stubIsTTY(t, true)` seam so the
command takes the render branch without adding another profile hook:

```go
func TestShowRenderBuiltinFormatsMarkdownFields(t *testing.T) {
    env, dir, pid := setupCLIWorkspace(t)
    ref := createShowMarkdownFixture(t, env, dir, pid)
    stubIsTTY(t, true)
    t.Setenv("NO_COLOR", "1")
    output, err := os.CreateTemp(t.TempDir(), "show-output-*")
    require.NoError(t, err)
    defer output.Close()

    cmd := newRootCmd()
    cmd.SetOut(output)
    cmd.SetErr(output)
    cmd.SetArgs([]string{"--workspace", dir, "show", ref, "--render"})
    cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
    require.NoError(t, cmd.Execute())
    require.NoError(t, output.Sync())
    _, err = output.Seek(0, io.SeekStart)
    require.NoError(t, err)
    got, err := io.ReadAll(output)
    require.NoError(t, err)

    text := string(got)
    assert.Contains(t, text, "Description")
    assert.Contains(t, text, "- comment item")
    assert.NotContains(t, text, "## Description")
    assert.Contains(t, text, "--- comments ---")
}
```

Keep fixture creation inline unless the exact same helper is used by another
test in this task.

- [ ] **Step 19: Run the complete show and next suites**

Run:

```sh
go test ./cmd/kata -run 'TestShow|TestNext' -count=1
```

Expected: PASS.

- [ ] **Step 20: Commit field-scoped command integration**

Invoke the commit and privacy-scrub skills, then run:

```sh
git add cmd/kata/show.go cmd/kata/show_test.go cmd/kata/next.go cmd/kata/tty.go
git commit -m "Render Markdown fields in kata show"
```

The commit body should explain that kata preserves its record chrome, renders
all fields before printing, and deliberately falls back to plain output for
redirects and pipelines.

---

### Task 6: Document The Client Contract And Verify The Branch

**Files:**

- Modify: `docs/reference/cli.md`
- Modify: `docs/reference/configuration.md`
- Verify: all files changed since `origin/main`

**Interfaces:**

- Documents `kata show <issue-ref> [--render]`.
- Documents `[display].markdown_renderer` as a local client preference.
- Documents the built-in default, exact argv/environment ownership, per-field
  invocation, output normalization, hidden child stderr, and non-TTY pipeline
  behavior.

- [ ] **Step 1: Update the CLI reference**

Change the inspect synopsis to:

```sh
kata show <issue-ref> [--render]
```

Immediately after the list/inspect block, add prose covering:

- only descriptions and comment bodies render;
- header/status/claims/labels/links/metadata remain literal;
- Glamour is the default;
- `--json` and `--agent` are incompatible; and
- redirects and pipelines, including `| less -R`, intentionally remain plain
  because this version has no force-render option.

- [ ] **Step 2: Add the client display section to configuration docs**

Before `## Daemon config` in `docs/reference/configuration.md`, add:

````markdown
## Client display preferences

`[display]` belongs to the client reading `<KATA_HOME>/config.toml`. It is not
sent to a remote daemon and does not change daemon rendering or API responses.

`kata show --render` uses built-in Glamour rendering when no override is set.
To use an external stdin/stdout renderer instead:

```toml
[display]
markdown_renderer = ["leaf"]
```

The value is an argv array. kata passes it verbatim without a shell, appended
flags, width injection, or environment changes. For example:

```toml
[display]
markdown_renderer = ["glow", "-", "-s", "dark", "-w", "80"]
```

kata starts the configured program once per non-empty Markdown field. Because
the child's stdout is captured, users are responsible for renderer-specific
color and width flags or inherited environment variables such as
`CLICOLOR_FORCE`. kata treats output with or without a final newline the same,
while preserving internal blank lines. Renderer stderr is discarded because a
program may echo the Markdown input there; run the configured argv directly to
diagnose renderer-specific failures.

The common daemon-config path recognizes `[display]` without decoding it. kata
validates this client section only when `show --render` is active on a terminal,
so a display-only typo cannot break daemon startup, plain output, or redirected
output.
````

- [ ] **Step 3: Format and validate documentation**

Run:

```sh
prettier --write docs/reference/cli.md docs/reference/configuration.md
markdownlint-cli2 docs/reference/cli.md docs/reference/configuration.md
make docs-check
```

Expected: all commands exit zero.

- [ ] **Step 4: Run focused package verification**

Run:

```sh
go test ./internal/markdownrender ./internal/config ./internal/processtree ./internal/hooks ./internal/tui ./cmd/kata -count=1
```

Expected: PASS.

- [ ] **Step 5: Run repository-wide verification**

Run:

```sh
make test
make lint
make build
git diff --check origin/main...HEAD
```

Expected: every command exits zero. Keep the built `kata` artifact untracked;
remove it with `make clean` after verification if `make build` creates it in the
worktree.

- [ ] **Step 6: Review the final diff against the approved spec**

Run:

```sh
git diff --stat origin/main...HEAD
git diff origin/main...HEAD
git status --short
```

Confirm from the diff that no API, generated client, migration, database schema,
`next --full`, JSON, or agent behavior changed.

- [ ] **Step 7: Commit documentation and final integration adjustments**

Invoke the commit and privacy-scrub skills, then run:

```sh
git add docs/reference/cli.md docs/reference/configuration.md
git commit -m "Document kata show Markdown rendering"
```

If final verification required a source fix, commit that fix separately with a
subject describing the corrected behavior; do not fold it into the docs commit
or amend an earlier commit.

- [ ] **Step 8: Re-run post-commit verification and update the kata issue**

Run:

```sh
make test
make lint
make build
make docs-check
git diff --check origin/main...HEAD
git status --short
```

After all commands pass and the worktree is clean, close issue `5b93` with the
final implementation commit SHA. Use the actual SHA in:

```sh
kata close 5b93 --done \
  --message "Added field-scoped Markdown rendering for kata show with built-in Glamour, a client-local external renderer override, and plain non-TTY fallback; verified repository tests, lint, build, and docs checks pass." \
  --evidence "tests:make test" \
  --evidence "lint:make lint" \
  --evidence "build:make build" \
  --evidence "docs:make docs-check" \
  --commit "$(git rev-parse HEAD)"
```

Do not close it before this evidence exists.
