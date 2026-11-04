package tui

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/tui/splitlayout"
)

// recurrenceHarness drives the Repeat form of one issue held by a real daemon.
type recurrenceHarness struct {
	*datesHarness
	api *generated.Client
}

func newRecurrenceHarness(t *testing.T, view viewID) *recurrenceHarness {
	t.Helper()
	t.Setenv("TZ", "Asia/Tokyo")
	h := &recurrenceHarness{datesHarness: newDatesHarness(t, nil, view)}
	api, err := NewClient(h.env.URL, h.env.HTTP).generatedClient()
	require.NoError(t, err)
	h.api = api
	h.m.width, h.m.height = 120, 40
	return h
}

// refreshDetail feeds the model the detail read a mutation would trigger.
func (h *recurrenceHarness) refreshDetail() {
	h.t.Helper()
	out, _ := h.m.Update(fetchIssue(h.client, h.issue.ProjectID, h.issue.UID, h.m.detail.gen)())
	h.m = out.(Model)
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

// msgsOfType runs cmd and the commands of any batch it returns, and collects
// the messages of type T: the model batches a request with unrelated
// housekeeping.
func msgsOfType[T tea.Msg](cmd tea.Cmd) []T {
	var found []T
	for _, msg := range runCmdTree(cmd) {
		if typed, ok := msg.(T); ok {
			found = append(found, typed)
		}
	}
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

func TestRecurrenceFormConflictAtMinimumHeight(t *testing.T) {
	defer snapshotInit(t)()
	s := newRecurrenceForm(formTarget{issueShortID: "42aa"}, Issue{}, &recurrenceclient.View{
		RRule: "FREQ=WEEKLY", DTStart: "2026-11-09", Timezone: "Europe/Paris",
	})
	s.err = recurrenceChangedMessage
	s.recurrence.currentAfterRefusal = "now: FREQ=DAILY from 2026-11-09 in Europe/Paris"

	for _, width := range []int{40, 60, 80} {
		t.Run(strconv.Itoa(width)+"x12", func(t *testing.T) {
			modal := renderCenteredForm(s, width, 12, snapshotFixedNow)
			assert.LessOrEqual(t, lipgloss.Height(modal), 12, "conflict recovery must fit the supported terminal height")
			assert.LessOrEqual(t, lipgloss.Width(modal), width)
			got := stripANSI(overlayModal(strings.Repeat(blankLine(width)+"\n", 11)+blankLine(width), modal, width, 12))
			assertLineCount(t, got, 12)
			assertLinesFitWidth(t, got, width)
			for _, want := range []string{"Rule", "Start", "Zone", "now: FREQ=DAILY", "ctrl+o save", "esc cancel"} {
				assert.Contains(t, got, want)
			}
			text := strings.ReplaceAll(got, "│", " ")
			assert.Contains(t, strings.Join(strings.Fields(text), " "), "Save again to overwrite.")
		})
	}
}

func TestRecurrenceSuccessKeepsTargetAfterReadFailure(t *testing.T) {
	h := newRecurrenceHarness(t, viewList)
	h.open()
	h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")
	reads := 0
	base := h.client.KataAPI.(*Client)
	base.setHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/"+h.issue.UID) {
			reads++
			if reads == 2 {
				return nil, errors.New("issue read unavailable")
			}
		}
		return h.env.HTTP.Do(r)
	})})
	h.save()
	assert.Equal(t, 2, reads, "the preflight succeeds and the follow-up read fails")
	require.Len(t, h.rows(), 1, "the recurrence was saved")
	assert.Equal(t, inputNone, h.m.input.kind)
	assert.Equal(t, "started repeat of #"+h.issue.ShortID, h.m.list.status)
}

func TestRecurrenceDetailScheduleFieldsAt80Columns(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.recurrence = &recurrenceclient.View{
		RRule: "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TU,WE,TH,FR", Timezone: "Europe/Paris",
		NextOccurrenceKey: new("2026-11-09"),
	}

	got := stripANSI(dm.View(80, 40, viewChrome{}))
	assertLinesFitWidth(t, got, 80)
	for _, want := range []string{"repeats:", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TU,WE,TH,FR", "next 2026-11-09", "Europe/Paris"} {
		assert.Contains(t, got, want)
	}
}

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
		h.patchDaemon(map[string]string{"scheduled_on": "2026-11-02", "timezone": "Europe/Paris"})
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
		_, err := recurrenceclient.Change(t.Context(), h.api, h.issue.ProjectID, current,
			recurrenceclient.Terms{Rule: "FREQ=MONTHLY", Start: "2026-11-09", Zone: "Europe/Paris", Actor: "bob"})
		require.NoError(t, err)
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
	t.Run("a failed read of the current recurrence keeps the form as it was", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)
		current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
		h.open()
		_, err := recurrenceclient.Change(t.Context(), h.api, h.issue.ProjectID, current,
			recurrenceclient.Terms{Rule: "FREQ=MONTHLY", Start: "2026-11-09", Zone: "Europe/Paris", Actor: "bob"})
		require.NoError(t, err)
		h.m.api = newConnectedUndoClient(t, NewClient(h.env.URL, &http.Client{Transport: failingRecurrenceList{inner: h.env.HTTP}}))
		h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")

		h.save()

		require.Equal(t, inputRecurrenceForm, h.m.input.kind)
		assert.NotNil(t, h.m.input.recurrence.rec, "the form keeps the recurrence it opened on")
		assert.NotContains(t, h.page(), "no recurrence")
		assert.Equal(t, "FREQ=MONTHLY", h.rows()[0].RRule, "nothing was written")
	})
	t.Run("starting on an issue another client changed", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)
		h.open()
		h.patchDaemon(map[string]string{"scheduled_on": "2026-12-01"})
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
	h.openForm()
	h.datesHarness.fill("2026-11-09", "")
	h.datesHarness.save()
	require.Len(t, h.m.undoHistory.entries, 1)
	h.open()
	h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")

	h.save()

	require.Len(t, h.rows(), 1)
	assert.Empty(t, h.m.undoHistory.entries)
	assert.Contains(t, h.m.undoHistory.boundary, "recurrence")
}

func TestRecurrenceBlankZoneNoopClosesQuietly(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	h.openForm()
	h.datesHarness.fill("2026-11-09", "")
	h.datesHarness.save()
	require.Len(t, h.m.undoHistory.entries, 1)
	h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	h.open()
	h.fill("FREQ=WEEKLY", "2026-11-09", "")
	h.save()

	assert.Equal(t, inputNone, h.m.input.kind)
	assert.Empty(t, h.m.detail.status, "the daemon reported no change")
	assert.Nil(t, h.m.toast)
	assert.Len(t, h.m.undoHistory.entries, 1, "a daemon no-op preserves undo")
}

func TestRecurrenceListBlankZoneNoopClosesQuietly(t *testing.T) {
	h := newRecurrenceHarness(t, viewList)
	before := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	h.open()
	h.m.input.field(fieldZone).setValue("")
	h.save()

	assert.Equal(t, inputNone, h.m.input.kind)
	assert.Equal(t, before.Revision, h.rows()[0].Revision)
	assert.Empty(t, h.m.list.status, "the daemon reported no change")
	assert.NotContains(t, h.page(), "changed repeat")
}

func TestRecurrenceFormRefusalExpires(t *testing.T) {
	for _, refusal := range []string{"stopped series", "cron parent", "failed read"} {
		t.Run(refusal, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewDetail)
			var want string
			switch refusal {
			case "stopped series":
				current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
				require.NoError(t, recurrenceclient.Stop(t.Context(), h.api, h.issue.ProjectID, current, "bob"))
				want = recurrenceStoppedMessage
			case "cron parent":
				_, err := h.env.DB.ExecContext(t.Context(),
					`UPDATE issues SET metadata='{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{}}' WHERE id=?`, h.issue.ID)
				require.NoError(t, err)
				want = recurrenceclient.CronParentRefusal
			case "failed read":
				h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
				h.m.api = newConnectedUndoClient(t, NewClient(h.env.URL, &http.Client{Transport: failingRecurrenceList{inner: h.env.HTTP}}))
				want = "Could not read the recurrence:"
			}
			loaded := msgOfType[recurrenceFormLoadedMsg](t, h.key(runeKey('O')))
			synctest.Test(t, func(t *testing.T) {
				h.m.toastNow = time.Now
				out, expiry := h.m.Update(loaded)
				h.m = out.(Model)
				require.Equal(t, inputNone, h.m.input.kind)
				require.Contains(t, h.page(), want)
				require.NotNil(t, expiry, "the refusal schedules its own expiry")

				msg := msgOfType[toastExpiredMsg](t, expiry)
				out, _ = h.m.Update(msg)
				h.m = out.(Model)
				assert.NotContains(t, h.page(), want)
				assert.Nil(t, h.m.toast)
			})
		})
	}
}

func TestRecurrencePreflightFailurePreservesUndo(t *testing.T) {
	for _, failure := range []string{"server", "transport", "write transport"} {
		t.Run(failure, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewDetail)
			h.openForm()
			h.datesHarness.fill("2026-11-09", "")
			h.datesHarness.save()
			require.Len(t, h.m.undoHistory.entries, 1)
			h.open()
			h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")
			writes := 0
			base := h.client.KataAPI.(*Client)
			base.setHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					writes++
					return nil, errors.New("write transport failed")
				}
				if strings.Contains(r.URL.Path, "/issues/") && failure != "write transport" {
					if failure == "transport" {
						return nil, errors.New("preflight transport failed")
					}
					status, body := http.StatusInternalServerError, `{"error":{"code":"internal","message":"preflight failed"}}`
					return &http.Response{StatusCode: status, Request: r, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				return h.env.HTTP.Do(r)
			})})
			h.save()
			assert.Equal(t, inputRecurrenceForm, h.m.input.kind)
			assert.NotEmpty(t, h.m.input.err)
			if failure == "write transport" {
				assert.Equal(t, 1, writes)
				assert.Empty(t, h.m.undoHistory.entries, "a sent write still has an uncertain outcome")
			} else {
				assert.Zero(t, writes)
				assert.Len(t, h.m.undoHistory.entries, 1, "a failed read preserves the earlier edit")
			}
		})
	}
}

func (h *recurrenceHarness) assertHistoricalRecurrenceSnapshot(oldProject int64, oldRef string) {
	h.t.Helper()
	snapshot, err := readRecurrenceSnapshot(h.t.Context(), h.client, formTarget{projectID: oldProject, issueUID: oldRef})
	require.NoError(h.t, err)
	assert.NotNil(h.t, snapshot.rec, "form reads use the resolved issue's project")
}

func TestRecurrenceFormRefusesStoppedSeries(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	require.NoError(t, recurrenceclient.Stop(t.Context(), h.api, h.issue.ProjectID, current, "bob"))

	cmd := h.key(runeKey('O'))
	out, _ := h.m.Update(msgOfType[recurrenceFormLoadedMsg](t, cmd))
	h.m = out.(Model)

	assert.Equal(t, inputNone, h.m.input.kind)
	require.NotNil(t, h.m.toast)
	assert.Contains(t, h.m.toast.text, "This issue's recurrence was stopped; a stopped series can't start again")
}

func TestRecurrenceFormRequiresDateOnlyStart(t *testing.T) {
	for _, typed := range []string{"", "2026-11-09T09:00", "next Tuesday"} {
		t.Run(typed, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewDetail)
			h.open()
			h.fill("FREQ=DAILY", typed, "Europe/Paris")

			cmd := h.key(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})

			assert.Empty(t, msgsOfType[mutationDoneMsg](cmd), "invalid starts are refused before writing")
			if strings.TrimSpace(typed) == "" {
				assert.Equal(t, "Start is required.", h.m.input.err)
			} else {
				assert.Equal(t, "Start must be YYYY-MM-DD, today, tomorrow, +Nd or +Nw.", h.m.input.err)
			}
			assert.Equal(t, typed, h.m.input.fieldValue(fieldStart))
			assert.Empty(t, h.rows())
		})
	}
}

func TestRecurrenceFormResolvesDateOnlyStart(t *testing.T) {
	for _, tc := range []struct{ typed, want string }{
		{"+2w", "2026-11-16"},
	} {
		t.Run(tc.typed, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewDetail)
			h.open()
			h.fill("FREQ=DAILY", tc.typed, "Europe/Paris")
			h.save()
			require.Len(t, h.rows(), 1)
			assert.Equal(t, tc.want, h.rows()[0].DTStart)
		})
	}
}

func TestRecurrenceKeyUsesTheSamePermissionGateAsOtherEdits(t *testing.T) {
	h := newRecurrenceHarness(t, viewList)
	h.m.issueScoped, h.m.scopedWritable = true, false

	cmd := h.key(runeKey('O'))

	assert.Empty(t, msgsOfType[recurrenceFormLoadedMsg](cmd), "nothing is read for a key that may not edit")
	assert.Equal(t, inputNone, h.m.input.kind)
}

func TestRecurrenceFormDropsLoadAfterNavigation(t *testing.T) {
	for _, moved := range []string{"project", "UID", "view", "detail generation"} {
		t.Run(moved, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewList)
			h.m.scope.allProjects = true
			if moved == "detail generation" {
				h.m.view = viewDetail
			}
			loaded := msgOfType[recurrenceFormLoadedMsg](t, h.key(runeKey('O')))
			switch moved {
			case "project":
				h.m.list.issues[0].ProjectID++
			case "UID":
				h.m.list.issues[0].UID = "another-issue"
			case "view":
				h.m.view = viewDetail
			case "detail generation":
				h.m.detail.gen++
			}
			out, _ := h.m.Update(loaded)
			assert.Equal(t, inputNone, out.(Model).input.kind)
		})
	}
}

func TestRecurrenceFormRequiresGuardedStartAPI(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	posts := 0
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/health" {
			return &http.Response{StatusCode: http.StatusOK, Request: r,
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"ok":true,"api_schema_version":"0.36.0"}`))}, nil
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/recurrences") {
			posts++
		}
		return h.env.HTTP.Do(r)
	})}
	h.m.api = newConnectedUndoClient(t, NewClient(h.env.URL, hc))
	h.open()
	h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")
	h.save()
	assert.Zero(t, posts, "an older daemon cannot enforce a guarded start")
	assert.Equal(t, inputRecurrenceForm, h.m.input.kind)
	assert.Contains(t, h.m.input.err, "upgrade the daemon")
	assert.Empty(t, h.rows())
}

func TestRecurrenceFormReloadsAfterAnotherClientStops(t *testing.T) {
	for _, action := range []string{"change", "stop"} {
		t.Run(action, func(t *testing.T) {
			h := newRecurrenceHarness(t, viewDetail)
			current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
			h.open()
			h.fill("FREQ=DAILY", "2026-11-10", "Asia/Tokyo")
			require.NoError(t, recurrenceclient.Stop(t.Context(), h.api, h.issue.ProjectID, current, "bob"))
			if action == "stop" {
				h.key(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
				h.send(h.key(runeKey('y')))
			} else {
				h.save()
			}
			require.Equal(t, inputRecurrenceForm, h.m.input.kind)
			assert.Nil(t, h.m.input.recurrence.rec)
			assert.Contains(t, h.m.input.err, "This issue's recurrence was stopped; a stopped series can't start again")
			assert.Contains(t, h.page(), "This issue's recurrence was stopped;")
			assert.NotContains(t, h.page(), "save again to write your input over it")
			cmd := h.key(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
			assert.Empty(t, msgsOfType[mutationDoneMsg](cmd), "saving cannot restart a stopped series")
			rule, start, zone := h.values()
			assert.Equal(t, []string{"FREQ=DAILY", "2026-11-10", "Asia/Tokyo"}, []string{rule, start, zone})
			assert.Empty(t, h.rows(), "the refused write leaves the recurrence stopped")
		})
	}
}

func TestRecurrenceMutationsRefreshSplitDetail(t *testing.T) {
	for _, source := range []string{"local", "incoming"} {
		t.Run(source+"/stop", func(t *testing.T) {
			h := newRecurrenceHarness(t, viewList)
			current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
			h.m.layout = splitlayout.Split
			h.m.focus = focusList
			h.refreshDetail()
			require.Contains(t, h.page(), "repeats: FREQ=WEEKLY")
			var cmd tea.Cmd
			if source == "local" {
				h.open()
				h.key(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
				cmd = h.key(runeKey('y'))
				out, refresh := h.m.Update(msgOfType[mutationDoneMsg](t, cmd))
				h.m, cmd = out.(Model), refresh
			} else {
				require.NoError(t, recurrenceclient.Stop(t.Context(), h.api, h.issue.ProjectID, current, "bob"))
				event := decodeEventReceived(frame{data: []byte(`{"type":"recurrence.deleted","project_id":` + strconv.FormatInt(h.issue.ProjectID, 10) + `,"payload":{"recurrence_uid":"` + current.UID + `"}}`)})
				cmd = h.m.maybeRefetchOpenDetail(event)
			}
			for _, msg := range runCmdTree(cmd) {
				out, next := h.m.Update(msg)
				h.m = out.(Model)
				for _, fetched := range runCmdTree(next) {
					out, _ = h.m.Update(fetched)
					h.m = out.(Model)
				}
			}
			assert.NotContains(t, h.page(), "repeats:")
		})
	}
}
