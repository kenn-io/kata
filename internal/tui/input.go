package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// inputKind discriminates which input shell is active. Plan 7 §"Input
// shell taxonomy": three shells (inline command bar, panel-local
// prompt, centered form) backed by one shared component family.
//
// M3a implements only inputSearchBar (the inline command bar; the
// matching owner bar was retired in Plan 8 commit 5a when the `o`
// key was subsumed by the centered filter modal). M3b adds the
// panel-local prompt kinds; M4 adds the centered form kinds.
type inputKind int

const (
	inputNone inputKind = iota
	inputSearchBar
	inputLabelPrompt       // detail `+` — add label
	inputRemoveLabelPrompt // detail `-` — remove label
	inputOwnerPrompt       // detail `a` — assign owner
	inputParentPrompt      // detail `p` — set parent
	inputBlockerPrompt     // detail `b` — add blocker
	inputLinkPrompt        // detail `l` — add related link
	inputPriorityPrompt    // detail `!` — set priority (0..4 or '-' to clear)
	inputNewIssueForm      // list `n` — multi-field modal: Title/Body/Labels/Owner
	inputBodyEditForm      // detail `e` — centered multi-line body editor
	inputCommentForm       // detail `c` — centered multi-line comment editor
	inputFilterForm        // list `f` — multi-axis filter modal: Status/Owner/Search
)

// isPanelPrompt reports whether a kind is one of the M3b panel-local
// prompt kinds (anchored to the bottom of the detail pane).
func (k inputKind) isPanelPrompt() bool {
	switch k {
	case inputLabelPrompt, inputRemoveLabelPrompt, inputOwnerPrompt,
		inputParentPrompt, inputBlockerPrompt, inputLinkPrompt,
		inputPriorityPrompt:
		return true
	}
	return false
}

// isCommandBar reports whether a kind is one of the M3a inline
// command bar kinds (replaces the chip strip). Plan 8 commit 5a
// retired the owner bar; only the search bar remains.
func (k inputKind) isCommandBar() bool {
	return k == inputSearchBar
}

// isCenteredForm reports whether a kind is one of the centered form
// kinds (multi-line textarea, ctrl+s commit, esc cancel, ctrl+e
// $EDITOR escape hatch). M4 introduced edit-body and comment;
// Plan 8 commit 4 added the multi-field new-issue form; Plan 8
// commit 5a added the filter modal. The filter modal renders via
// the centered overlay but its commit path is filter-apply, not
// mutation-dispatch — see Model.commitInput for the explicit early
// branch that keeps it out of commitFormInput.
func (k inputKind) isCenteredForm() bool {
	switch k {
	case inputBodyEditForm, inputCommentForm, inputNewIssueForm, inputFilterForm:
		return true
	}
	return false
}

// fieldKind picks the bubbles component backing an inputField.
type fieldKind int

const (
	fieldSingleLine fieldKind = iota
	fieldMultiLine
	fieldRadio // Plan 8 commit 5a — finite-choice axis (e.g. Status all/open/closed)
)

// radioField backs a fieldRadio inputField: a finite set of choices
// and the index of the currently-selected one. Used by the filter
// modal's Status axis (all/open/closed) where free-form text would
// be a worse fit than a discrete cycle. set("") falls back to the
// first choice so callers don't have to special-case "no value yet".
type radioField struct {
	choices []string
	index   int
}

// value returns the currently-selected choice, or "" when no choices
// are configured (defensive — every constructor seeds at least one).
func (r radioField) value() string {
	if len(r.choices) == 0 {
		return ""
	}
	idx := r.index
	if idx < 0 || idx >= len(r.choices) {
		idx = 0
	}
	return r.choices[idx]
}

// set positions the radio on s. Empty string and unknown values pin
// to index 0 — Status="" historically means "all", which is the
// expected first choice.
func (r *radioField) set(s string) {
	for i, c := range r.choices {
		if c == s {
			r.index = i
			return
		}
	}
	r.index = 0
}

// cycle advances by delta with wrap. delta=+1 is the space key's
// "next" gesture; ←/→ pass +1/-1 directly. Empty choice list is a
// no-op so callers don't have to guard.
func (r *radioField) cycle(delta int) {
	n := len(r.choices)
	if n == 0 {
		return
	}
	r.index = ((r.index+delta)%n + n) % n
}

// inputField is one editable field on an input. Bars and prompts have
// a single field; centered forms have two or more. The bubbles models
// are populated based on kind — never both at once.
//
// label appears above each field in the multi-field new-issue form;
// required gates whether commit can proceed when the field is blank
// (Title is the only required field today). Bars and prompts that
// only ever show one field leave both empty.
//
// The radio member backs fieldRadio kinds (Plan 8 commit 5a — Status
// axis on the filter modal); the bubbles models stay zero-valued.
type inputField struct {
	label    string
	kind     fieldKind
	input    textinput.Model // populated when kind == fieldSingleLine
	area     textarea.Model  // populated when kind == fieldMultiLine
	radio    radioField      // populated when kind == fieldRadio
	required bool
	locked   bool
}

// value returns the current text from whichever bubbles model backs f.
func (f *inputField) value() string {
	switch f.kind {
	case fieldMultiLine:
		return f.area.Value()
	case fieldRadio:
		return f.radio.value()
	}
	return f.input.Value()
}

// setValue mirrors a string into whichever bubbles model backs f.
// Used by the $EDITOR escape hatch (M4) when handing a buffer back to
// a multi-line field on resume; for fieldRadio this positions the
// radio on the matching choice (or first choice on no match).
func (f *inputField) setValue(s string) {
	switch f.kind {
	case fieldMultiLine:
		f.area.SetValue(s)
	case fieldRadio:
		f.radio.set(s)
	default:
		f.input.SetValue(s)
	}
}

// focus / blur delegate to the bubbles model so cursor visibility +
// key dispatch flip correctly when the active field changes. Used by
// the new-issue form when tab cycles fields between Title, Body,
// Labels, and Owner. fieldRadio carries no bubbles cursor (the
// rendered glyph is the focus indicator) so focus/blur are no-ops.
func (f *inputField) focus() tea.Cmd {
	switch f.kind {
	case fieldMultiLine:
		return f.area.Focus()
	case fieldRadio:
		return nil
	}
	return f.input.Focus()
}

func (f *inputField) blur() {
	switch f.kind {
	case fieldMultiLine:
		f.area.Blur()
	case fieldRadio:
		// no-op
	default:
		f.input.Blur()
	}
}

// inputState carries every active-input case. The renderer dispatches
// on kind to pick the chrome (bar / prompt / form). The data path is
// uniform — caller drives keys through Update; on actionCommit it
// reads field values and dispatches the mutation; on actionCancel it
// discards and restores any pre-open snapshot.
//
// preFilter is the listFilter snapshot captured when an inline
// command bar opened, so a cancel can revert any live-applied changes.
// Empty filter for non-bar inputs.
//
// target / err / saving / formGen are populated for centered-form and
// panel-prompt kinds. target carries the issue context so a stale
// editor return / label-suggestion fetch cannot land on the wrong
// issue; formGen is the per-form monotonic ID (assigned by
// Model.openInput at form-open time) used to reject stale
// editorReturnedMsg whose form has since closed or re-opened.
//
// suggestHighlight / suggestScroll back the autocomplete menu on the
// `+` and `-` panel prompts. highlight is the index of the selected
// suggestion (cycles 0..N-1 with wrap on ↑↓); scroll is the first
// visible row in the menu when the entry list overflows the menu's
// height. Both are zero on every non-suggesting input kind.
type inputState struct {
	kind             inputKind
	title            string
	fields           []inputField
	active           int
	err              string
	saving           bool
	preFilter        ListFilter
	target           formTarget
	formGen          int64
	suggestHighlight int
	suggestScroll    int
}

// formTarget carries the issue identity a centered form is acting
// on. Threaded into the form when it opens, into the editor handoff
// (so the return can be matched against the still-open form), and
// into the mutation dispatch (so a stale response on the daemon
// side can be discarded against detail.gen). projectID is zero and
// issueShortID is empty for forms that don't yet have a target
// (none today, but the shape leaves room for forward-looking
// shells).
type formTarget struct {
	projectID    int64
	issueShortID string
	detailGen    int64
}

// inputAction names what the caller should do after Update. Actions
// drive the Model-level handler, not the input itself.
type inputAction int

const (
	actionNone inputAction = iota
	actionCommit
	actionCancel
	// actionEditorHandoff: a centered form requested the $EDITOR
	// escape hatch (ctrl+e). Model-level handler launches editorCmd
	// with the form's current buffer and formGen tag; the resulting
	// editorReturnedMsg writes the content back into the form.
	actionEditorHandoff
)

// activeField returns a pointer to the currently-focused field so
// callers can read its value or mutate it (e.g. ctrl+e handoff).
func (s *inputState) activeField() *inputField {
	if s == nil || len(s.fields) == 0 {
		return nil
	}
	idx := s.active
	if idx < 0 || idx >= len(s.fields) {
		idx = 0
	}
	return &s.fields[idx]
}

// Update routes a key into the active field and reports the action
// the caller should take. Centered forms route differently from bars
// and prompts: ctrl+s commits (Enter inserts a newline into the
// textarea); ctrl+e requests the $EDITOR escape hatch; saving=true
// blocks duplicate commits while a mutation is in flight.
//
// Label-prompt kinds (`+` / `-`) intercept ↑/↓/⇥ BEFORE delegating to
// the textinput so the autocomplete menu's highlight cursor moves
// (and ⇥ completes) without the keys reaching bubbles' own input
// handler — bubbles would otherwise interpret arrow keys as intra-
// buffer cursor motion, which makes no sense for a single-line cell.
// The Update path returns actionNone for those keys; the caller's
// suggestion source (m.suggestionsForPrompt) is consulted at render
// time to project the new highlight.
func (s inputState) Update(msg tea.KeyPressMsg) (inputState, inputAction) {
	if s.kind.isCenteredForm() {
		return s.updateForm(msg)
	}
	switch msg.String() {
	case "enter":
		return s, actionCommit
	case "esc":
		return s, actionCancel
	case "ctrl+u":
		if f := s.activeField(); f != nil {
			f.setValue("")
		}
		return s, actionNone
	}
	if isLabelPromptKind(s.kind) {
		if next, handled := s.handleSuggestKey(msg); handled {
			return next, actionNone
		}
	}
	return s.delegateToField(msg)
}

// isLabelPromptKind reports whether kind is one of the autocomplete-
// backed panel-prompt kinds (`+` add label, `-` remove label).
func isLabelPromptKind(k inputKind) bool {
	return k == inputLabelPrompt || k == inputRemoveLabelPrompt
}

// handleSuggestKey dispatches the navigation keys that the suggestion
// menu owns: ↑/↓ move the highlight (with wrap), ⇥ completes the
// active buffer to the highlighted suggestion's label. Returns
// handled=true when the key was consumed by the menu so the caller
// knows not to forward it to the textinput (which would otherwise
// move the buffer cursor or insert a tab character).
//
// The actual suggestion list isn't on inputState — it's recomputed at
// render time from m.suggestionsForPrompt — so handleSuggestKey only
// adjusts the highlight index. Callers wrap the index modulo the
// projected list length when they read it; we don't need to know the
// length here. ⇥ is a no-op when the buffer is empty (no completion
// target candidate yet); the renderer surfaces the suggestion list
// either way.
func (s inputState) handleSuggestKey(msg tea.KeyPressMsg) (inputState, bool) {
	switch msg.String() {
	case "up":
		s.suggestHighlight--
		return s, true
	case "down":
		s.suggestHighlight++
		return s, true
	case "tab":
		// Tab completion of the buffer happens at the Model layer
		// where the suggestion list (LabelCount slice) is in scope.
		// Here we just signal "handled" so the textinput doesn't
		// receive the tab keystroke. The completion itself lives in
		// Model.completeFromSuggestion (called from routeInputKey).
		return s, true
	}
	return s, false
}

// updateForm is the Update path for centered forms. ctrl+s commits
// (Model-level handler validates kind-specific empty rules); esc
// cancels; ctrl+e hands off to $EDITOR (gated to the body field on
// multi-field forms so the user does not get $EDITOR for a one-line
// owner string); tab/shift+tab cycle fields with wrap on multi-field
// forms; enter on a single-line field advances to the next field
// rather than committing (commit is ctrl+s only).
//
// On single-field forms (edit-body, comment) tab and shift+tab fall
// through to the textarea so the user can still indent. The Body
// field on the multi-field form does the same — multi-line content
// can use tab natively. The cycling only triggers for the single-line
// fields on a multi-field form.
//
// While saving=true, ctrl+s is absorbed (no duplicate dispatches).
//
// The filter form (Plan 8 commit 5a) honors ctrl+r as a reset gesture
// (zeros every field; preFilter intact for esc) and routes ←/→/space
// through the radio when the Status field is active.
func (s inputState) updateForm(msg tea.KeyPressMsg) (inputState, inputAction) {
	if next, action, handled := s.handleFormControlKey(msg); handled {
		return next, action
	}
	if next, handled := s.handleFormNavKey(msg); handled {
		return next, actionNone
	}
	if next, handled := s.handleLockedFieldKey(msg); handled {
		return next, actionNone
	}
	if next, handled := s.handleRadioKey(msg); handled {
		return next, actionNone
	}
	return s.delegateToField(msg)
}

// handleFormControlKey handles the action-emitting key family
// (ctrl+s, esc, ctrl+e, ctrl+r). handled=true means the key was
// consumed (the action may still be actionNone for absorbed gestures
// like ctrl+r and the saving=true ctrl+s gate).
func (s inputState) handleFormControlKey(
	msg tea.KeyPressMsg,
) (inputState, inputAction, bool) {
	switch msg.String() {
	case "ctrl+s":
		if s.saving {
			return s, actionNone, true
		}
		return s, actionCommit, true
	case "esc":
		return s, actionCancel, true
	case "ctrl+e":
		if !s.ctrlEAllowed() {
			return s, actionNone, true
		}
		return s, actionEditorHandoff, true
	case "ctrl+r":
		if s.kind == inputFilterForm {
			return s.resetFilterFields(), actionNone, true
		}
	}
	return s, actionNone, false
}

// handleFormNavKey handles tab / shift+tab / enter for field cycling.
// Returns handled=true when the key was consumed by the navigation
// layer; otherwise the caller falls through to the next handler.
func (s inputState) handleFormNavKey(msg tea.KeyPressMsg) (inputState, bool) {
	switch msg.String() {
	case "tab":
		if s.shouldCycleFields() {
			return s.advanceField(1), true
		}
	case "shift+tab":
		if s.shouldCycleFields() {
			return s.advanceField(-1), true
		}
	case "enter":
		if s.shouldAdvanceOnEnter() {
			return s.advanceField(1), true
		}
	}
	return s, false
}

// handleRadioKey routes ←/→/space into the active radio field. Used
// only when the active field is fieldRadio; other fields fall through
// so arrow keys move the textinput cursor and space inserts a literal
// space character. handled=true means the key was consumed.
func (s inputState) handleRadioKey(msg tea.KeyPressMsg) (inputState, bool) {
	f := s.activeField()
	if f == nil || f.kind != fieldRadio {
		return s, false
	}
	switch msg.String() {
	case "left":
		f.radio.cycle(-1)
	case "right", "space":
		f.radio.cycle(1)
	default:
		return s, false
	}
	s.fields[s.active] = *f
	return s, true
}

func (s inputState) handleLockedFieldKey(msg tea.KeyPressMsg) (inputState, bool) {
	f := s.activeField()
	if f == nil || !f.locked {
		return s, false
	}
	switch msg.String() {
	case "backspace", "delete", "ctrl+u":
		f.setValue("")
		f.locked = false
		s.fields[s.active] = *f
	}
	return s, true
}

// resetFilterFields zeroes every filter-form field to its empty value.
// preFilter stays intact so a subsequent esc still restores the
// at-open snapshot — ctrl+r is a "start over inside the form" gesture,
// not "discard the form." Plan 8 commit 5b: now resets the Labels
// field as well (4th field).
func (s inputState) resetFilterFields() inputState {
	if s.kind != inputFilterForm || len(s.fields) < 4 {
		return s
	}
	s.fields[0].radio.set("all")
	s.fields[1].input.SetValue("")
	s.fields[2].input.SetValue("")
	s.fields[3].input.SetValue("")
	return s
}

// ctrlEAllowed reports whether the current form should treat ctrl+e
// as an $EDITOR handoff. Single-field forms (edit-body, comment) are
// always allowed; the multi-field new-issue form only honors ctrl+e
// when the body textarea owns focus; the filter form has no body
// field at all so $EDITOR is never offered.
func (s inputState) ctrlEAllowed() bool {
	switch s.kind {
	case inputFilterForm:
		return false
	case inputNewIssueForm:
		return s.active == newIssueFormBodyIndex
	}
	return true
}

// shouldCycleFields reports whether tab/shift+tab on this form should
// advance to the next field rather than insert a literal tab. The
// multi-field new-issue form cycles, and so does the filter form
// (Status/Owner/Search). Single-field forms let tab fall through to
// the textarea so users can indent body text.
func (s inputState) shouldCycleFields() bool {
	if len(s.fields) <= 1 {
		return false
	}
	return s.kind == inputNewIssueForm || s.kind == inputFilterForm
}

// shouldAdvanceOnEnter reports whether enter on the active single-
// line field should advance to the next field. The body field on the
// new-issue form stays as a newline-insert (textarea native
// behavior); other fields treat enter as "next field." Commit is
// reserved for ctrl+s. The filter form has no multi-line field, so
// every field advances on enter.
func (s inputState) shouldAdvanceOnEnter() bool {
	if len(s.fields) <= 1 {
		return false
	}
	switch s.kind {
	case inputFilterForm:
		return true
	case inputNewIssueForm:
		return s.active != newIssueFormBodyIndex
	}
	return false
}

// advanceField cycles s.active by delta with wrap. Blurs the
// previously-focused field and focuses the new one so the bubbles
// cursor renders on the active field only. The Focus() tea.Cmd is
// discarded — we don't drive cursor blink from the form.
func (s inputState) advanceField(delta int) inputState {
	if len(s.fields) == 0 {
		return s
	}
	s.fields[s.active].blur()
	n := len(s.fields)
	s.active = ((s.active+delta)%n + n) % n
	_ = s.fields[s.active].focus()
	return s
}

// delegateToField forwards a message into the active field's bubbles
// model so paste, cursor motion, backspace, arrow keys all work. It
// accepts tea.Msg (not just key presses) because Bubble Tea v2
// delivers pastes as tea.PasteMsg, which the bubbles models ingest
// directly. fieldRadio carries no bubbles model (the navigation/
// cycling keys are intercepted upstream by handleRadioKey) so the
// call is a no-op for that kind.
func (s inputState) delegateToField(msg tea.Msg) (inputState, inputAction) {
	f := s.activeField()
	if f == nil {
		return s, actionNone
	}
	switch f.kind {
	case fieldMultiLine:
		var cmd tea.Cmd
		f.area, cmd = f.area.Update(msg)
		_ = cmd
	case fieldRadio:
		// no bubbles model to delegate to.
		return s, actionNone
	default:
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		_ = cmd
	}
	s.fields[s.active] = *f
	return s, actionNone
}

// newSearchBar constructs the inline command bar for `/` (search).
// preFilter snapshots the caller's current filter so a cancel can
// revert. The bar text input has no placeholder — empty bar reads as
// "type to search."
func newSearchBar(current ListFilter) inputState {
	ti := textinput.New()
	ti.SetValue(current.Search)
	ti.Focus()
	ti.Prompt = ""
	return inputState{
		kind:      inputSearchBar,
		title:     "search",
		fields:    []inputField{{kind: fieldSingleLine, input: ti}},
		preFilter: current,
	}
}

// newPanelPrompt constructs an M3b panel-local prompt for kind. The
// title carries the issue context so the user sees "add label to #42"
// in the prompt's border. target carries projectID + issueNumber +
// detailGen so the autocomplete dispatch (label suggestions) and any
// future stale-response checks can scope themselves to the right
// issue.
func newPanelPrompt(kind inputKind, target formTarget) inputState {
	ti := textinput.New()
	ti.Focus()
	ti.Prompt = ""
	return inputState{
		kind:   kind,
		title:  panelPromptTitle(kind, target.issueShortID),
		fields: []inputField{{kind: fieldSingleLine, input: ti}},
		target: target,
	}
}

// panelPromptTitle is the verbal label that appears in the prompt
// border. Mirrors the modalLabel mapping from the now-retired
// modal.go but reads as a sentence ("add label to #abc4") rather
// than a CLI-style colon prefix.
func panelPromptTitle(kind inputKind, ref string) string {
	switch kind {
	case inputLabelPrompt:
		return fmt.Sprintf("add label to #%s", ref)
	case inputRemoveLabelPrompt:
		return fmt.Sprintf("remove label from #%s", ref)
	case inputOwnerPrompt:
		return fmt.Sprintf("assign #%s to", ref)
	case inputParentPrompt:
		return fmt.Sprintf("set parent of #%s", ref)
	case inputBlockerPrompt:
		return fmt.Sprintf("add blocker to #%s", ref)
	case inputLinkPrompt:
		return fmt.Sprintf("add related link to #%s", ref)
	case inputPriorityPrompt:
		return fmt.Sprintf("set priority of #%s (0..4 or '-')", ref)
	}
	return ""
}

// formMinHeight / formMinWidth are the smallest terminal cells we'll
// open a centered form on. Below either, the form falls back to a
// degraded inline render via renderTinyFormFallback.
const (
	formMinHeight = 12
	formMinWidth  = 40
)

// newBodyEditForm constructs the centered multi-line editor opened by
// `e` on the detail view. current pre-fills the textarea with the
// existing body so the user starts on top of what's there. esc
// closes the form (returns to detail); ctrl+s dispatches EditBody;
// ctrl+e suspends to $EDITOR.
func newBodyEditForm(target formTarget, current string) inputState {
	return inputState{
		kind:   inputBodyEditForm,
		title:  fmt.Sprintf("edit body of #%s", target.issueShortID),
		fields: []inputField{newFormTextarea(current)},
		target: target,
	}
}

// newCommentForm is the centered multi-line comment editor opened
// by `c` on the detail view. esc cancels (no comment posted);
// ctrl+s dispatches AddComment; empty content blocks commit per the
// kind-specific gate (comments must have content; clearing a body is
// legitimate but posting an empty comment is not).
func newCommentForm(target formTarget) inputState {
	return inputState{
		kind:   inputCommentForm,
		title:  fmt.Sprintf("comment on #%s", target.issueShortID),
		fields: []inputField{newFormTextarea("")},
		target: target,
	}
}

// newFormTextarea builds the bubbles textarea backing a centered
// form's only field. Pre-filled with current; focused so the cursor
// renders immediately; soft-wrap on so long lines fold inside the
// modal panel instead of horizontal-scrolling.
func newFormTextarea(current string) inputField {
	ta := textarea.New()
	ta.SetValue(current)
	ta.Focus()
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	return inputField{kind: fieldMultiLine, area: ta}
}

// newIssueForm*Index names fields inside newNewIssueForm. ctrl+e is
// allowed only on Body; Parent is optional and may start locked for
// the `N` new-child flow.
const (
	newIssueFormBodyIndex   = 1
	newIssueFormParentIndex = 4
)

// filterFormStatusChoices is the canonical Status-axis choice list for
// the filter modal. "all" maps to ListFilter.Status="" on commit so
// the wire stays the same; the surface form uses the verbal "all" so
// the radio reads naturally.
var filterFormStatusChoices = []string{"all", "open", "closed"}

// newFilterForm constructs the centered multi-axis filter modal opened
// by `f` on the list view. Plan 8 commit 5a shipped Status/Owner/Search;
// commit 5b adds the Labels axis (any-of semantics). Four axes:
//   - Status: tri-state radio (all/open/closed) — pre-positioned on
//     current.Status (empty maps to "all").
//   - Owner: single-line textinput, pre-filled from current.Owner.
//   - Search: single-line textinput, pre-filled from current.Search.
//   - Labels: single-line comma-separated textinput, pre-filled from
//     current.Labels (joined with ", ").
//
// preFilter snapshots the at-open ListFilter so esc can restore the
// previous narrowing without re-typing. The form has no daemon
// mutation — commit applies the filter and refetches; routing is
// handled by Model.commitInput's explicit early branch (see the
// load-bearing comment there).
//
// The Labels field is comma-separated text (mirrors the new-issue
// form's Labels field). Wiring an autocomplete suggestion menu inside
// the form is deferred to a follow-up — the suggestion-menu render
// path currently anchors on the detail view's panel, and overlaying it
// inside a centered form would require a second anchoring strategy.
// In all-projects scope the field already falls back to free-typed
// text (no project label cache to source from); the deferral keeps
// the same shape for the single-project case until the overlay
// rework lands.
func newFilterForm(current ListFilter) inputState {
	status := inputField{
		label: "Status",
		kind:  fieldRadio,
		radio: radioField{choices: filterFormStatusChoices},
	}
	status.radio.set(current.Status)
	owner := textinput.New()
	owner.SetValue(current.Owner)
	owner.Prompt = ""
	owner.Blur()
	search := textinput.New()
	search.SetValue(current.Search)
	search.Prompt = ""
	search.Blur()
	labels := textinput.New()
	labels.SetValue(joinLabelsForFilterForm(current.Labels))
	labels.Prompt = ""
	labels.Blur()
	return inputState{
		kind:  inputFilterForm,
		title: "filter",
		fields: []inputField{
			status,
			{kind: fieldSingleLine, input: owner, label: "Owner"},
			{kind: fieldSingleLine, input: search, label: "Search"},
			{kind: fieldSingleLine, input: labels, label: "Labels"},
		},
		preFilter: current,
	}
}

// joinLabelsForFilterForm flattens a Labels slice into the
// comma-separated form the filter form's Labels field expects. nil and
// empty slice both produce "". Used to pre-fill the field on open so a
// re-open sees the labels the user previously committed.
func joinLabelsForFilterForm(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return strings.Join(labels, ", ")
}

// newNewIssueForm constructs the multi-field modal opened by `n` on
// the list view. Title is required and focused on open; Body is the
// only multi-line field (so ctrl+e for $EDITOR escape only fires when
// it owns focus); Labels accepts a comma-separated string normalized
// at commit time; Owner is a single-line textinput, nil-on-wire when
// blank. tab cycles fields with wrap; ctrl+s commits; esc cancels.
func newNewIssueForm() inputState {
	return newNewIssueFormBase("new issue")
}

func newNewIssueFormWithParent(parentShortID string) inputState {
	s := newNewIssueFormBase("new child issue")
	s.fields[newIssueFormParentIndex].input.SetValue(parentShortID)
	s.fields[newIssueFormParentIndex].locked = true
	return s
}

func newNewIssueFormBase(title string) inputState {
	ti := textinput.New()
	ti.Prompt = ""
	body := textarea.New()
	body.ShowLineNumbers = false
	body.Prompt = ""
	body.Blur()
	labels := textinput.New()
	labels.Prompt = ""
	labels.Blur()
	owner := textinput.New()
	owner.Prompt = ""
	owner.Blur()
	parent := textinput.New()
	parent.Prompt = ""
	parent.Blur()
	ti.Focus()
	return inputState{
		kind:  inputNewIssueForm,
		title: title,
		fields: []inputField{
			{kind: fieldSingleLine, input: ti, label: "Title", required: true},
			{kind: fieldMultiLine, area: body, label: "Body"},
			{kind: fieldSingleLine, input: labels, label: "Labels"},
			{kind: fieldSingleLine, input: owner, label: "Owner"},
			{kind: fieldSingleLine, input: parent, label: "Parent"},
		},
	}
}
