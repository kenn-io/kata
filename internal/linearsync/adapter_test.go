package linearsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
)

type adapterSource struct {
	items       []Issue
	scope       Scope
	states      []State
	writes      int
	statusReads int
	contentErr  error
	statusErr   error
	duringWrite func()
}

func (s *adapterSource) ForRun(ctx context.Context, _ Config) (Session, error) { return s, ctx.Err() }
func (s *adapterSource) Scope(ctx context.Context, _ Config) (Scope, error) {
	return s.scope, ctx.Err()
}
func (s *adapterSource) States(ctx context.Context, _ Config) ([]State, error) {
	return s.states, ctx.Err()
}
func (s *adapterSource) Issues(ctx context.Context, _ Config) ([]Issue, error) {
	if s.contentErr != nil {
		return nil, s.contentErr
	}
	return s.items, ctx.Err()
}
func (s *adapterSource) ReadStatus(_ context.Context, _ Config, id string) (issuesync.StatusObservation, error) {
	s.statusReads++
	if s.statusErr != nil {
		return issuesync.StatusObservation{}, s.statusErr
	}
	for _, i := range s.items {
		if i.ID == id {
			typ := ""
			for _, state := range s.states {
				if state.ID == i.StateID {
					typ = state.Type
					break
				}
			}
			if typ == "" {
				return issuesync.StatusObservation{}, fmt.Errorf("missing workflow state")
			}
			status, reason, at, err := issueStatus(i, typ)
			obs := issuesync.StatusObservation{RawStatus: &i.StateID, Status: status, ClosedAt: at, Version: i.UpdatedAt}
			if reason != nil {
				obs.ClosedReason = *reason
			}
			return obs, err
		}
	}
	return issuesync.StatusObservation{}, fmt.Errorf("missing issue")
}
func (s *adapterSource) WriteStatus(ctx context.Context, c Config, id, desired string, admit func() error) (issuesync.StatusObservation, error) {
	obs, err := s.ReadStatus(ctx, c, id)
	if err != nil || obs.Status == desired {
		return obs, err
	}
	if err := admit(); err != nil {
		return issuesync.StatusObservation{}, err
	}
	s.writes++
	for n := range s.items {
		if s.items[n].ID == id {
			state := stateID
			if desired == "closed" {
				state = closedStateID
			}
			s.items[n].StateID = state
			s.items[n].UpdatedAt = s.items[n].UpdatedAt.Add(time.Minute)
		}
	}
	if s.duringWrite != nil {
		s.duringWrite()
	}
	return s.ReadStatus(ctx, c, id)
}
func newSource() *adapterSource {
	return &adapterSource{scope: Scope{WorkspaceID: workspaceID, TeamID: teamID, Name: "Example team"}, states: []State{{ID: stateID, Type: "unstarted"}, {ID: closedStateID, Type: "completed"}}, items: []Issue{testIssue()}}
}
func adapterStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	s, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}
func adapterBinding(t *testing.T, s db.Storage, c Config) db.IssueSyncBinding {
	t.Helper()
	p, err := s.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	b, err := s.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "linear", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example team", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func mappedIssue(t *testing.T, s db.Storage, b db.IssueSyncBinding, id string) db.Issue {
	t.Helper()
	m, err := s.ImportMappingBySource(context.Background(), b.ProjectID, b.SourceKey, "issue", "issue:"+id)
	require.NoError(t, err)
	i, err := s.IssueByID(context.Background(), *m.IssueID)
	require.NoError(t, err)
	return i
}
func pendingCount(t *testing.T, s *sqlitestore.Store, b db.IssueSyncBinding) int {
	t.Helper()
	var count int
	require.NoError(t, s.QueryRowContext(context.Background(), `SELECT count(*) FROM import_mappings WHERE source=$1 AND pending_event_uid IS NOT NULL`, b.SourceKey).Scan(&count))
	return count
}

func TestRunnerReplayLatestIntentAndContentFailure(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	b := adapterBinding(t, store, statusConfig())
	source := newSource()
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source})
	result, err := runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Created)
	result, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Unchanged)
	require.Zero(t, source.writes)
	issue := mappedIssue(t, store, b, issueID)
	_, _, _, err = store.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "Completed mapped task", nil)
	require.NoError(t, err)
	require.Equal(t, 1, pendingCount(t, store, b))
	source.duringWrite = func() {
		source.duringWrite = nil
		_, _, _, err := store.ReopenIssue(ctx, issue.ID, "worker")
		require.NoError(t, err)
	}
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, pendingCount(t, store, b))
	require.Equal(t, "open", mappedIssue(t, store, b, issueID).Status)
	source.contentErr = fmt.Errorf("content unavailable")
	_, err = NewRunner(RunnerConfig{Store: store, Fetcher: source}).RunOnce(ctx, b.ID)
	require.Error(t, err)
	require.Equal(t, 2, source.writes)
	require.Zero(t, pendingCount(t, store, b))
	require.Equal(t, stateID, source.items[0].StateID)
	source.items[0].StateID = closedStateID
	source.items[0].UpdatedAt = source.items[0].UpdatedAt.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.Error(t, err)
	require.Equal(t, "closed", mappedIssue(t, store, b, issueID).Status)
	require.Zero(t, pendingCount(t, store, b))
	require.Equal(t, 2, source.writes)
}
func TestRunnerRestartBeyondStatusPageAndBlockedMapping(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	b := adapterBinding(t, store, statusConfig())
	source := newSource()
	source.items = nil
	for n := range 101 {
		i := testIssue()
		i.ID = fmt.Sprintf("%08x-1111-4111-8111-111111111111", n+1)
		i.Identifier = fmt.Sprintf("EX-%d", n+1)
		source.items = append(source.items, i)
	}
	_, err := NewRunner(RunnerConfig{Store: store, Fetcher: source}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	for _, row := range source.items {
		i := mappedIssue(t, store, b, row.ID)
		_, _, _, err := store.CloseIssueWithEvents(ctx, i.ID, "done", "worker", "Completed mapped task", nil)
		require.NoError(t, err)
	}
	require.Equal(t, 101, pendingCount(t, store, b))
	for range 3 {
		_, err := NewRunner(RunnerConfig{Store: store, Fetcher: source}).RunOnce(ctx, b.ID)
		require.NoError(t, err)
	}
	require.Zero(t, pendingCount(t, store, b))
	require.Equal(t, 101, source.writes)
	source.statusErr = &issuesync.StatusError{Message: "mapped issue archived", Blocked: true}
	source.contentErr = fmt.Errorf("content unavailable")
	_, err = NewRunner(RunnerConfig{Store: store, Fetcher: source}).RunOnce(ctx, b.ID)
	require.Error(t, err)
	require.Equal(t, "closed", mappedIssue(t, store, b, source.items[0].ID).Status)
	require.Equal(t, 101, source.writes)
}

// Contract: a fixture API can prove import and binary writeback without live credentials.
func TestClientRunnerImportAndStateOnlyWriteback(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	b := adapterBinding(t, store, statusConfig())
	current := stateID
	writes := 0
	version := "2026-01-02T00:00:00Z"
	issue := func() map[string]any {
		i := wireIssue()
		i["state"] = map[string]any{"id": current}
		i["updatedAt"] = version
		return i
	}
	statusAPI := statusTransport(t, func(_ *http.Request, v map[string]any) (*http.Response, error) {
		writes++
		require.Len(t, v["input"].(map[string]any), 1)
		current = v["input"].(map[string]any)["stateId"].(string)
		version = fmt.Sprintf("2026-01-%02dT00:00:00Z", 2+writes)
		return dataResponse(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID}}}), nil
	}, issue)
	client := mockClient(t, func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if bytes.Contains(raw, []byte("KataLinearIssues")) {
			return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{issue()}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
		}
		return statusAPI(r)
	})
	_, err := NewRunner(RunnerConfig{Store: store, Fetcher: client}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	local := mappedIssue(t, store, b, issueID)
	require.Equal(t, "[Linear EX-1] Example task", local.Title)
	_, _, _, err = store.CloseIssueWithEvents(ctx, local.ID, "done", "worker", "Completed mapped task", nil)
	require.NoError(t, err)
	_, err = NewRunner(RunnerConfig{Store: store, Fetcher: client}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, closedStateID, current)
	require.Equal(t, 1, writes)
	require.Zero(t, pendingCount(t, store, b))
	_, err = NewRunner(RunnerConfig{Store: store, Fetcher: client}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, writes)
	_, _, _, err = store.ReopenIssue(ctx, local.ID, "worker")
	require.NoError(t, err)
	_, err = NewRunner(RunnerConfig{Store: store, Fetcher: client}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, stateID, current)
	require.Equal(t, 2, writes)
	require.Zero(t, pendingCount(t, store, b))
}

// Two-way binary observations preserve closure metadata; newer one-way content owns it.
func TestRunnerClosureMetadataFollowsMode(t *testing.T) {
	for _, mode := range []string{"one-way", "two-way"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := adapterStore(t)
			c := testConfig()
			c.StatusSync = mode
			b := adapterBinding(t, store, c)
			source := newSource()
			source.states = append(source.states, State{ID: projectID, Type: "canceled"})
			source.items[0].StateID = closedStateID
			_, err := NewRunner(RunnerConfig{Store: store, Fetcher: source}).RunOnce(ctx, b.ID)
			require.NoError(t, err)
			before := mappedIssue(t, store, b, issueID)
			require.Equal(t, new("done"), before.ClosedReason)
			source.items[0].StateID = projectID
			source.items[0].UpdatedAt = source.items[0].UpdatedAt.Add(time.Minute)
			_, err = NewRunner(RunnerConfig{Store: store, Fetcher: source}).RunOnce(ctx, b.ID)
			require.NoError(t, err)
			after := mappedIssue(t, store, b, issueID)
			if mode == "two-way" {
				require.Equal(t, before.ClosedReason, after.ClosedReason)
				require.Equal(t, before.ClosedAt, after.ClosedAt)
			} else {
				require.Equal(t, new("wontfix"), after.ClosedReason)
				require.Equal(t, new(source.items[0].UpdatedAt), after.ClosedAt)
			}
			require.Zero(t, pendingCount(t, store, b))
		})
	}
}

// Two-way sweeps reuse the previous content read, so each mapped issue does not
// cost its own request against the shared Linear quota.
func TestStatusSweepReusesPreviousContentRead(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	b := adapterBinding(t, store, statusConfig())
	source := newSource()
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source})
	_, err := runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	source.items[0].StateID = closedStateID
	source.items[0].UpdatedAt = source.items[0].UpdatedAt.Add(time.Minute)
	for range 2 {
		_, err = runner.RunOnce(ctx, b.ID)
		require.NoError(t, err)
	}
	require.Equal(t, "closed", mappedIssue(t, store, b, issueID).Status)
	require.Zero(t, source.statusReads)
	// Each content observation serves one sweep; without a newer content read
	// the sweep reads Linear directly.
	source.contentErr = fmt.Errorf("content unavailable")
	_, err = runner.RunOnce(ctx, b.ID)
	require.Error(t, err)
	require.Zero(t, source.statusReads)
	_, err = runner.RunOnce(ctx, b.ID)
	require.Error(t, err)
	require.Equal(t, 1, source.statusReads)
}
