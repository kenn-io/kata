package issuesync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type interruptedCheckpointStore struct {
	*sqlitestore.Store
	beforeCheckpoint func(db.IssueStatusScanState)
}

func (s *interruptedCheckpointStore) UpdateIssueStatusScan(ctx context.Context, guard db.IssueSyncImportGuard, state db.IssueStatusScanState) (db.IssueSyncBinding, error) {
	s.beforeCheckpoint(state)
	return s.Store.UpdateIssueStatusScan(ctx, guard, state)
}

func TestStatusRunnerInterruptedAttemptResumesAtNextMapping(t *testing.T) {
	for _, boundary := range []string{"provider", "checkpoint"} {
		for _, pending := range []bool{false, true} {
			name := "observation"
			if pending {
				name = "pending"
			}
			t.Run(name+"/"+boundary, func(t *testing.T) {
				s, b, mappings, at := statusFixture(t, 3)
				if pending {
					for _, mapping := range mappings {
						_, _, _, err := s.CloseIssue(t.Context(), *mapping.Mapping.IssueID, "done", "worker", "Completed task", nil)
						require.NoError(t, err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var visited []int64
				interrupt := true
				attempt := func(m db.IssueStatusMapping) (StatusObservation, error) {
					visited = append(visited, m.Mapping.ID)
					if interrupt && boundary == "provider" {
						cancel()
						return StatusObservation{}, context.Canceled
					}
					state := "open"
					if pending {
						state = "closed"
					}
					return observed(state, at), nil
				}
				run := &statusTestRun{
					read: func(_ context.Context, m db.IssueStatusMapping) (StatusObservation, error) { return attempt(m) },
					write: func(_ context.Context, m db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
						require.NoError(t, admit())
						return attempt(m)
					},
				}
				var store db.Storage = s
				if boundary == "checkpoint" {
					store = &interruptedCheckpointStore{Store: s, beforeCheckpoint: func(state db.IssueStatusScanState) {
						if interrupt && (state.Pending.After != 0 || state.Sweep.After != 0) {
							cancel()
						}
					}}
				}
				config := RunnerConfig{Store: store, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}
				_, err := NewRunner(config).RunOnce(ctx, b.ID)
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, []int64{mappings[0].Mapping.ID}, visited)
				binding, err := s.IssueSyncBindingByID(t.Context(), b.ID)
				require.NoError(t, err)
				scan, err := db.DecodeIssueStatusScan(binding.Config)
				require.NoError(t, err)
				cursor := scan.Sweep
				if pending {
					cursor = scan.Pending
				}
				require.Equal(t, mappings[0].Mapping.ID, cursor.After, "only the attempted mapping advances, even when its context was canceled")
				require.Equal(t, mappings[2].Mapping.ID, cursor.Through)
				interrupt = false
				visited = nil
				_, err = NewRunner(config).RunOnce(t.Context(), b.ID)
				require.NoError(t, err)
				require.GreaterOrEqual(t, len(visited), 2)
				require.Equal(t, mappings[1].Mapping.ID, visited[0], "a fresh runner resumes beyond the interrupted prefix")
				require.Equal(t, mappings[2].Mapping.ID, visited[1])
			})
		}
	}
}
