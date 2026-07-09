package tui

import (
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// modalKind names which centered confirm/info modal is active.
// modalNone is the quiescent state. After M3.5b only the quit-confirm
// case exists; future plans (delete-confirm, etc.) extend the enum.
type modalKind int

const (
	modalNone modalKind = iota
	modalQuitConfirm
)

// renderQuitConfirmModal returns the centered "Are you sure?" panel.
// Mirrors msgvault's modalQuitConfirm: bordered box with the prompt
// text and `[Y] Yes  [N] No` action hint. Width is fixed (~40 cells)
// so the panel stays compact regardless of terminal width.
func renderQuitConfirmModal() string {
	body := strings.Join([]string{
		"Quit kata?",
		"",
		"Are you sure you want to quit?",
		"",
		"[Y] Yes    [N] No",
	}, "\n")
	return modalBoxStyle.Render(body)
}

// overlayModal centers a modal panel over the rendered background
// view. Mirrors msgvault's view.go::overlayModal: split the bg into
// lines, compute centering, splice the modal into the right offset.
//
// ANSI handling: the bg lines may carry escape sequences (lipgloss
// styled chrome), so we use ansiAwareSlice to skip past escapes
// when measuring width and to preserve them when slicing. Without
// ANSI awareness the splice would either count escape bytes toward
// visible width (modal misalignment) or cut a sequence mid-escape
// (terminal control mangling) — see roborev #111 finding 2.
//
// width / height come from Model.width / Model.height. background
// is the already-rendered sub-view (list, detail, help, empty).
//
// Non-fullscreen backgrounds (help, empty) don't pad to height, so
// bgLines is extended with blank rows up to `height` before splicing.
// Without this the modal could land past the last bg line and
// disappear or render only partially on taller terminals — roborev
// #119 finding 1.
func overlayModal(background, modal string, width, height int) string {
	if modal == "" {
		return background
	}
	bgLines := strings.Split(background, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}
	modalLines := strings.Split(modal, "\n")
	modalH := len(modalLines)
	startLine := (height - modalH) / 2
	if startLine < 0 {
		startLine = 0
	}
	modalW := lipgloss.Width(modal)
	leftPad := (width - modalW) / 2
	if leftPad < 0 {
		leftPad = 0
	}
	for i, mLine := range modalLines {
		idx := startLine + i
		if idx >= len(bgLines) {
			break
		}
		bg := bgLines[idx]
		var b strings.Builder
		if leftPad > 0 {
			left, leftWidth := ansiAwarePrefix(bg, leftPad)
			b.WriteString(left)
			if leftWidth < leftPad {
				b.WriteString(strings.Repeat(" ", leftPad-leftWidth))
			}
		}
		b.WriteString(mLine)
		rightStart := leftPad + modalW
		b.WriteString(ansiAwareSuffix(bg, rightStart))
		bgLines[idx] = b.String()
	}
	return strings.Join(bgLines, "\n")
}

// ansiAwarePrefix returns the prefix of s whose visible width is at
// most w. ANSI escape sequences are passed through verbatim and
// don't contribute to width. Returns the (possibly truncated)
// prefix and its visible width.
func ansiAwarePrefix(s string, w int) (string, int) {
	var b strings.Builder
	used := 0
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			end := i + ansiEscapeLen(s[i:])
			if end > len(s) {
				end = len(s)
			}
			b.WriteString(s[i:end])
			i = end
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		rw := runewidth.RuneWidth(r)
		if used+rw > w {
			break
		}
		b.WriteString(s[i : i+sz])
		used += rw
		i += sz
	}
	return b.String(), used
}

// ansiAwareSuffix returns the suffix of s starting after the first
// `skip` visible cells. ANSI escape sequences encountered while
// skipping are concatenated and prefixed onto the returned suffix
// so styling continues correctly past the splice point.
func ansiAwareSuffix(s string, skip int) string {
	used := 0
	var carried strings.Builder
	i := 0
	for i < len(s) && used < skip {
		if s[i] == 0x1b {
			end := i + ansiEscapeLen(s[i:])
			if end > len(s) {
				end = len(s)
			}
			carried.WriteString(s[i:end])
			i = end
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		used += runewidth.RuneWidth(r)
		i += sz
	}
	if i >= len(s) {
		return ""
	}
	return carried.String() + s[i:]
}

// ansiEscapeLen returns the byte length of an ANSI escape sequence
// starting at s[0]. Handles CSI (`ESC [ ... <final>`) and OSC
// (`ESC ] ... BEL or ESC \`) shapes. Falls back to 2 for
// unrecognized escapes so the caller skips ESC + next byte rather
// than looping forever.
func ansiEscapeLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 1
	}
	switch s[1] {
	case '[': // CSI: ESC [ params ... <final byte 0x40-0x7e>
		for i := 2; i < len(s); i++ {
			c := s[i]
			if c >= 0x40 && c <= 0x7e {
				return i + 1
			}
		}
	case ']': // OSC: ESC ] ... (BEL | ESC \)
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
	}
	return 2
}
