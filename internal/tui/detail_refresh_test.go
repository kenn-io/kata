package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
	"pgregory.net/rapid"
)

func TestDetailLiveRefreshTickIsFencedByNavigation(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"})
	msg := cmd()
	_, immediate := msg.(tea.BatchMsg)
	require.False(t, immediate, "live invalidation must debounce reads")
	m = m.seedOpenDetail(Issue{ProjectID: 7, ShortID: "def5", UID: "01UID-NEXT"})
	_, work, handled := m.routeSSE(msg)
	require.True(t, handled)
	require.Nil(t, work, "old tick must not refresh the new issue")
}

func TestDetailLiveEventsDoNotStarveActiveReads(t *testing.T) {
	api := &completingDetailAPI{entered: make(chan context.Context, 4), release: make(chan struct{})}
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.api = api
	next, cmd := m.handleOpenDetail(openDetailMsg{issue: *m.detail.issue})
	m = next.(Model)
	done := make(chan tea.Msg, 4)
	for _, work := range cmd().(tea.BatchMsg) {
		go func() { done <- work() }()
	}
	contexts := make([]context.Context, 0, 4)
	for range 4 {
		select {
		case ctx := <-api.entered:
			contexts = append(contexts, ctx)
		case <-time.After(time.Second):
			t.Fatal("read did not start")
		}
	}
	defer m.detail.cancelReads()
	for range 40 {
		require.Nil(t, m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}), "active batch should mark dirty")
	}
	for _, ctx := range contexts {
		require.NoError(t, ctx.Err(), "live events canceled useful work")
	}
	close(api.release)
	trailing := 0
	for range 4 {
		select {
		case msg := <-done:
			next, cmd = m.Update(msg)
			m = next.(Model)
			if cmd != nil {
				trailing++
				require.Len(t, cmd().(tea.BatchMsg), 4)
			}
		case <-time.After(time.Second):
			t.Fatal("read did not finish")
		}
	}
	require.Equal(t, 1, trailing, "dirty active reads require exactly one trailing refresh")
}

type completingDetailAPI struct {
	KataAPI
	entered chan context.Context
	release chan struct{}
}

func (a *completingDetailAPI) read(ctx context.Context) error {
	a.entered <- ctx
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *completingDetailAPI) GetIssueDetail(ctx context.Context, _ int64, _ string) (*IssueDetail, error) {
	return nil, a.read(ctx)
}
func (a *completingDetailAPI) ListComments(ctx context.Context, _ int64, _ string) ([]CommentEntry, error) {
	return nil, a.read(ctx)
}
func (a *completingDetailAPI) ListEvents(ctx context.Context, _ int64, _ string) ([]EventLogEntry, error) {
	return nil, a.read(ctx)
}
func (a *completingDetailAPI) ListLinks(ctx context.Context, _ int64, _ string) ([]LinkEntry, error) {
	return nil, a.read(ctx)
}

func TestDetailLiveRefreshTwoEventsScheduleOneTick(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	event := eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}
	require.NotNil(t, m.maybeRefetchOpenDetail(event))
	require.Nil(t, m.maybeRefetchOpenDetail(event))
}

func TestDetailRefreshEligible(t *testing.T) {
	detail := sseDetailFixture(7, "abc4", "01UID-OPEN")
	detail.layout = splitlayout.Stacked
	require.True(t, detail.detailRefreshEligible(), "stacked detail view needs refreshes")

	help := detail
	help.view = viewHelp
	help.prevView = viewDetail
	require.True(t, help.detailRefreshEligible(), "Help stacked over detail retains refreshes")

	otherHelp := help
	otherHelp.prevView = viewList
	require.False(t, otherHelp.detailRefreshEligible(), "Help over the list does not retain detail")

	otherView := detail
	otherView.view = viewDaemons
	require.False(t, otherView.detailRefreshEligible(), "other stacked views do not refresh detail")

	split := otherView
	split.layout = splitlayout.Split
	require.True(t, split.detailRefreshEligible(), "split layout keeps detail fresh")
}

func TestDetailLiveRefreshTickRefreshesStackedDetailBehindHelp(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	tickCmd := m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"})
	require.NotNil(t, tickCmd)
	tick := tickCmd()
	m, _ = m.toggleHelp()
	require.Equal(t, viewHelp, m.view)
	require.Equal(t, viewDetail, m.prevView)

	next, cmd, handled := m.routeSSE(tick)
	require.True(t, handled)
	require.NotNil(t, cmd, "a pending detail invalidation must refresh behind Help")
	require.Len(t, cmd().(tea.BatchMsg), 4)
	refreshed := next.(Model)
	refreshed.detail.cancelReads()
}

func TestDetailLiveRefreshDirtyBatchRefreshesStackedDetailBehindHelp(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	api := &fakeDetailAPI{getIssueResult: m.detail.issue}
	reads := m.detail.refetch(api)().(tea.BatchMsg)
	require.Nil(t, m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}))
	m, _ = m.toggleHelp()

	var trailing tea.Cmd
	for _, read := range reads {
		next, cmd := m.Update(read())
		m = next.(Model)
		if cmd != nil {
			trailing = cmd
		}
	}
	require.NotNil(t, trailing, "a dirty detail batch must refresh behind Help")
	require.Len(t, trailing().(tea.BatchMsg), 4)
	m.detail.cancelReads()
}

func TestDetailLiveEventRefreshesStackedDetailBehindHelp(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m, _ = m.toggleHelp()

	tickCmd := m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"})
	require.NotNil(t, tickCmd, "matching events received behind Help must schedule refresh")
	next, cmd, handled := m.routeSSE(tickCmd())
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.Len(t, cmd().(tea.BatchMsg), 4)
	refreshed := next.(Model)
	refreshed.detail.cancelReads()
}

func TestDetailCanceledLiveTickDoesNotRestartReads(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	tick := m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"})()
	m.detail.cancelReads()
	_, cmd, _ := m.routeSSE(tick)
	require.Nil(t, cmd, "canceled read owner must not be revived by its pending tick")
}

func TestSearchSnapshotsRejectCanceledReadsAndRetainMutation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := sseDetailFixture(7, "abc4", "01UID-OPEN")
		m.detail.gen = 1
		m.nextGen = 1
		old := m.detail.refetch(&fakeDetailAPI{})().(tea.BatchMsg)
		snapshot := cloneDetailModel(m.detail)
		m.input.kind = inputSearchBar
		m.input.preSplitDetail = &snapshot
		if rapid.Bool().Draw(t, "retarget") {
			m = m.seedOpenDetail(Issue{ProjectID: 7, ShortID: "def5", UID: "01UID-NEXT"})
		} else {
			_ = m.detail.refetch(&fakeDetailAPI{})
		}
		defer m.detail.cancelReads()
		messages := make([]tea.Msg, 0, 5)
		for _, cmd := range old {
			messages = append(messages, cmd())
		}
		messages = append(messages, mutationDoneMsg{origin: "detail", gen: 1, kind: "close", resp: &MutationResp{Issue: snapshot.issue}})
		for _, msg := range rapid.Permutation(messages).Draw(t, "completion order") {
			m = m.syncSearchDetailSnapshot(msg)
		}
		saved := m.input.preSplitDetail
		if saved.err != nil || saved.commentsErr != nil || saved.eventsErr != nil || saved.linksErr != nil {
			t.Fatalf("canceled reads polluted saved detail: %v %v %v %v", saved.err, saved.commentsErr, saved.eventsErr, saved.linksErr)
		}
		if saved.status == "" {
			t.Fatal("accepted mutation status was lost")
		}
	})
}

func TestDetailLiveRefreshSettlesEveryCompletionOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := sseDetailFixture(7, "abc4", "01UID-OPEN")
		api := &fakeDetailAPI{}
		if rapid.Bool().Draw(t, "read errors") {
			api.getIssueErr = errors.New("read failed")
			api.commentsErr = api.getIssueErr
			api.eventsErr = api.getIssueErr
			api.linksErr = api.getIssueErr
		}
		batch := m.detail.refetch(api)().(tea.BatchMsg)
		floor := m.detail.reads.floor
		messages := make([]tea.Msg, 0, 4)
		for _, work := range batch {
			messages = append(messages, work())
		}
		order := rapid.Permutation(messages).Draw(t, "completion order")
		dirty, trailing := false, 0
		for _, msg := range order {
			count := rapid.Uint8().Draw(t, "events before completion")
			for range min(int(count), 32) {
				dirty = true
				if m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}) != nil {
					t.Fatal("active batch started overlapping reads")
				}
			}
			if m.detail.reads.floor != floor || m.detail.reads.canceled {
				t.Fatal("events replaced the active batch")
			}
			next, cmd := m.Update(msg)
			m = next.(Model)
			if cmd != nil {
				trailing++
			}
		}
		want := 0
		if dirty {
			want = 1
		}
		if trailing != want {
			t.Fatalf("trailing refreshes=%d, want %d", trailing, want)
		}
		m.detail.cancelReads()
	})
}

func TestDetailBackRestoresFreshReadsAtEveryCompletionBoundary(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prior := dmFixture()
		prior.gen = 1
		api := &fakeDetailAPI{getIssueResult: prior.issue}
		old := prior.refetch(api)().(tea.BatchMsg)
		queued := make([]tea.Msg, 0, 4)
		completed := rapid.IntRange(0, 4).Draw(t, "completed before jump")
		for i := range completed {
			msg := old[i]()
			queued = append(queued, msg)
			prior = prior.applyFetched(msg)
		}
		prior.cancelReads()
		for i := completed; i < 4; i++ {
			queued = append(queued, old[i]())
		}
		freshIssue := *prior.issue
		freshIssue.Title = "freshly restored"
		api.getIssueResult = &freshIssue
		current := newDetailModel()
		current.gen = 2
		current.navStack = []detailModel{cloneDetailModel(prior)}
		restored, cmd := current.Update(tea.KeyPressMsg{Code: tea.KeyEsc}, newKeymap(), api)
		if cmd == nil {
			t.Fatal("restored pane has no fresh reads")
		}
		for _, msg := range rapid.Permutation(queued).Draw(t, "queued order") {
			if _, accepted := restored.applyFetchedIfFresh(msg); accepted {
				t.Fatal("restored pane accepted old lifetime")
			}
		}
		freshCount := 0
		for _, work := range cmd().(tea.BatchMsg) {
			msg := work()
			var accepted bool
			restored, accepted = restored.applyFetchedIfFresh(msg)
			if !accepted {
				t.Fatalf("restored pane rejected fresh response %T", msg)
			}
			freshCount++
		}
		if freshCount != 4 {
			t.Fatalf("fresh detail responses = %d, want 4", freshCount)
		}
		if restored.issue == nil || restored.issue.Title != freshIssue.Title {
			t.Fatalf("restored issue = %+v, want title %q", restored.issue, freshIssue.Title)
		}
		if restored.loading || restored.commentsLoading || restored.eventsLoading || restored.linksLoading || restored.err != nil {
			t.Fatal("restored pane failed to finish fresh reads")
		}
		restored.cancelReads()
	})
}

// countingDetailAPI answers detail reads at once and counts issue fetches, so
// a test can observe whether a refresh reached the daemon.
type countingDetailAPI struct {
	KataAPI
	issueReads int
}

func (a *countingDetailAPI) ReportScreenViewed(context.Context, string) error { return nil }
func (a *countingDetailAPI) GetIssueDetail(context.Context, int64, string) (*IssueDetail, error) {
	a.issueReads++
	return nil, errors.New("not found")
}
func (a *countingDetailAPI) ListComments(context.Context, int64, string) ([]CommentEntry, error) {
	return nil, nil
}
func (a *countingDetailAPI) ListEvents(context.Context, int64, string) ([]EventLogEntry, error) {
	return nil, nil
}
func (a *countingDetailAPI) ListLinks(context.Context, int64, string) ([]LinkEntry, error) {
	return nil, nil
}

// runCmdTree runs cmd and every nested batch, returning the leaf messages.
func runCmdTree(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, inner := range batch {
		out = append(out, runCmdTree(inner)...)
	}
	return out
}

func stackedDetailBehindDaemons(t *testing.T) (Model, *countingDetailAPI) {
	t.Helper()
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.layout = splitlayout.Stacked
	api := &countingDetailAPI{}
	m.api = api
	m.daemonTargets = []daemonTarget{{Name: "local", Local: true}}
	return m, api
}

func leaveDaemonsAndCountReads(t *testing.T, m Model, api *countingDetailAPI) int {
	t.Helper()
	before := api.issueReads
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewDetail, m.view)
	runCmdTree(cmd)
	return api.issueReads - before
}

func TestDetailRefreshTickBehindDaemonSelectorFetchesOnReturn(t *testing.T) {
	m, api := stackedDetailBehindDaemons(t)
	tick := runCmdTree(m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}))
	require.Len(t, tick, 1)
	m, _ = m.transitionToDaemons()
	m, cmd := updateModel(m, tick[0])
	runCmdTree(cmd)
	require.Zero(t, api.issueReads, "hidden detail must not fetch")
	require.Equal(t, 1, leaveDaemonsAndCountReads(t, m, api))
}

func TestDetailDirtyBatchBehindDaemonSelectorFetchesOnReturn(t *testing.T) {
	m, api := stackedDetailBehindDaemons(t)
	next, cmd := m.handleOpenDetail(openDetailMsg{issue: *m.detail.issue})
	m = next.(Model)
	reads := runCmdTree(cmd)
	require.Nil(t, m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}), "active batch should mark dirty")
	m, _ = m.transitionToDaemons()
	for _, msg := range reads {
		m, cmd = updateModel(m, msg)
		runCmdTree(cmd)
	}
	require.Equal(t, 1, api.issueReads, "hidden detail must not fetch its follow-up")
	require.Equal(t, 1, leaveDaemonsAndCountReads(t, m, api))
}

func TestDetailEventBehindDaemonSelectorFetchesOnReturn(t *testing.T) {
	m, api := stackedDetailBehindDaemons(t)
	m, _ = m.transitionToDaemons()
	runCmdTree(m.maybeRefetchOpenDetail(eventReceivedMsg{projectID: 7, issueUID: "01UID-OPEN"}))
	require.Zero(t, api.issueReads)
	require.Equal(t, 1, leaveDaemonsAndCountReads(t, m, api))
}

func TestDetailReturnFromDaemonSelectorWithoutEventsDoesNotFetch(t *testing.T) {
	m, api := stackedDetailBehindDaemons(t)
	m, _ = m.transitionToDaemons()
	require.Zero(t, leaveDaemonsAndCountReads(t, m, api))
}

func (a *countingDetailAPI) ListIssues(context.Context, int64, ListFilter) ([]Issue, error) {
	return nil, nil
}
func (a *countingDetailAPI) ListAllIssues(context.Context, ListFilter) ([]Issue, error) {
	return nil, nil
}

// A link jump has no fetched issue until its first response. Leaving Inbox
// must restart the jump's canceled reads instead of leaving it loading.
func TestDetailJumpPendingAcrossInboxRoundTripRestartsReads(t *testing.T) {
	m, api := stackedDetailBehindDaemons(t)
	next, jumpReads := m.handleJumpDetail(jumpDetailMsg{ref: "def5", projectID: 7, connGen: m.connGen})
	m = next.(Model)
	require.NotNil(t, jumpReads)
	require.Nil(t, m.detail.issue, "jump target is still loading")
	m.inboxPending, m.inboxAttempt = true, 1
	m, _ = updateModel(m, inboxProjectLoadedMsg{connGen: m.connGen, attempt: 1, project: ProjectSummary{ID: 9, Name: "inbox-project"}, found: true})
	require.NotNil(t, m.inboxReturn)
	// The jump's reads finish only after Inbox canceled them.
	for _, msg := range runCmdTree(jumpReads) {
		m, _ = updateModel(m, msg)
	}
	before := api.issueReads
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Nil(t, m.inboxReturn)
	require.Equal(t, viewDetail, m.view)
	restored := runCmdTree(cmd)
	require.Equal(t, 1, api.issueReads-before, "restored loading pane must restart its reads")
	for _, msg := range restored {
		m, _ = updateModel(m, msg)
	}
	require.False(t, m.detail.loading, "restored pane must not stay loading")
}
