package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// renderInputBar formats the inline command bar as a bordered box.
// Used by the M3a above-the-table layout; M3.5 moved bars to the
// info line via renderInfoBar instead. Kept for any caller that
// wants the heavier bordered presentation.
//
//nolint:unused // superseded by renderInfoBar in M3.5
func renderInputBar(s inputState, width int) string {
	if width < 10 {
		width = 10
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(panelActiveBorder).
		Width(width). // border-box in v2: side borders count toward width
		Padding(0, 1)
	var body string
	if field := s.activeField(); field != nil {
		body = sanitizeForDisplay(field.input.View())
	}
	rendered := box.Render(body)
	// Embed the title in the top border via a manual overlay: lipgloss
	// doesn't expose a "title in border" primitive yet, so prepend a
	// labeled top line and let the box's own top border act as the
	// underline.
	title := titleStyle.Render(" " + s.title + " ")
	return title + "\n" + rendered
}

// renderPanelPrompt is the M3b bordered panel-prompt shell. M3.5
// moved panel prompts to the info line via renderInfoPrompt for a
// lighter footprint; this stays for any caller wanting the heavier
// bordered presentation.
//
//nolint:unused // superseded by renderInfoPrompt in M3.5
func renderPanelPrompt(s inputState, width int) string {
	if width < 10 {
		width = 10
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(panelActiveBorder).
		Width(width).
		Padding(0, 1)
	var body string
	if field := s.activeField(); field != nil {
		body = sanitizeForDisplay(field.input.View())
	}
	rendered := box.Render(body)
	title := titleStyle.Render(" " + s.title + " ")
	return title + "\n" + rendered
}

// renderCenteredForm is the centered modal panel for any centered
// form: bordered title strip, body, footer hint inside the box.
// Sized to ~70% of the terminal (capped to 100x24 lines so wide
// windows don't get a stretched form). Composed inline by Model.View
// via overlayModal so the form sits on top of the underlying view's
// background.
//
// Render-time sanitization is applied to the title and footer text
// (trusted package strings, but cheap and consistent) and the
// textarea's view delegates to bubbles' own input-side Sanitize so
// any pasted ANSI sequence is dropped before it reaches the buffer.
// Mutation payloads read the field value untouched — only the
// display layer applies sanitization.
//
// Dispatches on s.kind: the multi-field new-issue form has its own
// renderer that lays out four labeled fields stacked vertically;
// single-field forms (edit-body, comment) keep the M4 layout. Plan 8
// commit 5a added the filter modal, which is multi-field but uses a
// radio for the Status axis instead of a textinput.
func renderCenteredForm(s inputState, width, height int) string {
	if width < formMinWidth || height < formMinHeight {
		return renderTinyFormFallback(s)
	}
	innerW := formInnerWidth(width)
	innerH := formInnerHeight(height)
	switch s.kind {
	case inputNewIssueForm:
		return renderNewIssueForm(s, innerW, innerH)
	case inputFilterForm:
		return renderFilterForm(s, innerW)
	}
	return renderSingleFieldForm(s, innerW, innerH)
}

// renderFilterForm lays out the four filter axes: Status (radio),
// Owner (textinput), Search (textinput), Labels (textinput, comma-
// separated). No body field, so the form reads compactly — title
// strip, four labeled rows, footer hint inside the panel. ctrl+r is
// added to the footer hint so the reset gesture is discoverable;
// ctrl+e is omitted (no $EDITOR for this form). Plan 8 commit 5b
// added the Labels row.
func renderFilterForm(s inputState, innerW int) string {
	if len(s.fields) < 4 {
		return ""
	}
	statusLine := renderFormStatus(s)
	footer := renderFilterFormFooter(s)
	parts := []string{titleStyle.Render(s.title)}
	for i := 0; i < 4; i++ {
		parts = append(parts, renderFilterField(s, i, innerW))
	}
	if statusLine != "" {
		parts = append(parts, statusLine)
	}
	parts = append(parts, footer)
	// Lip Gloss v2 sizes border-box: +2 keeps the interior at innerW,
	// matching the v1 layout where the border sat outside the width.
	return modalBoxStyle.Width(innerW+2).Padding(0, 1).Render(strings.Join(parts, "\n"))
}

// renderFilterField renders one row of the filter form: bold label
// when active, the field's view beneath. Status field renders the
// radio glyphs; Owner/Search render the textinput view (resized to
// the inner width).
func renderFilterField(s inputState, idx, innerW int) string {
	f := &s.fields[idx]
	label := f.label
	if idx == s.active {
		label = titleStyle.Render(label)
	} else {
		label = subtleStyle.Render(label)
	}
	var view string
	if f.kind == fieldRadio {
		view = renderRadio(f.radio, idx == s.active)
	} else {
		f.input.SetWidth(innerW - 2)
		view = f.input.View()
	}
	return label + "\n" + view
}

// renderRadio formats the radio choices as a single line. The
// selected choice gets a filled glyph; the others get empty. Under
// KATA_COLOR_MODE=none we fall back to ASCII so the snapshot
// fixtures stay deterministic across UTF-8-aware terminals. The
// active form field gets a bolded label upstream; we don't bold the
// choices themselves so the row reads like a single statement.
func renderRadio(r radioField, _ bool) string {
	filled, empty := radioGlyphs()
	parts := make([]string, len(r.choices))
	for i, c := range r.choices {
		glyph := empty
		if i == r.index {
			glyph = filled
		}
		parts[i] = glyph + " " + c
	}
	return strings.Join(parts, "   ")
}

// radioGlyphs returns the (filled, empty) glyph pair for radio
// rendering. ASCII fallback under KATA_COLOR_MODE=none keeps the
// snapshot bytes deterministic on terminals whose default font may
// or may not include the ◉/◯ codepoints.
func radioGlyphs() (string, string) {
	if resolveColorMode() == colorNone {
		return "[X]", "[ ]"
	}
	return "◉", "○"
}

// renderFilterFormFooter is the footer hint for the filter form.
// ctrl+e is intentionally absent (this form has no body field). The
// reset gesture (ctrl+r) is surfaced so users can clear all axes
// without leaving the form.
func renderFilterFormFooter(s inputState) string {
	if s.saving {
		return statusStyle.Render("saving…")
	}
	hint := "ctrl+s apply · esc cancel · tab next · ctrl+r reset"
	return subtleStyle.Render(hint)
}

// renderSingleFieldForm renders the M4 single-textarea forms
// (inputBodyEditForm, inputCommentForm). Title strip on top, the
// active textarea filling the interior, optional status line, footer
// hint at the bottom.
func renderSingleFieldForm(s inputState, innerW, innerH int) string {
	f := s.activeField()
	if f == nil {
		return ""
	}
	f.area.SetWidth(innerW)
	f.area.SetHeight(innerH - 2 /* title + footer */)
	body := f.area.View()
	footer := renderFormFooter(s, innerW, true /* allowEditor */)
	statusLine := renderFormStatus(s)
	parts := []string{
		titleStyle.Render(s.title),
		body,
	}
	if statusLine != "" {
		parts = append(parts, statusLine)
	}
	parts = append(parts, footer)
	// Lip Gloss v2 sizes border-box: +2 keeps the interior at innerW,
	// matching the v1 layout where the border sat outside the width.
	return modalBoxStyle.Width(innerW+2).Padding(0, 1).Render(strings.Join(parts, "\n"))
}

// renderNewIssueForm lays out the fields of the multi-field new-issue
// form. Body gets the remaining vertical slack; the other fields are
// single-line. Each field is preceded by a label cell; the active
// field's label renders bold so the user can tell which field has
// focus.
//
// The footer hint flips ctrl+e to "(body only)" when a single-line
// field has focus so the user understands the editor handoff is
// gated to the body textarea.
func renderNewIssueForm(s inputState, innerW, innerH int) string {
	if len(s.fields) < 5 {
		return ""
	}
	statusLine := renderFormStatus(s)
	footer := renderFormFooter(s, innerW, s.active == newIssueFormBodyIndex)
	// Reserve title (1) + footer (1) + status (0 or 1) + one label
	// per field + one row for every single-line field. Body gets the
	// remaining height.
	singleLineRows := len(s.fields) - 1
	reserved := 1 + 1 + len(s.fields) + singleLineRows
	if statusLine != "" {
		reserved++
	}
	bodyRows := innerH - reserved
	if bodyRows < 3 {
		bodyRows = 3
	}
	// Single-line field width = innerW; resize the body textarea so
	// it fills the available width and bodyRows.
	body := &s.fields[1]
	body.area.SetWidth(innerW)
	body.area.SetHeight(bodyRows)
	parts := []string{titleStyle.Render(s.title)}
	for idx := range s.fields {
		parts = append(parts, renderNewIssueField(s, idx, innerW))
	}
	if statusLine != "" {
		parts = append(parts, statusLine)
	}
	parts = append(parts, footer)
	// Lip Gloss v2 sizes border-box: +2 keeps the interior at innerW,
	// matching the v1 layout where the border sat outside the width.
	return modalBoxStyle.Width(innerW+2).Padding(0, 1).Render(strings.Join(parts, "\n"))
}

// renderNewIssueField renders one labeled field row. The label is
// bold when the field has focus; the field's bubbles view is
// rendered beneath the label. Single-line fields render on a single
// row; multi-line fields render with whatever height the textarea
// was set to in renderNewIssueForm.
func renderNewIssueField(s inputState, idx, innerW int) string {
	f := &s.fields[idx]
	label := f.label
	if f.required {
		label += " *"
	}
	if idx == s.active {
		label = titleStyle.Render(label)
	} else {
		label = subtleStyle.Render(label)
	}
	var view string
	if f.kind == fieldMultiLine {
		view = f.area.View()
	} else {
		f.input.SetWidth(innerW - 2)
		view = f.input.View()
	}
	return label + "\n" + view
}

// renderFormFooter is the footer-hint row inside the panel. While
// saving=true the hint flips to a single "saving…" cell so the user
// sees they should wait. allowEditor=false suppresses the ctrl+e
// hint with a "(body only)" parenthetical, used by the new-issue
// form when a single-line field has focus.
func renderFormFooter(s inputState, innerW int, allowEditor bool) string {
	if s.saving {
		return statusStyle.Render("saving…")
	}
	hint := "ctrl+s save · esc cancel · ctrl+e $EDITOR"
	if !allowEditor {
		hint = "ctrl+s save · esc cancel · tab next · ctrl+e (body only)"
	}
	if len(hint) > innerW {
		hint = "ctrl+s save · esc cancel"
	}
	return subtleStyle.Render(hint)
}

// renderFormStatus surfaces in-form errors (editor cancel / error,
// empty-comment block on commit). Empty when no status to show.
func renderFormStatus(s inputState) string {
	if s.err == "" {
		return ""
	}
	return errorStyle.Render(s.err)
}

// renderTinyFormFallback is the degraded render for terminals smaller
// than the form's minimum size. Just dumps the textarea so the user
// can still type; no border / no centering. Useful in narrow CI
// terminals or test fixtures.
func renderTinyFormFallback(s inputState) string {
	f := s.activeField()
	if f == nil {
		return ""
	}
	return s.title + "\n" + f.area.View()
}

// formInnerWidth picks the centered form's interior width. ~70% of
// terminal width, capped at 100 cells so a 200-cell window doesn't
// produce a stretched-out modal that's hard to read.
func formInnerWidth(width int) int {
	w := width * 7 / 10
	if w > 100 {
		w = 100
	}
	if w < formMinWidth {
		w = formMinWidth
	}
	return w
}

// formInnerHeight picks the centered form's interior height. Caps
// at 24 rows so the modal is roughly screen-sized, not full-screen.
func formInnerHeight(height int) int {
	h := height * 7 / 10
	if h > 24 {
		h = 24
	}
	if h < formMinHeight {
		h = formMinHeight
	}
	return h
}
