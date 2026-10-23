package daemon_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// purgeDuringResetCheckStore commits real mutations after a reset lookup has
// completed, before the reader can fetch its event page.
type purgeDuringResetCheckStore struct {
	db.Storage
	afterCheck func()
}

func (s *purgeDuringResetCheckStore) PurgeResetCheck(ctx context.Context, after, projectID int64) (int64, error) {
	reset, err := s.Storage.PurgeResetCheck(ctx, after, projectID)
	if err == nil && s.afterCheck != nil {
		run := s.afterCheck
		s.afterCheck = nil
		run()
	}
	return reset, err
}

func TestMCPEventsReadDoesNotSkipAConcurrentPurge(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "unscoped"
		if scoped {
			name = "scoped"
		}
		t.Run(name, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				f := newEventsFixture(t, store)
				interleaved := &purgeDuringResetCheckStore{Storage: store}
				authority := daemon.NewMCPEventsAuthority(daemon.ServerConfig{DB: interleaved, Auth: config.AuthConfig{Token: "configured", RequireTokenIdentity: true}})
				principal := f.member
				if scoped {
					principal = f.scoped
				}
				auth, err := authority.Reacquire(ctx, principal)
				require.NoError(t, err)
				projects, err := auth.VisibleProjects(ctx, nil)
				require.NoError(t, err)
				before, err := auth.Head(ctx, projects)
				require.NoError(t, err)
				var resetAt int64
				interleaved.afterCheck = func() {
					purged, err := store.PurgeIssue(ctx, f.childIssue.ID, "admin", nil)
					require.NoError(t, err)
					require.NotNil(t, purged.PurgeResetAfterEventID)
					resetAt = *purged.PurgeResetAfterEventID
					_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: f.publicIssue.ID, Title: new("Updated title"), Actor: "coordinator"})
					require.NoError(t, err)
				}
				page, err := auth.Read(ctx, projects, before, 100)
				require.NoError(t, err)
				require.Positive(t, resetAt)
				if !page.Reset {
					require.Less(t, page.Next, resetAt, "a normal page must not settle past a purge it has not reported")
					page, err = auth.Read(ctx, projects, page.Next, 100)
					require.NoError(t, err)
				}
				assert.True(t, page.Reset, "the purge must require an inbox reload")
				assert.GreaterOrEqual(t, page.ResetHead, resetAt)
				assert.Empty(t, page.Occurrences)
			})
		})
	}
}

func TestMCPEventsScopedReadPreservesPageContinuations(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newEventsFixture(t, store)
		auth := f.as(t, f.scoped)
		projects, err := auth.VisibleProjects(ctx, nil)
		require.NoError(t, err)
		start, err := auth.Head(ctx, projects)
		require.NoError(t, err)
		var expected []string
		for i := range 105 {
			_, event, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: f.publicIssue.ID, Title: new(strconv.Itoa(i)), Actor: "coordinator"})
			require.NoError(t, err)
			expected = append(expected, event.UID)
		}
		head, err := auth.Head(ctx, projects)
		require.NoError(t, err)
		// Resume scans request one match; delivery drains request 100 at a time.
		for _, limit := range []int{1, 100} {
			cursor := start
			var received []string
			for cursor < head {
				page, err := auth.Read(ctx, projects, cursor, limit)
				require.NoError(t, err)
				require.False(t, page.Reset, "a bounded ordinary page preserves its backlog")
				require.Greater(t, page.Next, cursor)
				for _, occurrence := range page.Occurrences {
					received = append(received, occurrence.EventUID)
				}
				cursor = page.Next
			}
			assert.Equal(t, expected, received, "every visible event survives pagination with limit %d", limit)
		}
	})
}
