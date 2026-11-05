package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kata/internal/testenv"
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
	h.refreshDetail()
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
	h.key(runeKey('O'))
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

func TestRecurrenceFormOpensPrefilledFromDetail(t *testing.T) {
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
	t.Run("existing recurrence shows its rule, start and zone", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewDetail)
		h.startBehindTheTUI("FREQ=WEEKLY;BYDAY=MO", "2026-11-09", "Europe/Paris")
		h.open()
		rule, start, zone := h.values()
		assert.Equal(t, "FREQ=WEEKLY;BYDAY=MO", rule)
		assert.Equal(t, "2026-11-09", start)
		assert.Equal(t, "Europe/Paris", zone)
	})
}

func TestRecurrenceFormStartsAndChangesARecurrence(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)

	h.open()
	h.fill(" FREQ=WEEKLY;BYDAY=MO ", "+2w", "Europe/Paris")
	h.save()
	assert.Equal(t, "started repeat of #"+h.issue.ShortID, h.m.detail.status)

	require.Equal(t, inputNone, h.m.input.kind, "a saved form closes")
	recs := h.rows()
	require.Len(t, recs, 1)
	assert.Equal(t, "FREQ=WEEKLY;BYDAY=MO", recs[0].RRule, "surrounding spaces are trimmed")
	assert.Equal(t, "2026-11-16", recs[0].DTStart)
	assert.Equal(t, "Europe/Paris", recs[0].Timezone)

	h.refreshDetail()
	h.open()
	h.fill("FREQ=DAILY", "2026-11-16", "Europe/Paris")
	h.save()
	assert.Equal(t, "changed repeat of #"+h.issue.ShortID, h.m.detail.status)

	changed := h.rows()
	require.Len(t, changed, 1)
	assert.Equal(t, recs[0].UID, changed[0].UID, "a change keeps the recurrence")
	assert.Equal(t, "FREQ=DAILY", changed[0].RRule)
}

func TestRecurrenceFormBlankRuleStops(t *testing.T) {
	for _, name := range []string{"stops", "conflict"} {
		t.Run(name, func(t *testing.T) {
			conflict := name == "conflict"
			h := newRecurrenceHarness(t, viewDetail)
			current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
			h.open()
			assert.Contains(t, h.page(), "blank rule stops repeating")
			if conflict {
				_, err := recurrenceclient.Change(t.Context(), h.api, h.issue.ProjectID, current,
					recurrenceclient.Terms{Rule: "FREQ=DAILY", Start: "2026-11-09", Zone: "Europe/Paris", Actor: "bob"})
				require.NoError(t, err)
			}
			h.fill("", "", "Local") // Stopping needs neither a date nor a zone.
			h.save()
			if conflict {
				assert.Equal(t, inputRecurrenceForm, h.m.input.kind)
				assert.Contains(t, h.m.input.err, "Could not stop repeat:")
				assert.Len(t, h.rows(), 1)
				return
			}
			assert.Equal(t, inputNone, h.m.input.kind)
			assert.Equal(t, "stopped repeat of #"+h.issue.ShortID, h.m.detail.status)
			assert.Empty(t, h.rows())
			issue, err := h.env.DB.IssueByUID(t.Context(), h.issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			assert.Equal(t, h.issue.Title, issue.Title, "the existing occurrence stays")
			assert.NotNil(t, issue.RecurrenceID)
		})
	}
}

func TestRecurrenceKeyIsDetailOnly(t *testing.T) {
	t.Run("help with split detail focused", func(t *testing.T) {
		h := newRecurrenceHarness(t, viewHelp)
		h.m.layout, h.m.focus = splitlayout.Split, focusDetail
		m, cmd, handled := h.m.routeRecurrenceKey(runeKey('O'))
		assert.False(t, handled)
		assert.Equal(t, inputNone, m.input.kind)
		assert.Nil(t, cmd)
	})

	// Help hides both panes; these cases also check list focus in each layout.
	for _, split := range []bool{false, true} {
		t.Run(strconv.FormatBool(split), func(t *testing.T) {
			h := newRecurrenceHarness(t, viewList)
			if split {
				h.m.layout, h.m.focus = splitlayout.Split, focusList
			}
			_, _, handled := h.m.routeRecurrenceKey(runeKey('O'))
			assert.False(t, handled)
		})
	}
}

func TestRecurrenceKeyWaitsForDetail(t *testing.T) {
	h := newRecurrenceHarness(t, viewList)
	h.startBehindTheTUI("FREQ=DAILY", "2026-11-09", "Europe/Paris")
	h.m = h.m.seedOpenDetail(h.m.list.issues[0])
	_ = h.m.fetchOpenDetail()
	t.Cleanup(h.m.detail.cancelReads)
	require.NotZero(t, h.m.detail.reads.remaining&1)

	h.key(runeKey('O'))

	assert.Equal(t, inputNone, h.m.input.kind)
	assert.Nil(t, h.m.toast, "loading must not report a stopped recurrence")
	h.refreshDetail()
	h.open()
	rule, _, _ := h.values()
	assert.Equal(t, "FREQ=DAILY", rule)
}

func TestRecurrenceUndoBodyEditIgnoresScheduleList(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	h.startBehindTheTUI("FREQ=DAILY", "2026-11-09", "Europe/Paris")
	base := h.client.KataAPI.(*Client)
	base.setHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/recurrences") {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return h.env.HTTP.Do(r)
	})})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	resp, err := h.client.EditBody(ctx, h.issue.ProjectID, h.issue.UID, "revised body", "alice")
	require.NoError(t, err)
	require.NotNil(t, resp.undo.entry)
	resp.undo.complete()
	current, err := h.env.DB.IssueByUID(t.Context(), h.issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "revised body", current.Body)

	out := h.client.undo(ctx, *resp.undo.entry, undoOptions{})
	require.NoError(t, out.err)
	out.attempt.complete()
	current, err = h.env.DB.IssueByUID(t.Context(), h.issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "body", current.Body)
}

func TestRecurrenceKeyRefusesMissingRecurrence(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	require.NoError(t, recurrenceclient.Stop(t.Context(), h.api, h.issue.ProjectID, current, "bob"))
	h.refreshDetail()
	require.NotNil(t, h.m.detail.issue.RecurrenceID)
	cmd := h.key(runeKey('O'))
	assert.NotNil(t, cmd)
	assert.Equal(t, inputNone, h.m.input.kind)
	require.NotNil(t, h.m.toast)
	assert.Equal(t, "Can't read this issue's repeat; it may have been stopped", h.m.toast.text)
}

func TestRecurrenceKeyShowsScopedReadRefusal(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example", Author: "alice",
	})
	require.NoError(t, err)
	authedClient := func(token string) *Client {
		return NewClient(env.URL, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
			return env.HTTP.Do(r)
		})})
	}
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "owner-token", Actor: "alice", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	api, err := authedClient("owner-token").generatedClient()
	require.NoError(t, err)
	require.NoError(t, recurrenceclient.Start(t.Context(), api, project.ID, issue.UID,
		recurrenceclient.Template{Title: "Example"},
		recurrenceclient.Terms{Rule: "FREQ=WEEKLY", Start: "2026-11-09", Zone: "Europe/Paris", Actor: "alice"}, nil))
	expires := time.Now().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "alice", AdminActor: db.BootstrapActor,
		Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: issue.UID},
		ExpiresAt: &expires,
	})
	require.NoError(t, err)
	m := initialModel(Options{})
	m.view = viewDetail
	out, _ := m.Update(fetchIssue(authedClient("worker-token"), project.ID, issue.UID, m.detail.gen)())
	m = out.(Model)
	require.NotNil(t, m.detail.issue, "the scoped token can still read the issue")
	require.NotNil(t, m.detail.issue.RecurrenceID)
	m, _, handled := m.routeRecurrenceKey(runeKey('O'))
	require.True(t, handled)
	assert.Equal(t, inputNone, m.input.kind)
	require.NotNil(t, m.toast)
	assert.Contains(t, m.toast.text, "Could not read the repeat:")
	assert.Contains(t, m.toast.text, "operation is not available to an issue-scoped credential")
	assert.NotContains(t, m.toast.text, "may have been stopped")
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
		{"a blank rule requires an existing recurrence", nil, "", "Europe/Paris", []string{"Could not start repeat:"}},
		{"an issue changed after the form opened is refused", func(h *recurrenceHarness) {
			h.patchDaemon(map[string]string{"scheduled_on": "2026-12-01"})
		}, "FREQ=DAILY", "Europe/Paris", []string{"Could not start repeat:"}},
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

func TestRecurrenceSaveUpdatesUndo(t *testing.T) {
	for _, outcome := range []string{"server", "transport", "write transport", "success", "daemon no-op detail"} {
		t.Run(outcome, func(t *testing.T) {
			noop := outcome == "daemon no-op detail"
			h := newRecurrenceHarness(t, viewDetail)
			h.openForm()
			h.datesHarness.fill("2026-11-09", "")
			h.datesHarness.save()
			h.refreshDetail()
			require.Len(t, h.m.undoHistory.entries, 1)
			if noop {
				h.startBehindTheTUI("FREQ=WEEKLY;BYDAY=MO", "2026-11-09", "Europe/Paris")
			}
			h.open()
			if noop {
				h.fill(" FREQ=WEEKLY;BYDAY=MO ", "2026-11-09", "")
			} else {
				h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")
			}
			writes := 0
			if !noop && outcome != "success" {
				base := h.client.KataAPI.(*Client)
				base.setHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet {
						writes++
						return nil, errors.New("write transport failed")
					}
					if strings.Contains(r.URL.Path, "/issues/") && (outcome == "server" || outcome == "transport") {
						if outcome == "transport" {
							return nil, errors.New("preflight transport failed")
						}
						status, body := http.StatusInternalServerError, `{"error":{"code":"internal","message":"preflight failed"}}`
						return &http.Response{StatusCode: status, Request: r, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
					}
					return h.env.HTTP.Do(r)
				})})
			}
			h.save()
			if outcome == "success" {
				assert.Empty(t, h.m.undoHistory.entries)
				assert.Contains(t, h.m.undoHistory.boundary, "recurrence")
				return
			}
			if noop {
				assert.Equal(t, inputNone, h.m.input.kind)
				assert.Empty(t, h.m.detail.status, "the daemon reported no change")
				assert.Nil(t, h.m.toast)
				assert.Len(t, h.m.undoHistory.entries, 1, "a daemon no-op preserves undo")
				return
			}
			assert.Equal(t, inputRecurrenceForm, h.m.input.kind)
			assert.NotEmpty(t, h.m.input.err)
			if outcome == "write transport" {
				assert.Equal(t, 1, writes)
				assert.Empty(t, h.m.undoHistory.entries, "a sent write still has an uncertain outcome")
			} else {
				assert.Zero(t, writes)
				assert.Len(t, h.m.undoHistory.entries, 1, "a failed read preserves the earlier edit")
			}
		})
	}
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

func TestRecurrenceKeyUsesTheSamePermissionGateAsOtherEdits(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	h.m.issueScoped, h.m.scopedWritable = true, false

	h.key(runeKey('O'))

	assert.Equal(t, inputNone, h.m.input.kind)
}

func TestSaveRecurrenceRequiresGuardedStartAPI(t *testing.T) {
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
	h.client = newConnectedUndoClient(t, NewClient(h.env.URL, hc))
	_, err := h.client.SaveRecurrence(t.Context(), h.issue.ProjectID, h.issue.UID, RecurrenceInput{
		Rule:          "FREQ=DAILY",
		Start:         "2026-11-09",
		Zone:          "Europe/Paris",
		Actor:         "alice",
		IssueRevision: h.issue.Revision,
	})
	require.Error(t, err)
	assert.Zero(t, posts, "an older daemon cannot enforce a guarded start")
	assert.Contains(t, err.Error(), "upgrade the daemon")
}

func TestRecurrenceIncomingMutationRefreshesDetail(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	h.refreshDetail()
	require.Contains(t, h.page(), "repeats: FREQ=WEEKLY")
	require.NoError(t, recurrenceclient.Stop(t.Context(), h.api, h.issue.ProjectID, current, "bob"))
	event := decodeEventReceived(frame{data: []byte(`{"type":"recurrence.deleted","project_id":` + strconv.FormatInt(h.issue.ProjectID, 10) + `,"payload":{"recurrence_uid":"` + current.UID + `"}}`)})
	cmd := h.m.maybeRefetchOpenDetail(event)
	for _, msg := range runCmdTree(cmd) {
		out, next := h.m.Update(msg)
		h.m = out.(Model)
		for _, fetched := range runCmdTree(next) {
			out, _ = h.m.Update(fetched)
			h.m = out.(Model)
		}
	}
	assert.NotContains(t, h.page(), "repeats:")
}

func TestRecurrenceFormConflictKeepsInputUntilReopened(t *testing.T) {
	h := newRecurrenceHarness(t, viewDetail)
	current := h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	h.open()
	_, err := recurrenceclient.Change(t.Context(), h.api, h.issue.ProjectID, current,
		recurrenceclient.Terms{Rule: "FREQ=MONTHLY", Start: "2026-11-09", Zone: "Europe/Paris", Actor: "bob"})
	require.NoError(t, err)
	h.fill("FREQ=DAILY", "2026-11-09", "Europe/Paris")
	h.save()
	require.Equal(t, inputRecurrenceForm, h.m.input.kind)
	assert.Contains(t, h.m.input.err, "Could not change repeat:")
	rule, _, _ := h.values()
	assert.Equal(t, "FREQ=DAILY", rule)
	assert.Equal(t, current.Revision, h.m.input.recurrence.rec.Revision)
	assert.Equal(t, "FREQ=MONTHLY", h.rows()[0].RRule)
	h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	h.refreshDetail()
	h.open()
	rule, _, _ = h.values()
	assert.Equal(t, "FREQ=MONTHLY", rule)
}

func TestRecurrenceFormDaemonErrorFitsMinimumHeight(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{40, 60, 80} {
		// Keep the start form too: status rendering must work without a recurrence.
		for _, rec := range []*recurrenceclient.View{nil, {RRule: "FREQ=YEARLY;INTERVAL=2;BYMONTH=1,2,3,4,5,6,7,8,9,10,11,12;BYDAY=MO,TU,WE,TH,FR;BYHOUR=0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23;BYMINUTE=0,15,30,45;BYSECOND=0;WKST=MO", DTStart: "2026-11-09", Timezone: "Europe/Paris"}} {
			s := newRecurrenceForm(formTarget{issueShortID: "42aa"}, Issue{}, rec)
			s.err = "Could not start repeat: the daemon refused the recurrence rule FREQ=WEEKLY;INTERVAL=2;BYDAY=MON,TUE,WED,THU,FRI. Enter a supported rule and save again."
			modal := renderCenteredForm(s, width, 12, snapshotFixedNow)
			assert.LessOrEqual(t, lipgloss.Height(modal), 12)
			assert.LessOrEqual(t, lipgloss.Width(modal), width)
			got := stripANSI(modal)
			for _, want := range []string{"Rule", "Start", "Zone", "Could not start repeat", "ctrl+o save", "esc"} {
				assert.Contains(t, got, want)
			}
		}
	}
}

func TestRecurrenceFormRejectedLongRuleFitsMinimumHeight(t *testing.T) {
	defer snapshotInit(t)()
	h := newRecurrenceHarness(t, viewDetail)
	h.startBehindTheTUI("FREQ=WEEKLY", "2026-11-09", "Europe/Paris")
	h.open()
	h.fill("FREQ=DAILY;BYDAY=MO;BYMONTH=1,2,3,4,5;BAD=1", "2026-11-09", "Europe/Paris")
	// Match the cursor position after typing the rule.
	h.key(tea.KeyPressMsg{Code: tea.KeyEnd})
	h.save()
	require.Contains(t, h.m.input.err, "unknown RRULE property: BAD")

	for _, width := range []int{80, 40} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			modal := renderCenteredForm(h.m.input, width, 12, snapshotFixedNow)
			assert.LessOrEqual(t, lipgloss.Height(modal), 12)
			assert.LessOrEqual(t, lipgloss.Width(modal), width)
			lines := strings.Split(stripANSI(modal), "\n")
			for i := range lines {
				lines[i] = strings.Trim(lines[i], "│ ")
			}
			got := strings.Join(lines, " ")
			for _, want := range []string{"Rule", "Start", "Zone", "Could not change repeat", "unknown RRULE property: BAD", "ctrl+o save", "esc"} {
				assert.Contains(t, got, want)
			}
		})
	}
}
