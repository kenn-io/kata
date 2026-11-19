package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

// RunQuietFederationPollGrowth checks that repeating an empty federation poll
// of a quiet hub project costs the same whatever the project's size, while an
// unrelated project keeps writing past the poll's cursor. The caller chooses
// the cost measure and its minimum meaningful value.
func RunQuietFederationPollGrowth(t *testing.T, open func(*testing.T) db.Storage, measure growthtest.Measure, floor float64) {
	t.Helper()
	growthtest.Run(t, growthtest.Check{
		Small: 100, Large: 1000,
		MaxRatio: 2, Floor: floor,
		Measure: measure,
		Build: func(_ testing.TB, size int) (run, reset func()) {
			ctx := t.Context()
			store := open(t)
			quiet, err := store.CreateProject(ctx, "quiet-project")
			require.NoError(t, err)
			busy, err := store.CreateProject(ctx, "busy-project")
			require.NoError(t, err)
			for _, p := range []db.Project{quiet, busy} {
				_, err = store.EnableProjectFederation(ctx, p.ID, "tester")
				require.NoError(t, err)
			}
			busyIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: busy.ID, Title: "Busy", Author: "tester"})
			require.NoError(t, err)
			for range size {
				_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Quiet", Author: "tester"})
				require.NoError(t, err)
			}
			poll := func(after int64) db.FederationReadResult {
				out, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: after, Limit: 1000})
				require.NoError(t, err)
				return out
			}
			var cursor int64
			for page := poll(0); len(page.Events) > 0; page = poll(cursor) {
				cursor = page.NextAfterID
			}
			return func() {
					require.Empty(t, poll(cursor).Events)
				}, func() {
					_, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: busyIssue.ID, Body: "busy", Author: "tester"})
					require.NoError(t, err)
				}
		},
	})
}
