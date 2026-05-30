package tui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// assertNoSourceReference scans every non-test .go file in the
// current package directory and fails when sym appears in any of
// them. Used by the negative-grep tests to guard against accidental
// re-introduction of removed symbols.
func assertNoSourceReference(t *testing.T, sym string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		buf, err := os.ReadFile(path) //nolint:gosec // path is under cwd
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(buf), sym) {
			t.Fatalf("symbol %q must not appear in source file %s "+
				"(commit 4 dropped it)", sym, name)
		}
	}
}

// newIssueFormFixture returns a Model already at the list view with a
// resolved actor and a single-project scope so the `n` keybinding can
// open the form. Mirrors mFixtureForBar but seeds the scope so the
// all-projects gate doesn't fire.
func newIssueFormFixture() Model {
	return Model{
		view:   viewList,
		keymap: newKeymap(),
		scope:  scope{projectID: 7, projectName: "kata"},
		list:   listModel{actor: "tester"},
		cache:  newIssueCache(),
	}
}

// openNewIssueForm opens the form via the n keystroke + the resulting
// openInputCmd, returning a model with m.input.kind == inputNewIssueForm.
func openNewIssueForm(t *testing.T, m Model) Model {
	t.Helper()
	m, cmd := stepModel(m, runeKey('n'))
	if cmd == nil {
		t.Fatalf("press n produced no cmd; expected openInputCmd")
	}
	m, _ = stepModel(m, cmd())
	assertInputKind(t, m, inputNewIssueForm)
	return m
}

// primeLabelCache seeds m.projectLabels with a known entry for projectID
// at gen so tests can detect dispatchLabelFetch effects (gen advance,
// fetching=true) or, conversely, confirm a stale-message path left the
// entry untouched.
func primeLabelCache(m *Model, projectID, gen int64) {
	m.projectLabels = newLabelCache()
	m.projectLabels.byProject[projectID] = labelCacheEntry{
		pid: projectID, gen: gen,
		labels: []LabelCount{{Label: "old", Count: 1}},
	}
	m.nextLabelsGen = gen
}

func focusNewIssueField(s inputState, idx int) inputState {
	for i := range s.fields {
		s.fields[i].blur()
	}
	s.active = idx
	_ = s.fields[idx].focus()
	return s
}

// TestNewIssueForm_OpensOnNKey_ListView: pressing n on the list view
// opens the centered multi-field form (replaces the M3.5c inline row).
func TestNewIssueForm_OpensOnNKey_ListView(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	if len(m.input.fields) != 5 {
		t.Fatalf("form fields = %d, want 5 (Title/Body/Labels/Owner/Parent)", len(m.input.fields))
	}
	wantLabels := []string{"Title", "Body", "Labels", "Owner", "Parent"}
	for i, f := range m.input.fields {
		if f.label != wantLabels[i] {
			t.Fatalf("field[%d].label = %q, want %q", i, f.label, wantLabels[i])
		}
	}
	if !m.input.fields[0].required {
		t.Fatal("Title field must be required")
	}
}

// TestNewIssueForm_AllProjectsScopeIsNoOp: in cross-project view there
// is no projectID to create against, so n surfaces a status hint and
// does NOT open the form.
func TestNewIssueForm_AllProjectsScopeIsNoOp(t *testing.T) {
	m := newIssueFormFixture()
	m.scope = scope{allProjects: true}
	nm, cmd := stepModel(m, runeKey('n'))
	if cmd != nil {
		t.Fatalf("expected nil cmd in all-projects mode, got %T", cmd)
	}
	assertInputKind(t, nm, inputNone)
	if nm.list.status == "" {
		t.Fatal("expected a status hint explaining the no-op")
	}
}

// TestNewIssueForm_ConstructorBlursAllFieldsFocusesField0: every
// non-active field is blurred so only the focused field renders the
// bubbles cursor, and the active field starts at index 0 (Title).
func TestNewIssueForm_ConstructorBlursAllFieldsFocusesField0(t *testing.T) {
	s := newNewIssueForm()
	if s.active != 0 {
		t.Fatalf("active = %d, want 0 (Title)", s.active)
	}
	if !s.fields[0].input.Focused() {
		t.Fatal("field[0] (Title) must be focused")
	}
	if s.fields[1].area.Focused() {
		t.Fatal("field[1] (Body) must be blurred")
	}
	if s.fields[2].input.Focused() {
		t.Fatal("field[2] (Labels) must be blurred")
	}
	if s.fields[3].input.Focused() {
		t.Fatal("field[3] (Owner) must be blurred")
	}
	if s.fields[4].input.Focused() {
		t.Fatal("field[4] (Parent) must be blurred")
	}
}

// TestNewIssueForm_TabCyclesFieldsWithWrap: tab cycles 0→1→2→3→4→0 and
// blurs/focuses the right fields each step.
func TestNewIssueForm_TabCyclesFieldsWithWrap(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	for _, want := range []int{1, 2, 3, 4, 0} {
		m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
		assertActiveField(t, m, want)
	}
}

// TestNewIssueForm_ShiftTabReverseCyclesWithWrap: shift+tab cycles
// 0→4→3→2→1→0 with wrap.
func TestNewIssueForm_ShiftTabReverseCyclesWithWrap(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	for _, want := range []int{4, 3, 2, 1, 0} {
		m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyShiftTab})
		assertActiveField(t, m, want)
	}
}

// TestNewIssueForm_EnterInSingleLineAdvancesField: enter on a single-
// line field advances to the next field instead of committing. Title
// → Body, Labels → Owner, Owner → Parent, Parent → Title (wrap).
func TestNewIssueForm_EnterInSingleLineAdvancesField(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	m, cmd := stepModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter on Title dispatched a cmd %T; expected advance only", cmd)
	}
	assertActiveField(t, m, 1)
	// Skip Body — enter inserts a newline there. Cycle to Labels (idx 2).
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
	assertActiveField(t, m, 2)
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assertActiveField(t, m, 3)
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assertActiveField(t, m, 4)
	// Enter on Parent wraps to Title.
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assertActiveField(t, m, 0)
}

// TestNewIssueForm_EnterInBodyInsertsNewline: enter on the body field
// stays as a textarea newline insert (no advance).
func TestNewIssueForm_EnterInBodyInsertsNewline(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	// Tab to Body.
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
	assertActiveField(t, m, 1)
	// Type a line then enter then another line.
	m = typeString(m, "line1")
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assertActiveField(t, m, 1)
	m = typeString(m, "line2")
	body := m.input.fields[1].area.Value()
	if !strings.Contains(body, "line1") || !strings.Contains(body, "line2") {
		t.Fatalf("body missing one of the lines: %q", body)
	}
	if !strings.Contains(body, "\n") {
		t.Fatalf("body missing newline; got %q", body)
	}
}

// TestNewIssueForm_CtrlSEmptyTitleSetsErrNoDispatch: ctrl+s with a
// blank Title sets the in-form err and does NOT dispatch.
func TestNewIssueForm_CtrlSEmptyTitleSetsErrNoDispatch(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	nm, cmd := stepModel(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd != nil {
		t.Fatalf("empty-title ctrl+s dispatched cmd %T; want nil", cmd)
	}
	assertInputKind(t, nm, inputNewIssueForm)
	if nm.input.err == "" {
		t.Fatal("expected err on empty-title commit")
	}
	if nm.input.saving {
		t.Fatal("saving must NOT flip true on empty-title commit")
	}
}

// TestNewIssueForm_CtrlSTitleOnly_DispatchesWithMinimalPayload:
// ctrl+s with only a Title dispatches CreateIssue with empty body,
// nil owner, nil labels.
func TestNewIssueForm_CtrlSTitleOnly_DispatchesWithMinimalPayload(t *testing.T) {
	api := &fakeListAPI{createResult: &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz"}}}
	m := openNewIssueForm(t, newIssueFormFixture())
	m = typeString(m, "fix bug")
	// Drive dispatchCreateIssue directly to assert the wire shape; the
	// commit cmd uses Model.api which is *Client and unfittable to
	// fakeListAPI without major plumbing.
	_, cmd := m.list.dispatchCreateIssue(
		api, m.scope, "fix bug", "", nil, nil,
	)
	if cmd == nil {
		t.Fatal("expected dispatch cmd from non-empty title")
	}
	cmd()
	if api.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", api.createCalls)
	}
	if api.lastCreateBody.Title != "fix bug" {
		t.Fatalf("title = %q, want fix bug", api.lastCreateBody.Title)
	}
	if api.lastCreateBody.Body != "" {
		t.Fatalf("body = %q, want empty", api.lastCreateBody.Body)
	}
	if api.lastCreateBody.Owner != nil {
		t.Fatalf("owner = %v, want nil", api.lastCreateBody.Owner)
	}
	if api.lastCreateBody.Labels != nil {
		t.Fatalf("labels = %v, want nil", api.lastCreateBody.Labels)
	}
}

// TestNewIssueForm_CtrlSAllFields_NormalizedPayload: with every field
// populated, commit produces the normalized payload — title sent
// untrimmed, owner trimmed, empty label tokens dropped, whitespace-
// only owner omitted.
func TestNewIssueForm_CtrlSAllFields_NormalizedPayload(t *testing.T) {
	api := &fakeListAPI{createResult: &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz"}}}
	owner := "  alice  "
	labels := normalizeLabels("bug, , prio-1 ,  , feature")
	owned := normalizeOwner(owner)
	if owned == nil || *owned != "alice" {
		t.Fatalf("normalizeOwner mishandled trim: %v", owned)
	}
	wantLabels := []string{"bug", "prio-1", "feature"}
	if len(labels) != len(wantLabels) {
		t.Fatalf("normalizeLabels = %v, want %v", labels, wantLabels)
	}
	for i, w := range wantLabels {
		if labels[i] != w {
			t.Fatalf("labels[%d] = %q, want %q", i, labels[i], w)
		}
	}
	// Title sent untrimmed.
	_, cmd := listModel{actor: "tester"}.dispatchCreateIssue(
		api, scope{projectID: 7},
		"  spaced title  ", "body content", labels, owned,
	)
	if cmd == nil {
		t.Fatal("expected dispatch cmd")
	}
	cmd()
	if api.lastCreateBody.Title != "  spaced title  " {
		t.Fatalf("title = %q, want untrimmed", api.lastCreateBody.Title)
	}
	if api.lastCreateBody.Body != "body content" {
		t.Fatalf("body = %q, want body content", api.lastCreateBody.Body)
	}
	if api.lastCreateBody.Owner == nil || *api.lastCreateBody.Owner != "alice" {
		t.Fatalf("owner = %v, want trimmed alice", api.lastCreateBody.Owner)
	}
	if got := api.lastCreateBody.Labels; len(got) != 3 ||
		got[0] != "bug" || got[1] != "prio-1" || got[2] != "feature" {
		t.Fatalf("labels = %v, want [bug prio-1 feature]", got)
	}
	// Whitespace-only owner must yield nil.
	if normalizeOwner("   ") != nil {
		t.Fatal("normalizeOwner of whitespace must be nil")
	}
	// Empty labels must yield nil (so omitempty drops on the wire).
	if normalizeLabels(" , , ") != nil {
		t.Fatal("normalizeLabels of all-empty must be nil")
	}
}

// TestNewIssueForm_CtrlEOnlyWhenBodyFocused: ctrl+e produces an
// editor handoff cmd only when the Body field has focus; on Title /
// Labels / Owner it is a silent no-op (and the form stays open).
func TestNewIssueForm_CtrlEOnlyWhenBodyFocused(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	// Title focused — ctrl+e is a no-op.
	m, cmd := stepModel(m, tea.KeyMsg{Type: tea.KeyCtrlE})
	if cmd != nil {
		t.Fatalf("ctrl+e on Title dispatched cmd %T; want nil (gated)", cmd)
	}
	assertInputKind(t, m, inputNewIssueForm)
	// Tab to Body — ctrl+e fires.
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
	m, cmd = stepModel(m, tea.KeyMsg{Type: tea.KeyCtrlE})
	if cmd == nil {
		t.Fatal("ctrl+e on Body produced no editor handoff cmd")
	}
	assertInputKind(t, m, inputNewIssueForm)
	// Tab through Labels, Owner, and Parent — ctrl+e is a no-op for all three.
	for _, want := range []int{2, 3, 4} {
		m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
		assertActiveField(t, m, want)
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
		if cmd != nil {
			t.Fatalf("ctrl+e on field[%d] dispatched cmd %T; want nil", want, cmd)
		}
	}
}

func TestNewIssueForm_ParentBlankOmitsLink(t *testing.T) {
	s := newNewIssueForm()
	s.fields[0].input.SetValue("childless issue")
	body, err := newIssueBodyFromForm(s.fields, "tester")
	if err != nil {
		t.Fatalf("unexpected parent error: %v", err)
	}
	if len(body.Links) != 0 {
		t.Fatalf("links = %+v, want none", body.Links)
	}
}

func TestNewIssueForm_ParentRefCreatesInitialParentLink(t *testing.T) {
	s := newNewIssueForm()
	s.fields[0].input.SetValue("child issue")
	s.fields[4].input.SetValue("#abc4")
	body, err := newIssueBodyFromForm(s.fields, "tester")
	if err != nil {
		t.Fatalf("unexpected parent error: %v", err)
	}
	if len(body.Links) != 1 {
		t.Fatalf("links len = %d, want 1: %+v", len(body.Links), body.Links)
	}
	if body.Links[0].Type != "parent" || body.Links[0].ToRef != "abc4" {
		t.Fatalf("link = %+v, want parent -> #abc4", body.Links[0])
	}
}

// TestNewIssueForm_ParentFreeFormPassesToDaemon: short_id format
// validation lives on the daemon. The form trims and strips "#"; any
// non-empty value flows through as the ToRef. The daemon's 400 is the
// canonical error path.
func TestNewIssueForm_ParentFreeFormPassesToDaemon(t *testing.T) {
	s := newNewIssueForm()
	s.fields[0].input.SetValue("any parent")
	s.fields[4].input.SetValue("parent-ish")
	body, err := newIssueBodyFromForm(s.fields, "tester")
	if err != nil {
		t.Fatalf("expected no client-side parent error; got %v", err)
	}
	if len(body.Links) != 1 || body.Links[0].ToRef != "parent-ish" {
		t.Fatalf("link = %+v, want ToRef=parent-ish", body.Links)
	}
}

func TestList_NewChild_NoSelectionNoOp(t *testing.T) {
	m := newIssueFormFixture()
	nm, cmd := stepModel(m, runeKey('N'))
	if cmd != nil {
		t.Fatalf("N with no selected row returned cmd %T, want nil", cmd)
	}
	assertInputKind(t, nm, inputNone)
}

func TestList_NewChild_PrefillsSelectedParent(t *testing.T) {
	m := newIssueFormFixture()
	m.list.issues = []Issue{{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "parent", Status: "open"}}
	m, cmd := stepModel(m, runeKey('N'))
	if cmd == nil {
		t.Fatal("N on a selected row produced no open-input command")
	}
	m, _ = stepModel(m, cmd())
	assertInputKind(t, m, inputNewIssueForm)
	if m.input.title != "new child issue" {
		t.Fatalf("title = %q, want new child issue", m.input.title)
	}
	parent := m.input.fields[4]
	if got := parent.input.Value(); got != "42aa" {
		t.Fatalf("parent field = %q, want 42aa", got)
	}
	if !parent.locked {
		t.Fatal("prefilled child parent field must start locked")
	}
}

func TestNewChildForm_ParentPrefillIgnoresEditsUntilCleared(t *testing.T) {
	s := newNewIssueFormWithParent("42aa")
	s = focusNewIssueField(s, 4)
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'9'}})
	if got := next.fields[4].input.Value(); got != "42aa" {
		t.Fatalf("locked parent accepted edit; got %q, want 42aa", got)
	}
}

func TestNewChildForm_ParentPrefillBackspaceClearsAndUnlocks(t *testing.T) {
	s := newNewIssueFormWithParent("42aa")
	s = focusNewIssueField(s, 4)
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := next.fields[4].input.Value(); got != "" {
		t.Fatalf("backspace on locked parent = %q, want empty", got)
	}
	if next.fields[4].locked {
		t.Fatal("backspace on locked parent should unlock the field")
	}
	next, _ = next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'9'}})
	if got := next.fields[4].input.Value(); got != "9" {
		t.Fatalf("unlocked parent did not accept edit; got %q, want 9", got)
	}
}

// TestNewIssueForm_StaleEditorReturnDropped: an editor return whose
// formGen mismatches the open form is silently discarded. Mirrors
// the existing single-field form's stale-return guard.
func TestNewIssueForm_StaleEditorReturnDropped(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	// Tab to Body and seed it.
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
	m.input.fields[1].area.SetValue("user-typed body")
	staleGen := m.input.formGen + 999 // any non-matching value
	nm, _ := stepModel(m, editorReturnedMsg{
		kind: "create", content: "stale editor content", formGen: staleGen,
	})
	if got := nm.input.fields[1].area.Value(); got != "user-typed body" {
		t.Fatalf("body = %q, want unchanged (stale return must not write)", got)
	}
}

// TestNewIssueForm_MutationFailureLeavesFormOpenWithErr: a failed
// form-side create leaves the form open with err set and saving
// cleared so the user can retry.
func TestNewIssueForm_MutationFailureLeavesFormOpenWithErr(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	m.input.saving = true
	nm, _ := stepModel(m, mutationDoneMsg{
		origin: "form", kind: "create", formGen: m.input.formGen,
		err: errStub("daemon 500"),
	})
	assertInputKind(t, nm, inputNewIssueForm)
	if nm.input.saving {
		t.Fatal("saving stayed true after failure; user can't retry")
	}
	if !strings.Contains(nm.input.err, "daemon 500") {
		t.Fatalf("err = %q, want it to mention daemon 500", nm.input.err)
	}
}

// TestNewIssueForm_EscDiscardsAndReturnsToList: esc closes the form
// and does NOT auto-open detail (the M3.5c-era inline-row + M4 post-
// create chain forced detail open; the multi-field form does not).
func TestNewIssueForm_EscDiscardsAndReturnsToList(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	m = typeString(m, "draft")
	nm, cmd := stepModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	assertInputKind(t, nm, inputNone)
	if nm.view != viewList {
		t.Fatalf("view = %v, want viewList (no auto-detail)", nm.view)
	}
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isOpen := msg.(openDetailMsg); isOpen {
				t.Fatal("esc on new-issue form emitted openDetailMsg; must not auto-open detail")
			}
		}
	}
}

// TestNewIssueForm_MutationSuccessRoutesToList pins the hard
// invariant: a successful new-issue mutation closes the form, seeds
// the list selection with the new issue's number, and does NOT
// auto-open detail. The success path goes through list create
// handling (lm.applyMutation), not the detail re-classification used
// by the body-edit and comment forms.
func TestNewIssueForm_MutationSuccessRoutesToList(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	m.input.saving = true
	mut := mutationDoneMsg{
		origin: "form", kind: "create", formGen: m.input.formGen,
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz"}},
	}
	nm, _ := stepModel(m, mut)
	assertInputKind(t, nm, inputNone)
	if nm.list.selectedUID != "01TEST-99zz" {
		t.Fatalf("selectedUID = %q, want 01TEST-99zz (seeded by lm.applyMutation)",
			nm.list.selectedUID)
	}
	if nm.view == viewDetail {
		t.Fatal("view = viewDetail; new-issue form must NOT auto-open detail")
	}
}

// TestSnapshot_NewIssueForm_AllFields locks in the rendered modal
// layout when every field is populated. Body field is focused so the
// footer hint advertises the unrestricted ctrl+e handoff.
func TestSnapshot_NewIssueForm_AllFields(t *testing.T) {
	defer snapshotInit(t)()
	s := newNewIssueForm()
	s.fields[0].input.SetValue("fix login bug on Safari")
	s.fields[1].area.SetValue("Reproduces in Safari 17 only.\nClick login twice.")
	s.fields[2].input.SetValue("bug, prio-1")
	s.fields[3].input.SetValue("alice")
	// Focus Body so the footer says "ctrl+e $EDITOR" (unrestricted).
	s.fields[0].blur()
	s.active = 1
	_ = s.fields[1].focus()
	got := renderCenteredForm(s, 120, 30)
	assertGolden(t, "new-issue-form-all-fields", got)
}

func TestSnapshot_NewChildForm(t *testing.T) {
	defer snapshotInit(t)()
	s := newNewIssueFormWithParent("42aa")
	s.fields[0].input.SetValue("follow-up child issue")
	s.fields[2].input.SetValue("ux")
	got := renderCenteredForm(s, 120, 30)
	assertGolden(t, "new-child-form", got)
}

// TestNewIssueForm_MutationSuccessRefreshesLabelCache pins the
// invariant that a successful form-side create routes through the
// label-cache refresh hook the same way list/detail mutations do.
//
// Bug it guards against (commit 4 follow-up I-1): routeFormMutation
// short-circuited at the top of routeMutation, so the
// mutAffectsLabelCounts → batchLabelRefresh wiring on the regular
// path was bypassed for inputNewIssueForm. Combined with the daemon
// emitting only issue.created (not issue.labeled) for create-with-
// labels, the per-project cache stayed stale until the next project
// switch / restart / unrelated label SSE event.
//
// Setup primes the cache for pid=7 with a known gen so the assertion
// can confirm dispatchLabelFetch ran (gen advanced + fetching=true).
func TestNewIssueForm_MutationSuccessRefreshesLabelCache(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	m.input.saving = true
	// Prime the label cache for pid=7 as if the user had opened the
	// `+` menu against this project once already. Without an entry
	// present, batchLabelRefresh's existence gate would skip the
	// dispatch — but the test scenario is "user already opened menu,
	// then created with labels, expects fresh counts on next open".
	primeLabelCache(&m, 7, 1)
	mut := mutationDoneMsg{
		origin: "form", kind: "create", formGen: m.input.formGen,
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz", ProjectID: 7}},
	}
	nm, _ := stepModel(m, mut)
	assertInputKind(t, nm, inputNone)
	entry := nm.projectLabels.byProject[7]
	if !entry.fetching {
		t.Fatal("label cache for pid=7 did not enter fetching=true; " +
			"form-create success must dispatch a label refresh " +
			"(commit 4 follow-up I-1)")
	}
	if entry.gen <= 1 {
		t.Fatalf("label cache gen for pid=7 = %d, want > 1 "+
			"(dispatchLabelFetch must stamp a fresh gen)", entry.gen)
	}
}

func TestNewIssueFormCreateRefetchCarriesConnGen(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	t.Cleanup(srv.Close)

	m := openNewIssueForm(t, newIssueFormFixture())
	m.api = NewClient(srv.URL, srv.Client())
	m.connGen = 4
	m.input.saving = true
	mut := mutationDoneMsg{
		connGen: 4, origin: "form", kind: "create", formGen: m.input.formGen,
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz", ProjectID: 7}},
	}

	_, cmd := stepModel(m, mut)

	if cmd == nil {
		t.Fatal("form create success returned no refetch command")
	}
	msg := cmd()
	fetched, ok := msg.(refetchedMsg)
	if !ok {
		t.Fatalf("form create refetch returned %T, want refetchedMsg", msg)
	}
	if fetched.err != nil {
		t.Fatalf("form create refetch err = %v", fetched.err)
	}
	if !called {
		t.Fatal("form create refetch did not call ListIssues")
	}
	if fetched.connGen != 4 {
		t.Fatalf("form create refetch connGen = %d, want 4", fetched.connGen)
	}
}

// TestNewIssueForm_StaleResponseFromPriorFormDropped pins the
// formGen-mismatch guard added by jobs 242/244: if the user submits
// form A (formGen=N), Esc, then opens a NEW form B (formGen=N+1)
// before form A's response returns, the stale response carrying
// formGen=N MUST be dropped without touching form B's state.
//
// Without the guard, the stale "create" response would be re-classified
// as origin=detail by routeFormMutation and the new form (form B)
// would be cleared mid-typing, or the stale response would
// trigger an unrelated batchLabelRefresh (form A's project may differ
// from form B's), or the body-edit / comment "form A" response could
// land on form B's still-open new-issue draft.
func TestNewIssueForm_StaleResponseFromPriorFormDropped(t *testing.T) {
	m := openNewIssueForm(t, newIssueFormFixture())
	staleGen := m.input.formGen
	// Type something into the original form so we have observable state.
	m = typeString(m, "draft A")
	// User presses esc, closing form A.
	m, _ = stepModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	assertInputKind(t, m, inputNone)
	// User opens form B — fresh formGen should be staleGen+1 (or larger).
	m = openNewIssueForm(t, m)
	freshGen := m.input.formGen
	if freshGen <= staleGen {
		t.Fatalf("setup: form B's formGen=%d must be > form A's formGen=%d",
			freshGen, staleGen)
	}
	// Type something into form B so we can confirm it survives untouched.
	m = typeString(m, "draft B")
	bBeforeBuf := m.input.fields[0].input.Value()
	// Prime the label cache so a hypothetical stray batchLabelRefresh
	// would visibly bump entry.gen — that lets the assertion catch the
	// "stale response sneaks through and refreshes labels for the
	// inactive project" failure mode too.
	primeLabelCache(&m, 7, 5)
	// Stale response from form A finally lands.
	stale := mutationDoneMsg{
		origin: "form", kind: "create", formGen: staleGen,
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-11bb", ShortID: "11bb", ProjectID: 7}},
	}
	nm, _ := stepModel(m, stale)
	// Form B must remain open and untouched.
	assertInputKind(t, nm, inputNewIssueForm)
	if nm.input.formGen != freshGen {
		t.Fatalf("form B formGen mutated: got %d, want %d",
			nm.input.formGen, freshGen)
	}
	if got := nm.input.fields[0].input.Value(); got != bBeforeBuf {
		t.Fatalf("form B's Title buffer mutated: got %q, want %q",
			got, bBeforeBuf)
	}
	// No label-cache refresh should have fired (stale response dropped
	// before reaching batchLabelRefresh).
	if nm.projectLabels.byProject[7].gen != 5 {
		t.Fatalf("label cache for pid=7 refreshed by stale response: "+
			"gen=%d, want 5 (unchanged)",
			nm.projectLabels.byProject[7].gen)
	}
	if nm.projectLabels.byProject[7].fetching {
		t.Fatal("label cache for pid=7 entered fetching=true on stale " +
			"response; the formGen guard must drop the message before " +
			"batchLabelRefresh runs")
	}
}

// TestNoLingeringInlineRowReferences walks internal/tui/*.go (skipping
// test files) and asserts no source contains the symbol
// `inputNewIssueRow` — guards against accidental re-introduction of
// the M3.5c inline new-issue row code path.
func TestNoLingeringInlineRowReferences(t *testing.T) {
	assertNoSourceReference(t, "inputNewIssueRow")
}

// TestNoLingeringPostCreateChain mirrors TestNoLingeringInlineRowReferences
// for `openBodyEditPostCreate` — the M4 post-create chain symbol.
func TestNoLingeringPostCreateChain(t *testing.T) {
	assertNoSourceReference(t, "openBodyEditPostCreate")
}
