package tui

import (
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// editorCmd suspends Bubble Tea, runs $EDITOR on a tmpfile pre-seeded
// with `template`, and returns editorReturnedMsg with the final
// content. kind is one of "edit" | "comment" so the caller knows
// which mutation context the buffer was for; under M4 routing, kind
// is informational only — the formGen tag is the actual stale-
// handoff guard.
//
// formGen is stamped on the editorReturnedMsg so the Model-level
// router can match the return against the currently-open form (or
// drop it if the form has since closed or re-opened against a
// different issue). Pass 0 for the legacy detail-side shell-out
// path that doesn't go through a form.
//
// tea.ExecProcess tears down the renderer, runs the child process, and
// re-establishes the renderer when the child exits. While the child is
// running, the terminal belongs to $EDITOR — Bubble Tea is paused, so
// keys (including 'q') do not reach our handlers.
func editorCmd(kind, template string, formGen int64) tea.Cmd {
	tmp, err := os.CreateTemp("", "kata-*.md")
	if err != nil {
		return func() tea.Msg {
			return editorReturnedMsg{kind: kind, formGen: formGen, err: err}
		}
	}
	// tmp.Name() is os.CreateTemp's tmp path, not user input — nolint:gosec
	// silences G703 (path-traversal via taint) for the os.Remove/ReadFile
	// calls below.
	name := tmp.Name()
	if _, err := tmp.WriteString(template); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name) //nolint:gosec // path is os.CreateTemp output
		return func() tea.Msg {
			return editorReturnedMsg{kind: kind, formGen: formGen, err: err}
		}
	}
	_ = tmp.Close()
	editor := resolveEditor()
	//nolint:gosec // user-controlled $EDITOR is intentional
	cmd := exec.Command(editor[0], append(editor[1:], name)...)
	return tea.ExecProcess(cmd, func(execErr error) tea.Msg {
		defer func() { _ = os.Remove(name) }() //nolint:gosec // path is os.CreateTemp output
		if execErr != nil {
			return editorReturnedMsg{kind: kind, formGen: formGen, err: execErr}
		}
		content, err := os.ReadFile(name) //nolint:gosec // path is os.CreateTemp output
		if err != nil {
			return editorReturnedMsg{kind: kind, formGen: formGen, err: err}
		}
		return editorReturnedMsg{
			kind: kind, formGen: formGen, content: string(content),
		}
	})
}

// resolveEditor returns the command (with args) to invoke. Honors
// $VISUAL > $EDITOR per POSIX convention; falls back to vi.
func resolveEditor() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := os.Getenv(env); v != "" {
			parts := strings.Fields(v)
			if len(parts) > 0 {
				return parts
			}
		}
	}
	return []string{"vi"}
}
