package issuesync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type testAdapter struct {
	prepare func(context.Context, db.IssueSyncBinding, time.Time) (Prepared, error)
}

func (*testAdapter) Provider() string     { return "synthetic" }
func (*testAdapter) InitialPhase() string { return "source" }
func (a *testAdapter) Prepare(ctx context.Context, b db.IssueSyncBinding, at time.Time) (Prepared, error) {
	return a.prepare(ctx, b, at)
}

type statusErrorTestAdapter struct {
	*testAdapter
	statusErr error
}

func (a *statusErrorTestAdapter) OpenStatus(context.Context, db.IssueSyncBinding, time.Time) (StatusRun, error) {
	return nil, a.statusErr
}

func testStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	s, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}
func testBinding(t *testing.T, s db.Storage, name, provider string) db.IssueSyncBinding {
	t.Helper()
	p, err := s.CreateProject(context.Background(), name)
	require.NoError(t, err)
	b, err := s.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: provider, SourceKey: provider + ":" + name, RemoteID: name, DisplayName: name, Config: []byte(`{}`), IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func testPrepared(b db.IssueSyncBinding, at time.Time) Prepared {
	return Prepared{Binding: b, Batch: db.ImportBatchParams{Actor: "sync", Items: []db.ImportItem{{ExternalID: "item-1", Title: "task", Author: "author", Status: "open", CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Minute)}}}}
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runner")
		var zero T
		return zero
	}
}

// A missing provider filter imports the other provider; a missing stable source duplicates replay events.
func TestRunnerAdapterIsolationAndReplay(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, "spoke-project", "synthetic")
	other := testBinding(t, s, "hub-project", "github")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	tracker := NewProgressTracker()
	events := 0
	a := &testAdapter{prepare: func(ctx context.Context, binding db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		require.Equal(t, b.ID, binding.ID)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return Prepared{}, ctx.Err()
		}
		return testPrepared(binding, at), nil
	}}
	r := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }, Progress: tracker, EventSink: func(_ context.Context, project int64, e []db.Event) error {
		require.Equal(t, b.ProjectID, project)
		events += len(e)
		return nil
	}})
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	receive(t, entered)
	require.Equal(t, "source", tracker.Snapshot(b.ID, at).Phase)
	_, err := r.RunOnce(context.Background(), b.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	close(release)
	require.NoError(t, receive(t, done))
	status, err := s.IssueSyncStatusByProject(context.Background(), b.ProjectID)
	require.NoError(t, err)
	require.Equal(t, 1, status.LastCreated)
	untouched, err := s.IssueSyncStatusByProject(context.Background(), other.ProjectID)
	require.NoError(t, err)
	require.Nil(t, untouched.LastAttemptAt)
	eventsAfterFirst := events
	require.Positive(t, eventsAfterFirst)
	replay, err := r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, replay.Import.Unchanged)
	require.Equal(t, eventsAfterFirst, events)
}

func TestRunnerOnceOnlyClassifiesStatusPassBlocksAsWarnings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentErr  error
		wantWarning bool
		wantCreated int
	}{
		{name: "blocked content read is fatal", contentErr: &StatusError{Message: "content read blocked", Blocked: true}},
		{name: "blocked status pass preserves successful content", wantWarning: true, wantCreated: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			b := testBinding(t, s, "example-project", "synthetic")
			at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
			statusErr := &StatusError{Message: "status read blocked", Blocked: true}
			a := &statusErrorTestAdapter{
				testAdapter: &testAdapter{prepare: func(_ context.Context, binding db.IssueSyncBinding, _ time.Time) (Prepared, error) {
					if tc.contentErr != nil {
						return Prepared{}, tc.contentErr
					}
					return testPrepared(binding, at), nil
				}},
				statusErr: statusErr,
			}
			r := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }})

			result, err := r.RunOnce(context.Background(), b.ID)

			if tc.contentErr != nil {
				require.ErrorIs(t, err, tc.contentErr)
				require.ErrorContains(t, err, statusErr.Error())
			} else {
				require.ErrorIs(t, err, statusErr)
			}
			require.Equal(t, tc.wantWarning, IsBlockedStatusWarning(err))
			require.Equal(t, tc.wantCreated, result.Import.Created)
		})
	}
}

// Binding-local deadline failures must not terminate the parent worker or skip other due bindings.
func TestRunnerBindingDeadlineKeepsSchedulerAlive(t *testing.T) {
	s := &completedSuccessStore{Storage: testStore(t), completed: make(chan db.IssueSyncStatus, 4)}
	bad := testBinding(t, s, "spoke-project", "synthetic")
	good := testBinding(t, s, "hub-project", "synthetic")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ticks atomic.Int64
	goodCalls := make(chan struct{}, 4)
	a := &testAdapter{prepare: func(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		if b.ID == bad.ID {
			<-ctx.Done()
			return Prepared{}, ctx.Err()
		}
		goodCalls <- struct{}{}
		return testPrepared(b, at), nil
	}}
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at.Add(time.Duration(ticks.Load()) * time.Minute) }, RunTimeout: time.Second, Interval: time.Hour, Wake: wake, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	receive(t, goodCalls)
	committed := receive(t, s.completed)
	require.Equal(t, good.ProjectID, committed.ProjectID)
	require.Equal(t, good.ID, committed.BindingID)
	require.NotNil(t, committed.LastSuccessAt)
	failed, err := s.IssueSyncBindingByID(context.Background(), bad.ID)
	require.NoError(t, err)
	require.Nil(t, failed.LastCursorAt)
	status, err := s.IssueSyncStatusByProject(context.Background(), bad.ProjectID)
	require.NoError(t, err)
	require.Contains(t, status.LastError, "deadline exceeded")
	select {
	case err := <-done:
		t.Fatalf("scheduler exited while parent live: %v", err)
	default:
	}
	ticks.Store(10)
	wake <- struct{}{}
	receive(t, goodCalls)
	cancel()
	require.ErrorIs(t, receive(t, done), context.Canceled)
}

func TestRunnerClaimFences(t *testing.T) {
	for _, mode := range []string{"disable-reenable", "stale-replacement", "finalizer-failure", "cancel-between-chunks", "import-failure", "stale-finalizer", "identity"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			b := testBinding(t, s, "spoke-project", "synthetic")
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			originalCursor := at.Add(-time.Hour)
			claim := originalCursor.Add(-time.Minute)
			_, ok, err := s.ClaimIssueSyncBinding(context.Background(), b.ID, "synthetic", claim, claim.Add(-time.Hour))
			require.NoError(t, err)
			require.True(t, ok)
			_, err = s.RecordIssueSyncSuccess(context.Background(), db.IssueSyncSuccessParams{BindingID: b.ID, StartedAt: claim, At: originalCursor, CursorAt: originalCursor})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			successor := at.Add(time.Hour)
			finalized := false
			a := &testAdapter{prepare: func(ctx context.Context, b db.IssueSyncBinding, started time.Time) (Prepared, error) {
				p := testPrepared(b, at)
				p.Batch.Items = append(p.Batch.Items, db.ImportItem{ExternalID: "item-2", Title: "second task", Author: "author", Status: "open", CreatedAt: at, UpdatedAt: at})
				p.Finalize = func(ctx context.Context) (db.IssueSyncBinding, error) {
					finalized = true
					if mode == "finalizer-failure" {
						return b, errors.New("finalizer failed")
					}
					return s.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: b.ID, StartedAt: &started, BindingUpdatedAt: new(b.UpdatedAt), DisplayName: "old metadata", Config: []byte(`{}`)})
				}
				switch mode {
				case "disable-reenable":
					_, err := s.DisableIssueSyncBinding(ctx, b.ProjectID)
					require.NoError(t, err)
					_, err = s.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: "new metadata", Config: []byte(`{}`), IntervalSeconds: 300})
					require.NoError(t, err)
				case "stale-replacement":
					_, ok, err := s.ClaimIssueSyncBinding(ctx, b.ID, b.Provider, successor, at.Add(time.Minute))
					require.NoError(t, err)
					require.True(t, ok)
				case "identity":
					p.Binding.SourceKey = "synthetic:wrong"
				}
				return p, nil
			}}
			store := &failureStore{Storage: s}
			if mode == "import-failure" {
				store.failAt = 2
			}
			emitted := 0
			r := NewRunner(RunnerConfig{Store: store, Adapter: a, Clock: func() time.Time { return at }, InitialBatchSize: 1, EventSink: func(_ context.Context, _ int64, e []db.Event) error {
				emitted += len(e)
				if mode == "cancel-between-chunks" {
					cancel()
				}
				if mode == "stale-finalizer" && store.calls == 2 {
					_, ok, err := s.ClaimIssueSyncBinding(context.Background(), b.ID, b.Provider, successor, at.Add(time.Minute))
					require.NoError(t, err)
					require.True(t, ok)
				}
				return nil
			}})
			result, err := r.RunOnce(ctx, b.ID)
			require.Error(t, err)
			switch mode {
			case "disable-reenable":
				require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
			case "stale-replacement", "stale-finalizer":
				require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
			}
			if mode == "cancel-between-chunks" {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, result.Import.Created)
				require.Positive(t, emitted)
			}
			if mode == "finalizer-failure" || mode == "stale-finalizer" {
				require.True(t, finalized)
				require.Equal(t, 2, result.Import.Created)
			} else {
				require.False(t, finalized)
			}
			failedBinding, err := s.IssueSyncBindingByID(context.Background(), b.ID)
			require.NoError(t, err)
			require.Equal(t, &originalCursor, failedBinding.LastCursorAt)
			status, err := s.IssueSyncStatusByProject(context.Background(), b.ProjectID)
			require.NoError(t, err)
			if mode == "stale-replacement" || mode == "stale-finalizer" {
				require.NotNil(t, status.SyncStartedAt)
				require.Equal(t, successor, *status.SyncStartedAt)
				require.NotEqual(t, "old metadata", failedBinding.DisplayName)
			}
		})
	}
}

type failureStore struct {
	db.Storage
	calls, failAt int
}

func (s *failureStore) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	s.calls++
	if s.calls == s.failAt {
		return db.ImportBatchResult{}, nil, errors.New("import failed")
	}
	return s.Storage.ImportBatch(ctx, p)
}

// A bounded finalizer must observe both the binding deadline and parent cancellation.
func TestRunnerBoundedFinalizerContext(t *testing.T) {
	for _, mode := range []string{"deadline", "parent-cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			b := testBinding(t, s, "spoke-project", "synthetic")
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			entered := make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := &testAdapter{prepare: func(_ context.Context, binding db.IssueSyncBinding, _ time.Time) (Prepared, error) {
				p := testPrepared(binding, at)
				p.Finalize = func(ctx context.Context) (db.IssueSyncBinding, error) {
					entered <- struct{}{}
					<-ctx.Done()
					return binding, ctx.Err()
				}
				return p, nil
			}}
			r := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }, RunTimeout: time.Second})
			type outcome struct {
				result RunResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() { result, err := r.RunOnce(ctx, b.ID); done <- outcome{result, err} }()
			receive(t, entered)
			want := context.DeadlineExceeded
			if mode == "parent-cancel" {
				cancel()
				want = context.Canceled
			}
			got := receive(t, done)
			require.ErrorIs(t, got.err, want)
			require.Equal(t, 1, got.result.Import.Created)
			failed, err := s.IssueSyncBindingByID(context.Background(), b.ID)
			require.NoError(t, err)
			require.Nil(t, failed.LastCursorAt)
			require.Nil(t, got.result.Status.SyncStartedAt)
		})
	}
}

// Starting success cleanup before a long bounded finalizer falsely fails a completed import.
func TestRunnerBoundedFinalizerSuccessAfterCleanupWindow(t *testing.T) {
	for _, mode := range []string{"success", "remaining-run-deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testStore(t)
				b := testBinding(t, s, "spoke-project", "synthetic")
				at := time.Now().UTC().Truncate(time.Millisecond)
				a := &testAdapter{prepare: func(_ context.Context, binding db.IssueSyncBinding, _ time.Time) (Prepared, error) {
					p := testPrepared(binding, at)
					p.Finalize = func(ctx context.Context) (db.IssueSyncBinding, error) {
						select {
						case <-ctx.Done():
							return binding, ctx.Err()
						case <-time.After(11 * time.Second):
							return binding, nil
						}
					}
					return p, nil
				}}
				var store db.Storage = s
				runTimeout := 25 * time.Minute
				if mode == "remaining-run-deadline" {
					runTimeout = 12 * time.Second
					store = &delayedSuccessStore{Storage: s, delay: 2 * time.Second}
				}
				r := NewRunner(RunnerConfig{Store: store, Adapter: a, Clock: time.Now, RunTimeout: runTimeout})
				result, err := r.RunOnce(context.Background(), b.ID)
				require.Equal(t, 1, result.Import.Created)
				binding, readErr := s.IssueSyncBindingByID(context.Background(), b.ID)
				require.NoError(t, readErr)
				if mode == "remaining-run-deadline" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Nil(t, binding.LastCursorAt)
					return
				}
				require.NoError(t, err)
				require.Equal(t, &at, binding.LastCursorAt)
				require.Equal(t, &at, result.Binding.LastCursorAt)
				require.NotNil(t, result.Status.LastSuccessAt)
				require.Nil(t, result.Status.LastErrorAt)
				require.Nil(t, result.Status.SyncStartedAt)
			})
		})
	}
}

type delayedSuccessStore struct {
	db.Storage
	delay time.Duration
}

func (s *delayedSuccessStore) RecordIssueSyncSuccess(ctx context.Context, p db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	select {
	case <-ctx.Done():
		return db.IssueSyncStatus{}, ctx.Err()
	case <-time.After(s.delay):
		return s.Storage.RecordIssueSyncSuccess(ctx, p)
	}
}

// completedSuccessStore signals only after the real success record commits.
type completedSuccessStore struct {
	db.Storage
	completed chan db.IssueSyncStatus
}

func (s *completedSuccessStore) RecordIssueSyncSuccess(ctx context.Context, params db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	status, err := s.Storage.RecordIssueSyncSuccess(ctx, params)
	if err == nil {
		s.completed <- status
	}
	return status, err
}

func TestRunnerFailureLogsProvider(t *testing.T) {
	s := testStore(t)
	testBinding(t, s, "example-project", "synthetic")
	var logs bytes.Buffer
	r := NewRunner(RunnerConfig{Store: s, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Adapter: &testAdapter{prepare: func(context.Context, db.IssueSyncBinding, time.Time) (Prepared, error) {
		return Prepared{}, errors.New("upstream unavailable")
	}}})
	require.Error(t, r.Run(t.Context()))
	require.Contains(t, logs.String(), "issue sync binding failed")
	require.Contains(t, logs.String(), "provider=synthetic")
}
