package tui

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kit/tui/splitlayout"
)

const (
	backlogParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	backlogEventUID  = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
)

// backlogJob describes one native recurring job stored on the issue being
// reopened. missed is how many one-minute occurrences passed since its progress
// stopped advancing; the extra half minute keeps the count stable while the
// test runs.
type backlogJob struct {
	uid        string
	catchup    string
	grace      int64
	missed     int
	deleted    bool
	unreadable bool
}

func (j backlogJob) definition() api.CronJob {
	def := api.CronJobDefinition{
		Version: 1, Kind: "job", Enabled: true,
		Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60},
		Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: backlogParentUID, Title: "Review"},
		Overlap: "allow", Catchup: j.catchup, GraceSeconds: j.grace,
	}
	var job api.CronJob
	job.UID, job.DefinitionEventUID, job.Definition = j.uid, backlogEventUID, def
	if j.deleted {
		now := time.Now().UTC()
		job.DeletedAt = &now
	}
	return job
}

func (j backlogJob) progress() cron.RecurringProgress {
	through := time.Now().UTC().Add(-time.Duration(j.missed)*time.Minute - 30*time.Second).Truncate(time.Second)
	return cron.RecurringProgress{Version: 1, Anchor: through, Through: through}
}

// backlogDaemon serves the two reads the reopen check makes: the issue's
// metadata and each job named by a progress key.
func backlogDaemon(t *testing.T, extraMeta map[string]any, jobs ...backlogJob) *Client {
	t.Helper()
	metadata := map[string]any{}
	byUID := map[string]backlogJob{}
	for _, j := range jobs {
		metadata[cronProgressPrefix+j.uid] = j.progress()
		byUID[j.uid] = j
	}
	maps.Copy(metadata, extraMeta)
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/7/issues/abcd/metadata":
			respondJSON(t, w, map[string]any{"issue": map[string]any{"short_id": "abcd", "metadata": metadata, "revision": 3}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/7/cron/jobs/"):
			job, ok := byUID[strings.TrimPrefix(r.URL.Path, "/api/v1/projects/7/cron/jobs/")]
			if !ok || job.unreadable {
				http.NotFound(w, r)
				return
			}
			respondJSON(t, w, map[string]any{"job": job.definition()})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func TestReopenBacklogCountsRecurringJobs(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
		jobs []backlogJob
		want int
	}{
		{"no cron progress", map[string]any{"note": "text"}, nil, 0},
		{"all job missed several", nil, []backlogJob{{uid: "job-a", catchup: "all", missed: 3}}, 3},
		{"jobs on one parent are summed", nil, []backlogJob{
			{uid: "job-a", catchup: "all", missed: 3},
			{uid: "job-b", catchup: "all", missed: 4},
		}, 7},
		{"skip with several occurrences inside grace", nil, []backlogJob{{uid: "job-a", catchup: "skip", grace: 300, missed: 800}}, 5},
		{"latest delivers one", nil, []backlogJob{{uid: "job-a", catchup: "latest", missed: 500}}, 1},
		{"skip delivers only what is inside grace", nil, []backlogJob{{uid: "job-a", catchup: "skip", grace: 60, missed: 500}}, 1},
		{"nothing missed", nil, []backlogJob{{uid: "job-a", catchup: "all", missed: 0}}, 0},
		{"count is exact beyond one thousand", nil, []backlogJob{{uid: "job-a", catchup: "all", missed: 2503}}, 2503},
		{"deleted and unreadable jobs are ignored", nil, []backlogJob{
			{uid: "job-a", catchup: "all", missed: 3},
			{uid: "job-b", catchup: "all", missed: 9, deleted: true},
			{uid: "job-c", catchup: "all", missed: 9, unreadable: true},
		}, 3},
		{"latest and all mix", nil, []backlogJob{
			{uid: "job-a", catchup: "latest", missed: 50},
			{uid: "job-b", catchup: "all", missed: 2},
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := backlogDaemon(t, tc.meta, tc.jobs...)
			got, err := c.ReopenBacklog(context.Background(), 7, "abcd")
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestReopenBacklogReportsMetadataFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	_, err := c.ReopenBacklog(context.Background(), 7, "abcd")
	require.Error(t, err)
}

// reopenSpyAPI counts the reopens the model sends to the daemon.
type reopenSpyAPI struct {
	*Client
	reopens    int
	project    int64
	ref, actor string
}

func (s *reopenSpyAPI) Reopen(_ context.Context, projectID int64, ref, actor string) (*MutationResp, error) {
	s.reopens++
	s.project, s.ref, s.actor = projectID, ref, actor
	return &MutationResp{Issue: &Issue{UID: "01TEST-abcd", ShortID: "abcd", Status: "open"}, Changed: true}, nil
}

func listReopenModel(t *testing.T, spy *reopenSpyAPI) Model {
	t.Helper()
	m := newTestModel()
	m.api = spy
	m.view = viewList
	m.list.actor = "tester"
	m.list.issues = []Issue{{ProjectID: 7, ShortID: "abcd", Status: "closed", Title: "Parent"}}
	return m
}

func detailReopenModel(t *testing.T, spy *reopenSpyAPI) Model {
	t.Helper()
	m := newTestModel()
	m.api = spy
	m.view = viewDetail
	m.detail.actor = "tester"
	m.detail.scopePID = 7
	m.detail.gen = 4
	m.detail.issue = &Issue{ProjectID: 7, ShortID: "abcd", Status: "closed", Title: "Parent"}
	return m
}

// pressReopen sends r and delivers the backlog answer back to the model.
func pressReopen(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	m, cmd := updateModel(m, runeKey('r'))
	require.NotNil(t, cmd)
	msg, ok := cmd().(reopenBacklogMsg)
	require.True(t, ok, "r must check the backlog before reopening")
	return updateModel(m, msg)
}

func TestReopenFromListAndDetailPromptsBeforeDeliveringBacklog(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      func(*testing.T, *reopenSpyAPI) Model
		wantOrigin string
		wantGen    int64
	}{
		{"list", listReopenModel, "list", 0},
		{"detail", detailReopenModel, "detail", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
			m, cmd := pressReopen(t, tc.model(t, spy))
			require.Nil(t, cmd)
			require.Equal(t, modalReopenBacklog, m.modal)
			require.Zero(t, spy.reopens, "nothing is reopened until the user confirms")
			view := stripANSI(renderConfirmModal(m.modal, m.reopenPrompt))
			require.Contains(t, view, "Reopen #abcd?")
			require.Contains(t, view, "3 recurring child")
			require.Contains(t, view, "kata cron job update")
			require.NotContains(t, view, "latest", "the prompt informs without steering catch-up")

			m, cmd = updateModel(m, runeKey('y'))
			require.Equal(t, modalNone, m.modal)
			require.NotNil(t, cmd)
			done, ok := cmd().(mutationDoneMsg)
			require.True(t, ok)
			require.Equal(t, 1, spy.reopens)
			require.Equal(t, "reopen", done.kind)
			require.Equal(t, tc.wantOrigin, done.origin)
			require.Equal(t, tc.wantGen, done.gen)
		})
	}
}

func TestReopenBacklogCancelLeavesIssueClosed(t *testing.T) {
	for _, cancel := range []tea.KeyPressMsg{runeKey('n'), {Code: tea.KeyEscape}} {
		spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 2})}
		m, _ := pressReopen(t, listReopenModel(t, spy))
		require.Equal(t, modalReopenBacklog, m.modal)
		m, cmd := updateModel(m, cancel)
		require.Equal(t, modalNone, m.modal)
		require.Nil(t, cmd)
		require.Nil(t, m.reopenPrompt)
		require.Zero(t, spy.reopens)
	}
}

func TestReopenWithoutBacklogSkipsPrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		jobs []backlogJob
	}{
		{"no cron jobs", nil},
		{"zero due", []backlogJob{{uid: "job-a", catchup: "all", missed: 0}}},
		{"one due", []backlogJob{{uid: "job-a", catchup: "all", missed: 1}}},
	} {
		for _, view := range []string{"list", "detail"} {
			t.Run(tc.name+"/"+view, func(t *testing.T) {
				spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, tc.jobs...)}
				model := listReopenModel
				if view == "detail" {
					model = detailReopenModel
				}
				m, cmd := pressReopen(t, model(t, spy))
				require.Equal(t, modalNone, m.modal)
				require.NotNil(t, cmd)
				require.IsType(t, mutationDoneMsg{}, cmd())
				require.Equal(t, 1, spy.reopens)
			})
		}
	}
}

func TestReopenReadFailureStillReopens(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	spy := &reopenSpyAPI{Client: client}
	m, cmd := pressReopen(t, listReopenModel(t, spy))
	require.Equal(t, modalNone, m.modal)
	require.NotNil(t, cmd)
	cmd()
	require.Equal(t, 1, spy.reopens)
}

func TestReopenBacklogAnswerForMovedSelectionIsDropped(t *testing.T) {
	spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
	m := listReopenModel(t, spy)
	m, cmd := updateModel(m, runeKey('r'))
	msg := cmd().(reopenBacklogMsg)
	m.list.issues = []Issue{{ProjectID: 7, ShortID: "wxyz", Status: "closed"}}
	m, cmd = updateModel(m, msg)
	require.Nil(t, cmd)
	require.Equal(t, modalNone, m.modal)
	require.Zero(t, spy.reopens)
}

// backlogUndoAPI answers the backlog question for the undo path.
type backlogUndoAPI struct {
	*undoTestAPI
	count int
	err   error
}

func (b *backlogUndoAPI) ReopenBacklog(context.Context, int64, string) (int, error) {
	return b.count, b.err
}

func undoCloseModel(t *testing.T, count int, err error) (Model, *undoTestAPI) {
	t.Helper()
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "closed", ClosedReason: &done, Revision: 4},
		writeResp: &MutationResp{Issue: &Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 5}, Changed: true}}
	m := initialModel(Options{})
	m.api = newConnectedUndoClient(t, &backlogUndoAPI{undoTestAPI: f, count: count, err: err})
	m.view = viewList
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: f.issue, revision: 4})
	return m, f
}

func pressUndo(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(runeKey('u'))
	require.NotNil(t, cmd)
	m = updated.(Model)
	updated, cmd = m.Update(cmd())
	return updated.(Model), cmd
}

func TestUndoCloseConfirmsBacklogBeforeReopening(t *testing.T) {
	m, f := undoCloseModel(t, 5, nil)
	m, cmd := pressUndo(t, m)
	require.Nil(t, cmd)
	require.Equal(t, modalReopenBacklog, m.modal)
	require.Zero(t, f.reopenCalls)
	require.Len(t, m.undoHistory.entries, 1, "the close stays undoable until confirmed")
	require.False(t, m.undoInFlight)
	view := stripANSI(renderConfirmModal(m.modal, m.reopenPrompt))
	require.Contains(t, view, "5 recurring child")
	require.Contains(t, view, "kata cron job update")

	m, cmd = updateModel(m, runeKey('y'))
	require.NotNil(t, cmd)
	require.Equal(t, modalNone, m.modal)
	updated, _ := m.Update(cmd())
	m = updated.(Model)
	require.Equal(t, 1, f.reopenCalls)
	require.Empty(t, m.undoHistory.entries)
}

func TestUndoCloseCancelKeepsEntryAndIssueClosed(t *testing.T) {
	m, f := undoCloseModel(t, 5, nil)
	m, _ = pressUndo(t, m)
	m, cmd := updateModel(m, runeKey('n'))
	require.Nil(t, cmd)
	require.Equal(t, modalNone, m.modal)
	require.Zero(t, f.reopenCalls)
	require.Len(t, m.undoHistory.entries, 1)
}

func TestUndoCloseWithoutBacklogReopensDirectly(t *testing.T) {
	for name, tc := range map[string]struct {
		count int
		err   error
	}{
		"zero":         {0, nil},
		"one":          {1, nil},
		"lookup fails": {0, context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			m, f := undoCloseModel(t, tc.count, tc.err)
			m, _ = pressUndo(t, m)
			require.Equal(t, modalNone, m.modal)
			require.Equal(t, 1, f.reopenCalls)
			require.Empty(t, m.undoHistory.entries)
		})
	}
}

func TestReopenBacklogModalFooterShowsChoices(t *testing.T) {
	spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
	m, _ := updateModel(listReopenModel(t, spy), tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = pressReopen(t, m)
	footer := stripANSI(m.View().Content)
	require.Contains(t, footer, "reopen")
	require.Contains(t, footer, "cancel")
}

// Without a backlog reader the key still reopens, with the row's project, ref and actor.
func TestReopenDispatchesRowAndActor(t *testing.T) {
	for name, model := range map[string]func(*testing.T, *reopenSpyAPI) Model{"list": listReopenModel, "detail": detailReopenModel} {
		t.Run(name, func(t *testing.T) {
			spy := &reopenSpyAPI{Client: backlogDaemon(t, nil)}
			m := model(t, spy)
			m.list.status = "stale status"
			m, cmd := pressReopen(t, m)
			require.NotNil(t, cmd)
			require.IsType(t, mutationDoneMsg{}, cmd())
			require.Equal(t, 1, spy.reopens)
			require.Equal(t, int64(7), spy.project)
			require.Equal(t, "abcd", spy.ref)
			require.Equal(t, "tester", spy.actor)
			if name == "list" {
				require.Empty(t, m.list.status)
			}
		})
	}
}

// An open parent can be part-way through delivering a backlog; reopening does
// nothing for it, so the closed-period warning must not appear.
func TestReopenOfOpenIssueSkipsBacklogCheck(t *testing.T) {
	for name, model := range map[string]func(*testing.T, *reopenSpyAPI) Model{"list": listReopenModel, "detail": detailReopenModel} {
		t.Run(name, func(t *testing.T) {
			spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
			m := model(t, spy)
			if name == "list" {
				m.list.issues[0].Status = "open"
			} else {
				m.detail.issue.Status = "open"
			}
			m, cmd := updateModel(m, runeKey('r'))
			require.Equal(t, modalNone, m.modal)
			require.NotNil(t, cmd)
			require.IsType(t, mutationDoneMsg{}, cmd())
			require.Equal(t, 1, spy.reopens)
		})
	}
}

// A backlog answer that lands while the quit confirmation is open must not
// replace it: the `y` meant to quit would otherwise reopen the issue.
func TestReopenBacklogAnswerDoesNotReplaceQuitConfirm(t *testing.T) {
	spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
	m, cmd := updateModel(listReopenModel(t, spy), runeKey('r'))
	require.NotNil(t, cmd)
	reply := cmd()
	m, _ = updateModel(m, runeKey('q'))
	require.Equal(t, modalQuitConfirm, m.modal)

	m, cmd = updateModel(m, reply)
	require.Nil(t, cmd)
	require.Equal(t, modalQuitConfirm, m.modal)
	require.Nil(t, m.reopenPrompt)

	_, cmd = updateModel(m, runeKey('y'))
	require.NotNil(t, cmd)
	require.IsType(t, tea.QuitMsg{}, cmd())
	require.Zero(t, spy.reopens)
}

// An answer arriving while a form owns input is dropped instead of stacking a
// confirmation over the form.
func TestReopenBacklogAnswerIgnoredWhileInputActive(t *testing.T) {
	spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
	m, cmd := updateModel(listReopenModel(t, spy), runeKey('r'))
	require.NotNil(t, cmd)
	reply := cmd()
	m, _ = m.openInput(inputSearchBar)
	require.Equal(t, inputSearchBar, m.input.kind)

	m, cmd = updateModel(m, reply)
	require.Nil(t, cmd)
	require.Equal(t, modalNone, m.modal)
	require.Nil(t, m.reopenPrompt)
	require.Zero(t, spy.reopens)
}

// The undo backlog answer likewise must not replace an open quit confirmation.
func TestUndoBacklogAnswerDoesNotReplaceQuitConfirm(t *testing.T) {
	m, f := undoCloseModel(t, 5, nil)
	updated, cmd := m.Update(runeKey('u'))
	require.NotNil(t, cmd)
	m = updated.(Model)
	reply := cmd()
	m, _ = updateModel(m, runeKey('q'))
	require.Equal(t, modalQuitConfirm, m.modal)

	m, cmd = updateModel(m, reply)
	require.Nil(t, cmd)
	require.Equal(t, modalQuitConfirm, m.modal)
	require.Nil(t, m.reopenPrompt)
	require.False(t, m.undoInFlight)
	require.Len(t, m.undoHistory.entries, 1, "the close stays undoable")

	_, cmd = updateModel(m, runeKey('y'))
	require.NotNil(t, cmd)
	require.IsType(t, tea.QuitMsg{}, cmd())
	require.Zero(t, f.reopenCalls)
}

// In split layout the hidden selection must not be reopened from the help view.
func TestReopenKeyIgnoredWhileHelpIsVisibleInSplitLayout(t *testing.T) {
	spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
	m := listReopenModel(t, spy)
	m.layout = splitlayout.Split
	m.focus = focusList
	m, _ = updateModel(m, runeKey('?'))
	require.Equal(t, viewHelp, m.view)

	m, cmd := updateModel(m, runeKey('r'))
	require.Nil(t, cmd)
	require.Equal(t, modalNone, m.modal)
	require.Zero(t, spy.reopens)
}

// slowBacklogUndoAPI models a backlog lookup that outlasts the undo attempt:
// a lookup with its own deadline times out harmlessly, while one that shares
// the attempt's context uses the attempt up (expireAttempt) before returning.
// Reopen refuses a context that has already ended.
type slowBacklogUndoAPI struct {
	*undoTestAPI
	expireAttempt context.CancelFunc
}

func (s *slowBacklogUndoAPI) ReopenBacklog(ctx context.Context, _ int64, _ string) (int, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		s.expireAttempt()
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func (s *slowBacklogUndoAPI) Reopen(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.undoTestAPI.Reopen(ctx, projectID, ref, actor)
}

// A backlog lookup that runs out of time must not take the undo's own deadline
// with it: the reopen still goes through and is not classified as ambiguous.
func TestUndoCloseSlowBacklogLookupStillReopens(t *testing.T) {
	previous := undoBacklogLookupTimeout
	undoBacklogLookupTimeout = 20 * time.Millisecond
	t.Cleanup(func() { undoBacklogLookupTimeout = previous })

	m, f := undoCloseModel(t, 0, nil)
	ctx, expireAttempt := context.WithCancel(context.Background())
	defer expireAttempt()
	client := newConnectedUndoClient(t, &slowBacklogUndoAPI{undoTestAPI: f, expireAttempt: expireAttempt})

	out := client.undo(ctx, m.undoHistory.entries[0], undoOptions{checkBacklog: true})
	if out.attempt != nil {
		defer out.attempt.complete()
	}
	require.NoError(t, out.err)
	require.True(t, out.changed)
	require.False(t, out.attempt.unknown, "an expired lookup is not an ambiguous write")
	require.Equal(t, 1, f.reopenCalls)
}

// The lookup must read what a real daemon serves: a drift in the metadata or
// cron job responses would otherwise be swallowed as a zero backlog and the
// prompt would silently vanish.
func TestReopenBacklogAgainstRealDaemon(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Ongoing review", Author: "operator"})
	require.NoError(t, err)

	request := fmt.Sprintf(`{"actor":"operator","name":"Review every minute","definition":{"version":1,"kind":"job","enabled":true,`+
		`"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"notify","recipient":"operator/consumer"},`+
		`"issue":{"kind":"per-run","parent_uid":%q,"title":"Review service"},"overlap":"allow","catchup":"all"}}`, parent.UID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/cron/jobs", env.URL, project.ID), strings.NewReader(request))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var created struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(body, &created))

	client := NewClient(env.URL, env.HTTP)
	_, err = client.Close(ctx, project.ID, parent.ShortID, "operator")
	require.NoError(t, err)

	// Closing pauses the job; move its stored progress back so three
	// occurrences came due while the parent was closed.
	stale := backlogJob{uid: created.Job.UID, missed: 3}.progress()
	raw, err := json.Marshal(map[string]cron.RecurringProgress{cronProgressPrefix + created.Job.UID: stale})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(ctx, `UPDATE issues SET metadata=? WHERE id=?`, string(raw), parent.ID)
	require.NoError(t, err)

	count, err := client.ReopenBacklog(ctx, project.ID, parent.ShortID)
	require.NoError(t, err)
	require.Equal(t, 3, count)
}

// `r` after `!` cancels the priority prompt, as any other key does, so a digit
// typed after the reopen prompt is answered does not change the priority.
func TestReopenKeyCancelsPendingPriority(t *testing.T) {
	spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
	m, _ := updateModel(listReopenModel(t, spy), runeKey('!'))
	require.True(t, m.list.pendingPriority)

	m, cmd := updateModel(m, runeKey('r'))
	require.NotNil(t, cmd)
	require.False(t, m.list.pendingPriority)
	m, _ = updateModel(m, cmd())
	require.Equal(t, modalReopenBacklog, m.modal)
	m, _ = updateModel(m, runeKey('n'))

	_, cmd = updateModel(m, runeKey('2'))
	require.Nil(t, cmd, "a stale priority prompt must not turn the digit into a priority change")
}

// Pressing `r` twice starts two lookups. A failed lookup's zero count must not
// reopen the issue while the other lookup's confirmation is open, nor
// before it, because only the latest press is still wanted.
func TestReopenSupersededLookupNeverDispatches(t *testing.T) {
	for _, order := range []string{"confirmation first", "fallback first"} {
		t.Run(order, func(t *testing.T) {
			spy := &reopenSpyAPI{Client: backlogDaemon(t, nil, backlogJob{uid: "job-a", catchup: "all", missed: 3})}
			m, first := updateModel(listReopenModel(t, spy), runeKey('r'))
			m, second := updateModel(m, runeKey('r'))
			require.NotNil(t, first)
			require.NotNil(t, second)
			failed := first().(reopenBacklogMsg)
			failed.count = 0
			confirm := second().(reopenBacklogMsg)

			replies := []reopenBacklogMsg{confirm, failed}
			if order == "fallback first" {
				replies = []reopenBacklogMsg{failed, confirm}
			}
			for _, reply := range replies {
				var cmd tea.Cmd
				m, cmd = updateModel(m, reply)
				require.Nil(t, cmd, "no reopen is sent without the user's confirmation")
			}
			require.Equal(t, modalReopenBacklog, m.modal)
			require.Zero(t, spy.reopens)
		})
	}
}
