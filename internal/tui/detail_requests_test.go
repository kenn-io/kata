package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
)

type blockedDetailAPI struct {
	KataAPI
	entered         chan context.Context
	returned        chan context.Context
	mutationEntered chan context.Context
	mutationRelease chan struct{}
	mutationResult  *MutationResp
}

func (a *blockedDetailAPI) read(ctx context.Context) error {
	a.entered <- ctx
	<-ctx.Done()
	if a.returned != nil {
		a.returned <- ctx
	}
	return ctx.Err()
}
func (a *blockedDetailAPI) GetIssueDetail(ctx context.Context, _ int64, _ string) (*IssueDetail, error) {
	return nil, a.read(ctx)
}
func (a *blockedDetailAPI) ListComments(ctx context.Context, _ int64, _ string) ([]CommentEntry, error) {
	return nil, a.read(ctx)
}
func (a *blockedDetailAPI) ListEvents(ctx context.Context, _ int64, _ string) ([]EventLogEntry, error) {
	return nil, a.read(ctx)
}
func (a *blockedDetailAPI) ListLinks(ctx context.Context, _ int64, _ string) ([]LinkEntry, error) {
	return nil, a.read(ctx)
}

func (a *blockedDetailAPI) Close(ctx context.Context, _ int64, _, _ string) (*MutationResp, error) {
	a.mutationEntered <- ctx
	select {
	case <-a.mutationRelease:
		return a.mutationResult, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitBlockedReadContexts(t *testing.T, api *blockedDetailAPI) []context.Context {
	t.Helper()
	contexts := make([]context.Context, 0, 4)
	for range 4 {
		select {
		case ctx := <-api.entered:
			contexts = append(contexts, ctx)
		case <-time.After(time.Second):
			t.Fatal("detail read did not start")
		}
	}
	return contexts
}

func waitBlockedReadsReturn(t *testing.T, api *blockedDetailAPI, count int) {
	t.Helper()
	for range count {
		select {
		case ctx := <-api.returned:
			if ctx.Err() != context.Canceled {
				t.Errorf("detail read returned with context error %v, want canceled", ctx.Err())
			}
		case <-time.After(time.Second):
			t.Fatal("canceled detail read did not return")
		}
	}
}

func startBlockedDetail(t *testing.T) (Model, *blockedDetailAPI, []context.Context) {
	t.Helper()
	api := &blockedDetailAPI{
		entered:  make(chan context.Context, 8),
		returned: make(chan context.Context, 8),
	}
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.api = api
	next, cmd := m.handleOpenDetail(openDetailMsg{issue: *m.detail.issue})
	m = next.(Model)
	batch := cmd().(tea.BatchMsg)
	for _, work := range batch {
		go work()
	}
	contexts := waitBlockedReadContexts(t, api)
	reads := m.detail.reads
	t.Cleanup(func() {
		reads.stop()
		waitBlockedReadsReturn(t, api, len(contexts))
	})
	return m, api, contexts
}

func TestDetailReadsCancelWhenOpeningAnotherIssue(t *testing.T) {
	m, _, contexts := startBlockedDetail(t)
	m = m.seedOpenDetail(Issue{ProjectID: 7, ShortID: "def5", UID: "01UID-NEXT"})
	for _, ctx := range contexts {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
	require.Equal(t, "def5", m.detail.issue.ShortID)
}

func TestDetailReadsCancelOnPop(t *testing.T) {
	m, _, contexts := startBlockedDetail(t)
	_, _ = m.Update(popDetailMsg{})
	for _, ctx := range contexts {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
}

func TestDetailReplacementFencesQueuedResultsBeforeNewResponse(t *testing.T) {
	m, _, contexts := startBlockedDetail(t)
	old := m.detail.refetch(m.api)().(tea.BatchMsg)
	// Starting a replacement must fence both errors and successful queued data.
	_ = m.detail.refetch(m.api)
	for _, ctx := range contexts {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
	for _, work := range old {
		msg := work()
		got, accepted := m.detail.applyFetchedIfFresh(msg)
		require.False(t, accepted, "replaced response %T must be discarded", msg)
		require.NoError(t, got.err)
		require.NoError(t, got.commentsErr)
		require.NoError(t, got.eventsErr)
		require.NoError(t, got.linksErr)
	}
}

func TestDetailCanceledQueuedCommandsDoNotCallAPI(t *testing.T) {
	api := &blockedDetailAPI{entered: make(chan context.Context, 8)}
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.api = api
	next, cmd := m.handleOpenDetail(openDetailMsg{issue: *m.detail.issue})
	m = next.(Model)
	m = m.seedOpenDetail(Issue{ProjectID: 7, ShortID: "def5", UID: "01UID-NEXT"})
	done := make(chan struct{})
	go func() {
		for _, work := range cmd().(tea.BatchMsg) {
			work()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled commands entered the API")
	}
	require.Empty(t, api.entered)
}

func TestDetailInitReadsRetainCancellationOwner(t *testing.T) {
	api := &blockedDetailAPI{entered: make(chan context.Context, 8)}
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.api = api
	m = m.seedOpenDetail(*m.detail.issue)
	initBatch := m.Init()().(tea.BatchMsg)
	detailBatch := initBatch[len(initBatch)-1]().(tea.BatchMsg)
	for _, work := range detailBatch {
		go work()
	}
	contexts := make([]context.Context, 0, 4)
	for range 4 {
		select {
		case ctx := <-api.entered:
			contexts = append(contexts, ctx)
		case <-time.After(time.Second):
			t.Fatal("init read did not start")
		}
	}
	m = m.seedOpenDetail(Issue{ProjectID: 7, ShortID: "def5", UID: "01UID-NEXT"})
	for _, ctx := range contexts {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
}

func TestDetailReadsCancelOnQuit(t *testing.T) {
	m, _, contexts := startBlockedDetail(t)
	_, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	for _, ctx := range contexts {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
}

func TestDetailReplacementRejectsQueuedSuccesses(t *testing.T) {
	dm := dmFixture()
	dm.gen = 9
	api := &fakeDetailAPI{}
	oldBatch := dm.refetch(api)().(tea.BatchMsg)
	results := make([]tea.Msg, 0, 4)
	for _, work := range oldBatch {
		results = append(results, work())
	}
	_ = dm.refetch(api)
	for _, msg := range results {
		_, accepted := dm.applyFetchedIfFresh(msg)
		require.False(t, accepted, "queued %T applied", msg)
	}
	dm.cancelReads()
}

func TestDetailReadCancellationDoesNotCancelMutation(t *testing.T) {
	dm := dmFixture()
	api := &blockedDetailAPI{
		entered:         make(chan context.Context, 8),
		returned:        make(chan context.Context, 8),
		mutationEntered: make(chan context.Context, 1),
		mutationRelease: make(chan struct{}),
		mutationResult:  &MutationResp{Issue: dm.issue},
	}
	reads := dm.refetch(api)().(tea.BatchMsg)
	for _, read := range reads {
		go read()
	}
	readContexts := waitBlockedReadContexts(t, api)
	t.Cleanup(func() {
		dm.cancelReads()
		waitBlockedReadsReturn(t, api, len(readContexts))
	})

	write := dm.dispatchClose(api)
	writeDone := make(chan tea.Msg, 1)
	go func() { writeDone <- write() }()
	var mutationCtx context.Context
	select {
	case mutationCtx = <-api.mutationEntered:
	case <-time.After(time.Second):
		t.Fatal("close mutation did not start")
	}

	dm.cancelReads()
	require.NoError(t, mutationCtx.Err(), "canceling detail reads must leave the mutation context active")
	select {
	case msg := <-writeDone:
		t.Fatalf("close mutation completed before its API call was released: %#v", msg)
	default:
	}

	close(api.mutationRelease)
	select {
	case msg := <-writeDone:
		result, ok := msg.(mutationDoneMsg)
		require.True(t, ok, "close result type = %T", msg)
		require.NoError(t, result.err)
		require.NotNil(t, result.resp)
	case <-time.After(time.Second):
		t.Fatal("close mutation did not finish after release")
	}
}

func TestDetailCanceledPaneRestartsWhenSplitRevealsIt(t *testing.T) {
	m, _, _ := startBlockedDetail(t)
	next, _ := m.Update(popDetailMsg{})
	m = next.(Model)
	require.True(t, m.detail.reads.canceled)
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(Model)
	require.Equal(t, splitlayout.Split, m.layout)
	require.NotNil(t, cmd, "revealing a retained canceled pane requires fresh reads")
	require.False(t, m.detail.reads.canceled)
	m.detail.cancelReads()
}

func TestDetailSplitPopKeepsVisibleReads(t *testing.T) {
	m, _, contexts := startBlockedDetail(t)
	m.layout = splitlayout.Split
	defer m.detail.cancelReads()
	_, _ = m.Update(popDetailMsg{})
	for _, ctx := range contexts {
		require.NoError(t, ctx.Err(), "visible split pane must finish its reads")
	}
}

func TestDetailFollowEmptyListClearsCanceledPane(t *testing.T) {
	m, _, contexts := startBlockedDetail(t)
	previous := m.nextDetailFollowGen
	m, cmd := m.scheduleDetailFollow()
	require.Nil(t, cmd)
	require.Nil(t, m.detail.issue, "empty list must clear its obsolete detail pane")
	require.Greater(t, m.nextDetailFollowGen, previous, "empty list must invalidate pending cursor tick")
	for _, ctx := range contexts {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
}
