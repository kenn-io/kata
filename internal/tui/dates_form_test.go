package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

var datesNow = time.Date(2026, time.November, 2, 9, 30, 0, 0, time.UTC)

// datesHarness drives the Dates form of one issue held by a real daemon.
type datesHarness struct {
	t      *testing.T
	env    *testenv.Env
	client *undoClient
	issue  db.Issue
	m      Model
}

func newDatesHarness(t *testing.T, stored map[string]string, view viewID) *datesHarness {
	t.Helper()
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example", Body: "body", Author: "alice",
	})
	require.NoError(t, err)
	h := &datesHarness{t: t, env: env, issue: issue, client: newConnectedUndoClient(t, NewClient(env.URL, env.HTTP))}
	if len(stored) > 0 {
		h.patchDaemon(stored)
	}
	h.m = initialModel(Options{})
	h.m.api = h.client
	h.m.toastNow = func() time.Time { return datesNow }
	h.m.list.actor = "alice"
	h.m.scope = scope{projectID: project.ID}
	h.m.view = view
	h.reload()
	return h
}

// patchDaemon writes metadata behind the TUI's back, as another client would.
func (h *datesHarness) patchDaemon(values map[string]string) {
	h.t.Helper()
	patch := map[string]jsontext.Value{}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		require.NoError(h.t, err)
		patch[key] = encoded
	}
	_, err := h.env.DB.PatchIssueMetadata(h.t.Context(), db.PatchIssueMetadataIn{
		IssueID: h.issue.ID, Actor: "bob", Patch: patch,
	})
	require.NoError(h.t, err)
}

// reload shows the daemon's current issue in the model's list or detail.
func (h *datesHarness) reload() {
	h.t.Helper()
	detail, err := h.client.GetIssueDetail(h.t.Context(), h.issue.ProjectID, h.issue.UID)
	require.NoError(h.t, err)
	h.m.list.issues = []Issue{*detail.Issue}
	h.m.detail = detailModel{scopePID: h.issue.ProjectID, issue: detail.Issue}
}

func (h *datesHarness) stored() map[string]string {
	h.t.Helper()
	current, err := h.env.DB.IssueByUID(h.t.Context(), h.issue.UID, db.IncludeDeletedNo)
	require.NoError(h.t, err)
	out := map[string]string{}
	require.NoError(h.t, json.Unmarshal([]byte(current.Metadata), &out))
	return out
}

func (h *datesHarness) revision() int64 {
	h.t.Helper()
	current, err := h.env.DB.IssueByUID(h.t.Context(), h.issue.UID, db.IncludeDeletedNo)
	require.NoError(h.t, err)
	return current.Revision
}

func (h *datesHarness) key(msg tea.KeyPressMsg) tea.Cmd {
	out, cmd := h.m.Update(msg)
	h.m = out.(Model)
	return cmd
}

func (h *datesHarness) openForm() {
	h.t.Helper()
	h.key(runeKey('d'))
	require.Equal(h.t, inputDatesForm, h.m.input.kind, "d opens the Dates form")
}

func (h *datesHarness) fill(scheduled, deadline string) {
	h.m.input.field(fieldScheduled).setValue(scheduled)
	h.m.input.field(fieldDeadline).setValue(deadline)
}

// save presses ctrl+o and feeds the daemon's answer back to the model.
func (h *datesHarness) save() tea.Cmd {
	h.t.Helper()
	cmd := h.key(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if cmd == nil {
		return nil
	}
	done, ok := cmd().(mutationDoneMsg)
	require.True(h.t, ok, "saving sends one request")
	out, follow := h.m.Update(done)
	h.m = out.(Model)
	return follow
}

func (h *datesHarness) undo() {
	h.t.Helper()
	cmd := h.key(runeKey('u'))
	require.NotNil(h.t, cmd)
	out, _ := h.m.Update(cmd())
	h.m = out.(Model)
}

func TestDatesFormWritesRelativeAndLocalTimeForms(t *testing.T) {
	for _, tt := range []struct{ name, typed, want string }{
		{"local minute", "2026-11-15T09:00", "2026-11-15T09:00"},
		{"weeks", "+2w", "2026-11-16"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newDatesHarness(t, nil, viewDetail)
			before := h.revision()
			h.openForm()
			h.fill(tt.typed, "")
			h.save()

			assert.Equal(t, inputNone, h.m.input.kind, "a saved form closes")
			assert.Equal(t, map[string]string{"scheduled_on": tt.want}, h.stored())
			assert.Equal(t, before+1, h.revision())
		})
	}
}

func TestDatesFormOpensFromListAndDetail(t *testing.T) {
	for _, view := range []viewID{viewList, viewDetail} {
		h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02", "deadline_on": "2026-11-20T17:00"}, view)
		h.openForm()
		assert.Equal(t, "2026-11-02", h.m.input.fieldValue(fieldScheduled))
		assert.Equal(t, "2026-11-20T17:00", h.m.input.fieldValue(fieldDeadline), "prefilled with the stored value, not a projection")
	}
}

func TestDatesFormWritesOnlyChangedKeysInOneRequest(t *testing.T) {
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-09-01T09:00", "deadline_on": "2026-11-20"}, viewList)
	before := h.revision()
	h.openForm()
	h.fill("2026-09-01T09:00", "+1w")
	h.save()

	assert.Equal(t, map[string]string{"scheduled_on": "2026-09-01T09:00", "deadline_on": "2026-11-09"}, h.stored(),
		"the untouched Scheduled keeps its time")
	assert.Equal(t, before+1, h.revision(), "both keys travel in one patch")
}

func TestDatesFormBlankClears(t *testing.T) {
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02", "deadline_on": "2026-11-20"}, viewDetail)
	before := h.revision()
	h.openForm()
	h.fill("", "  ")
	h.save()

	assert.Empty(t, h.stored())
	assert.Equal(t, before+1, h.revision())
}

func TestDatesFormClearsOneKeyAndKeepsTheOther(t *testing.T) {
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02", "deadline_on": "2026-11-20"}, viewDetail)
	h.openForm()
	h.fill("", "2026-11-20")
	h.save()

	assert.Equal(t, map[string]string{"deadline_on": "2026-11-20"}, h.stored())
}

func TestDatesFormSavingNothingWritesNothing(t *testing.T) {
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02"}, viewDetail)
	before := h.revision()
	h.openForm()
	h.fill("2026-11-02", "")
	cmd := h.key(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})

	assert.Nil(t, cmd)
	assert.Equal(t, inputNone, h.m.input.kind)
	assert.Equal(t, before, h.revision())
}

func TestDatesFormRefusesTextThatIsNotADate(t *testing.T) {
	for _, typed := range []string{"monday", "+3m", "2026-13-01", "next week"} {
		t.Run(typed, func(t *testing.T) {
			h := newDatesHarness(t, nil, viewDetail)
			before := h.revision()
			h.openForm()
			h.fill("", typed)
			cmd := h.key(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})

			assert.Nil(t, cmd, "nothing is sent")
			assert.Equal(t, inputDatesForm, h.m.input.kind, "the form stays open")
			assert.Equal(t, "Deadline must be "+dateInputHint+".", h.m.input.err)
			assert.Equal(t, before, h.revision())
			assert.Empty(t, h.stored())
		})
	}
}

func TestDatesFormReloadsWhenTheIssueChangedMeanwhile(t *testing.T) {
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02"}, viewDetail)
	h.openForm()
	h.patchDaemon(map[string]string{"deadline_on": "2026-12-01"})
	moved := h.revision()

	h.fill("2026-11-09", "")
	h.save()

	require.Equal(t, inputDatesForm, h.m.input.kind, "the form stays open")
	assert.False(t, h.m.input.saving)
	assert.Equal(t, datesChangedMessage, h.m.input.err)
	assert.Equal(t, "2026-11-02", h.m.input.fieldValue(fieldScheduled), "shows what the daemon holds now")
	assert.Equal(t, "2026-12-01", h.m.input.fieldValue(fieldDeadline))
	assert.Equal(t, map[string]string{"scheduled_on": "2026-11-02", "deadline_on": "2026-12-01"}, h.stored(), "nothing was overwritten")
	assert.Equal(t, moved, h.revision())
	assert.Equal(t, "2026-12-01", h.m.list.issues[0].deadlineOn(), "the list row shows the current dates")
	assert.Equal(t, "2026-12-01", h.m.detail.issue.deadlineOn(), "so does the open detail")
	assert.Equal(t, moved, h.m.detail.issue.Revision)

	h.fill("2026-11-09", "2026-12-01")
	h.save()

	assert.Equal(t, inputNone, h.m.input.kind)
	assert.Equal(t, map[string]string{"scheduled_on": "2026-11-09", "deadline_on": "2026-12-01"}, h.stored(),
		"the next save is guarded on the revision just read")
}

func TestDatesUndoWritesThePreviousDatesBack(t *testing.T) {
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02", "deadline_on": "2026-11-20"}, viewDetail)
	h.openForm()
	h.fill("tomorrow", "")
	h.save()
	require.Equal(t, map[string]string{"scheduled_on": "2026-11-03"}, h.stored())
	changed := h.revision()

	h.undo()

	assert.Equal(t, map[string]string{"scheduled_on": "2026-11-02", "deadline_on": "2026-11-20"}, h.stored())
	assert.Equal(t, changed+1, h.revision(), "one patch")
	assert.Empty(t, h.m.undoHistory.entries)
}

func TestDatesUndoRestoresDatesSetOnAnIssueWithNone(t *testing.T) {
	h := newDatesHarness(t, nil, viewList)
	h.openForm()
	h.fill("today", "+2w")
	h.save()
	require.Len(t, h.stored(), 2)

	h.undo()

	assert.Empty(t, h.stored())
}

func TestDatesUndoRefusesWhenTheIssueMovedOn(t *testing.T) {
	for _, tt := range []struct {
		name  string
		other map[string]string
	}{
		{"the dates changed", map[string]string{"scheduled_on": "2026-12-25"}},
		{"another key changed the revision", map[string]string{"example_note": "x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02"}, viewDetail)
			h.openForm()
			h.fill("2026-11-09", "")
			h.save()
			h.patchDaemon(tt.other)
			want := h.stored()

			h.undo()

			assert.Equal(t, want, h.stored(), "the daemon is left as it is")
			require.NotNil(t, h.m.toast)
			assert.Contains(t, h.m.toast.text, "issue dates changed since the action")
		})
	}
}

func TestDatesFormShowsWhatItWillWrite(t *testing.T) {
	m := initialModel(Options{})
	m.toastNow = func() time.Time { return datesNow }
	m.width, m.height = 120, 30
	m.view = viewList
	m.scope = scope{projectID: 7}
	m.list.issues = []Issue{{ProjectID: 7, ShortID: "abc4", Status: "open", Revision: 1}}
	out, _ := m.Update(runeKey('d'))
	m = out.(Model)
	m.input.field(fieldScheduled).setValue("+3d")
	m.input.field(fieldDeadline).setValue("monday")

	view := stripANSI(m.viewContent())

	assert.Contains(t, view, "2026-11-05 Thu")
	assert.NotContains(t, view, "monday 2026", "text that would be refused has no preview")
}

func TestDetailStripShowsBothDates(t *testing.T) {
	render := func(iss Issue) string {
		return stripANSI(strings.Join(renderDocumentMetadata(100, iss, nil, nil, scope{}, uidDisplayNone), "\n"))
	}
	withDates := Issue{Metadata: issueMetadata{
		ScheduledOn: jsontext.Value(`"2026-11-02"`), DeadlineOn: jsontext.Value(`"2026-11-20T17:00"`),
	}}
	got := render(withDates)
	assert.Contains(t, got, "scheduled: 2026-11-02")
	assert.Contains(t, got, "deadline: 2026-11-20T17:00")

	onlyDeadline := Issue{Metadata: issueMetadata{DeadlineOn: jsontext.Value(`"2026-11-20"`)}}
	got = render(onlyDeadline)
	assert.Contains(t, got, "scheduled: none")
	assert.Contains(t, got, "deadline: 2026-11-20")

	got = render(Issue{})
	assert.NotContains(t, got, "scheduled:")
	assert.NotContains(t, got, "deadline:")
}
