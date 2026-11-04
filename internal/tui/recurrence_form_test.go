package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/client/generated"
)

// recurrenceHarness drives the Repeat form of one issue held by a real daemon.
type recurrenceHarness struct {
	t      *testing.T
	env    *testenv.Env
	client *undoClient
	api    *generated.Client
	issue  db.Issue
	m      Model
}

func newRecurrenceHarness(t *testing.T, view viewID) *recurrenceHarness {
	t.Helper()
	t.Setenv("TZ", "Asia/Tokyo")
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example", Body: "body", Author: "alice",
	})
	require.NoError(t, err)
	base := NewClient(env.URL, env.HTTP)
	api, err := base.generatedClient()
	require.NoError(t, err)
	h := &recurrenceHarness{t: t, env: env, api: api, issue: issue, client: newConnectedUndoClient(t, base)}
	h.m = initialModel(Options{})
	h.m.api = h.client
	h.m.toastNow = func() time.Time { return datesNow }
	h.m.list.actor = "alice"
	h.m.scope = scope{projectID: project.ID}
	h.m.view = view
	h.m.width, h.m.height = 120, 40
	h.reload()
	return h
}

// reload shows the daemon's current issue and its recurrence in the model.
func (h *recurrenceHarness) reload() {
	h.t.Helper()
	detail, err := h.client.GetIssueDetail(h.t.Context(), h.issue.ProjectID, h.issue.UID)
	require.NoError(h.t, err)
	h.m.list.issues = []Issue{*detail.Issue}
	h.m.detail = detailModel{scopePID: h.issue.ProjectID, issue: detail.Issue}
}

// refreshDetail feeds the model the detail read a mutation would trigger.
func (h *recurrenceHarness) refreshDetail() {
	h.t.Helper()
	out, _ := h.m.Update(fetchIssue(h.client, h.issue.ProjectID, h.issue.UID, h.m.detail.gen)())
	h.m = out.(Model)
}

func (h *recurrenceHarness) patchMetadata(values map[string]string) {
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

// rows lists the project's live recurrences as the daemon holds them.
func (h *recurrenceHarness) rows() []recurrenceclient.View {
	h.t.Helper()
	recs, err := recurrenceclient.List(h.t.Context(), h.api, h.issue.ProjectID)
	require.NoError(h.t, err)
	return recs
}

// startBehindTheTUI starts a recurrence from the issue as another client would.
func (h *recurrenceHarness) startBehindTheTUI(rule, start, zone string) recurrenceclient.View {
	h.t.Helper()
	terms := recurrenceclient.Terms{Rule: rule, Start: start, Zone: zone, Actor: "bob"}
	require.NoError(h.t, recurrenceclient.Start(h.t.Context(), h.api, h.issue.ProjectID, h.issue.UID,
		recurrenceclient.Template{Title: "Example", Body: "body"}, terms, nil))
	h.reload()
	return h.rows()[0]
}

func (h *recurrenceHarness) key(msg tea.KeyPressMsg) tea.Cmd {
	out, cmd := h.m.Update(msg)
	h.m = out.(Model)
	return cmd
}

// msgsOfType runs cmd and the commands of any batch it returns, and collects
// the messages of type T: the model batches a request with unrelated
// housekeeping.
func msgsOfType[T tea.Msg](cmd tea.Cmd) []T {
	var found []T
	var visit func(tea.Cmd)
	visit = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch msg := c().(type) {
		case T:
			found = append(found, msg)
		case tea.BatchMsg:
			for _, sub := range msg {
				visit(sub)
			}
		}
	}
	visit(cmd)
	return found
}

func msgOfType[T tea.Msg](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	found := msgsOfType[T](cmd)
	require.Len(t, found, 1, "the key sends exactly one request")
	return found[0]
}

func (h *recurrenceHarness) open() {
	h.t.Helper()
	cmd := h.key(runeKey('O'))
	require.NotNil(h.t, cmd, "O reads the recurrence before opening the form")
	out, _ := h.m.Update(msgOfType[recurrenceFormLoadedMsg](h.t, cmd))
	h.m = out.(Model)
	require.Equal(h.t, inputRecurrenceForm, h.m.input.kind, "O opens the Repeat form")
}

func (h *recurrenceHarness) fill(rule, start, zone string) {
	h.m.input.field(fieldRule).setValue(rule)
	h.m.input.field(fieldStart).setValue(start)
	h.m.input.field(fieldZone).setValue(zone)
}

func (h *recurrenceHarness) values() (rule, start, zone string) {
	return h.m.input.fieldValue(fieldRule), h.m.input.fieldValue(fieldStart), h.m.input.fieldValue(fieldZone)
}

// send runs the one request cmd stands for and feeds the answer to the model.
func (h *recurrenceHarness) send(cmd tea.Cmd) {
	h.t.Helper()
	require.NotNil(h.t, cmd)
	out, _ := h.m.Update(msgOfType[mutationDoneMsg](h.t, cmd))
	h.m = out.(Model)
}

func (h *recurrenceHarness) save() {
	h.t.Helper()
	h.send(h.key(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}))
}

func (h *recurrenceHarness) page() string { return stripANSI(h.m.viewContent()) }

// failingRecurrenceList fails the recurrence list and passes everything else
// to the daemon.
type failingRecurrenceList struct{ inner *http.Client }

func (f failingRecurrenceList) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/recurrences") {
		return nil, errors.New("recurrence list unavailable")
	}
	return f.inner.Do(req)
}

func TestRecurrenceFormShowsTheReadErrorForARecurringIssue(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	h.m.api = newConnectedUndoClient(t, NewClient(h.env.URL, &http.Client{Transport: failingRecurrenceList{inner: h.env.HTTP}}))

	cmd := h.key(runeKey('O'))
	out, _ := h.m.Update(msgOfType[recurrenceFormLoadedMsg](t, cmd))
	h.m = out.(Model)

	assert.Equal(t, inputNone, h.m.input.kind, "a recurring issue never opens as a new recurrence")
	require.NotNil(t, h.m.toast)
	assert.Contains(t, h.m.toast.text, "recurrence list unavailable")
}

func TestRecurrenceFormOpensPrefilledFromListAndDetail(t *testing.T) {
	t.Run("new recurrence uses the issue's date and zone", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)
		h.patchMetadata(map[string]string{"scheduled_on": "2026-11-02", "timezone": "Europe/Paris"})
		h.reload()

		h.open()

		rule, start, zone := h.values()
		assert.Empty(t, rule)
		assert.Equal(t, "2026-11-02", start)
		assert.Equal(t, "Europe/Paris", zone)
	})
	t.Run("zone falls back to the caller's own IANA zone", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)

		h.open()

		_, _, zone := h.values()
		assert.Equal(t, "Asia/Tokyo", zone, "never Local")
	})
	for _, from := range []struct {
		name string
		view viewID
	}{{"list", viewList}, {"detail", viewDetail}} {
		t.Run(from.name+" existing recurrence shows its rule, start and zone", func(t *testing.T) {
			h := newRecurrenceHarness(t, from.view)
			h.startBehindTheTUI("FREQ=WEEKLY;BYDAY=MO", "2026-11-09", "Europe/Paris")

			h.open()

			rule, start, zone := h.values()
			assert.Equal(t, "FREQ=WEEKLY;BYDAY=MO", rule)
			assert.Equal(t, "2026-11-09", start)
			assert.Equal(t, "Europe/Paris", zone)
		})
	}
}

func TestRecurrenceFormStartsChangesAndStopsARecurrence(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)

	h.open()
	h.fill("FREQ=WEEKLY;BYDAY=MO", "2026-11-09", "Europe/Paris")
	h.save()
	assert.Equal(t, "started repeat of #"+h.issue.ShortID, h.m.detail.status)

	require.Equal(t, inputNone, h.m.input.kind, "a saved form closes")
	recs := h.rows()
	require.Len(t, recs, 1)
	assert.Equal(t, "FREQ=WEEKLY;BYDAY=MO", recs[0].RRule, "the rule is sent as written")
	assert.Equal(t, "2026-11-09", recs[0].DTStart)
	assert.Equal(t, "Europe/Paris", recs[0].Timezone)
	h.refreshDetail()
	page := h.page()
	assert.Contains(t, page, "repeats: FREQ=WEEKLY;BYDAY=MO")
	assert.Contains(t, page, "next 2026-11-16")
	assert.Contains(t, page, "Europe/Paris")

	h.open()
	rule, _, _ := h.values()
	require.Equal(t, "FREQ=WEEKLY;BYDAY=MO", rule)
	h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")
	h.save()
	assert.Equal(t, "changed repeat of #"+h.issue.ShortID, h.m.detail.status)

	changed := h.rows()
	require.Len(t, changed, 1)
	assert.Equal(t, recs[0].UID, changed[0].UID, "a change keeps the recurrence")
	assert.Equal(t, "FREQ=DAILY", changed[0].RRule)
	h.refreshDetail()
	assert.Contains(t, h.page(), "repeats: FREQ=DAILY")

	h.open()
	h.key(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	require.Equal(t, modalStopRecurrence, h.m.modal, "stopping asks first")
	h.key(runeKey('n'))
	assert.Equal(t, modalNone, h.m.modal)
	assert.Equal(t, inputRecurrenceForm, h.m.input.kind, "declining keeps the form")
	require.Len(t, h.rows(), 1, "declining writes nothing")

	h.key(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	h.send(h.key(runeKey('y')))
	assert.Equal(t, "stopped repeat of #"+h.issue.ShortID, h.m.detail.status)

	assert.Empty(t, h.rows())
	assert.Equal(t, inputNone, h.m.input.kind)
	h.refreshDetail()
	assert.NotContains(t, h.page(), "repeats:")
}

func TestRecurrenceFormRefusesAndKeepsTheInput(t *testing.T) {
	cronParent := func(h *recurrenceHarness) {
		_, err := h.env.DB.ExecContext(t.Context(),
			`UPDATE issues SET metadata='{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{}}' WHERE id=?`, h.issue.ID)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name       string
		setup      func(*recurrenceHarness)
		rule, zone string
		wantErr    []string
	}{
		{"the daemon's refusal is shown as is", nil, "every other tuesday", "Europe/Paris", []string{"every other tuesday"}},
		{"Local is never sent as a zone", nil, "FREQ=DAILY", "Local", []string{"Local"}},
		{"an issue that became a cron parent after the form opened is refused", cronParent, "FREQ=DAILY", "Europe/Paris",
			[]string{recurrenceclient.CronParentRefusal, "kata cron job create --parent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewDetail)
			h.open()
			if tc.setup != nil {
				tc.setup(h)
			}
			h.fill(tc.rule, "2026-11-09", tc.zone)

			h.save()

			assert.Equal(t, inputRecurrenceForm, h.m.input.kind)
			assert.False(t, h.m.input.saving, "the form can be saved again")
			for _, want := range tc.wantErr {
				assert.Contains(t, h.m.input.err, want)
			}
			rule, start, zone := h.values()
			assert.Equal(t, []string{tc.rule, "2026-11-09", tc.zone}, []string{rule, start, zone}, "the input stays")
			assert.Empty(t, h.rows())
		})
	}
}

func TestRecurrenceKeyRefusesTheParentOfACronJobBeforeOpeningTheForm(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	_, err := h.env.DB.ExecContext(t.Context(),
		`UPDATE issues SET metadata='{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{}}' WHERE id=?`, h.issue.ID)
	require.NoError(t, err)

	cmd := h.key(runeKey('O'))
	out, _ := h.m.Update(msgOfType[recurrenceFormLoadedMsg](t, cmd))
	h.m = out.(Model)

	assert.Equal(t, inputNone, h.m.input.kind, "no form opens for a cron parent")
	require.NotNil(t, h.m.toast)
	assert.Contains(t, h.m.toast.text, recurrenceclient.CronParentRefusal)
	assert.Contains(t, h.m.toast.text, "kata cron job create --parent")
}

func TestRecurrenceFormConflictKeepsInputAndShowsTheCurrentRecurrence(t *testing.T) {
	t.Run("changing a recurrence another client changed", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)
		current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
		h.open()
		require.NoError(t, recurrenceclient.Change(t.Context(), h.api, h.issue.ProjectID, current,
			recurrenceclient.Terms{Rule: "FREQ=MONTHLY", Start: "2026-11-09", Zone: "Europe/Paris", Actor: "bob"}))
		h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")

		h.save()

		require.Equal(t, inputRecurrenceForm, h.m.input.kind)
		rule, _, _ := h.values()
		assert.Equal(t, "FREQ=DAILY", rule, "the typed rule stays")
		assert.Contains(t, h.m.input.err, "changed since the form opened")
		assert.Contains(t, h.page(), "FREQ=MONTHLY", "the current recurrence is shown")
		assert.Equal(t, "FREQ=MONTHLY", h.rows()[0].RRule, "nothing was written")

		h.save()

		assert.Equal(t, inputNone, h.m.input.kind, "saving again is guarded on the revision just shown")
		assert.Equal(t, "FREQ=DAILY", h.rows()[0].RRule)
	})
	t.Run("starting on an issue another client changed", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)
		h.open()
		h.patchMetadata(map[string]string{"scheduled_on": "2026-12-01"})
		h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")

		h.save()

		require.Equal(t, inputRecurrenceForm, h.m.input.kind)
		rule, _, _ := h.values()
		assert.Equal(t, "FREQ=DAILY", rule)
		assert.Contains(t, h.m.input.err, "changed since the form opened")
		assert.Empty(t, h.rows(), "nothing was written")

		h.save()

		assert.Equal(t, inputNone, h.m.input.kind)
		assert.Len(t, h.rows(), 1)
	})
}

func TestRecurrenceWriteClearsUndoHistory(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	h.m.undoHistory.push(undoEntry{kind: "dates", uid: h.issue.UID, projectID: h.issue.ProjectID})
	h.open()
	h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")

	h.save()

	require.Len(t, h.rows(), 1)
	assert.Empty(t, h.m.undoHistory.entries)
	assert.Contains(t, h.m.undoHistory.boundary, "recurrence")
}

func TestRecurrenceKeyUsesTheSamePermissionGateAsOtherEdits(t *testing.T) {
	h := newRecurrenceHarness(t, viewList)
	h.m.issueScoped, h.m.scopedWritable = true, false

	cmd := h.key(runeKey('O'))

	assert.Empty(t, msgsOfType[recurrenceFormLoadedMsg](cmd), "nothing is read for a key that may not edit")
	assert.Equal(t, inputNone, h.m.input.kind)
}
