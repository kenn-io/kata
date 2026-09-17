package daemon

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Status changes must take effect without refreshing the embedding index.
func TestSearchStatusTracksCloseAndReopenWithoutReembedding(t *testing.T) {
	ctx := t.Context()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "login race", Body: "x", Author: "a",
	})
	require.NoError(t, err)
	_, err = store.AddLabel(ctx, issue.ID, "bug", "a")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "unlabeled login race", Body: "x", Author: "a",
	})
	require.NoError(t, err)
	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

	for _, state := range []string{"open", "closed", "reopened"} {
		t.Run(state, func(t *testing.T) {
			current := state
			switch state {
			case "closed":
				_, _, changed, err := store.CloseIssue(ctx, issue.ID, "done", "a", "", nil)
				require.NoError(t, err)
				require.True(t, changed)
			case "reopened":
				current = "open"
				_, _, changed, err := store.ReopenIssue(ctx, issue.ID, "a")
				require.NoError(t, err)
				require.True(t, changed)
			}
			for _, mode := range []string{"semantic", "hybrid", "lexical", "auto"} {
				for _, status := range []string{"open", "closed", ""} {
					res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
						ProjectID: project.ID, Query: "login race", Limit: 10, Requested: mode,
						Status: status, Labels: []string{"bug"},
					})
					require.NoError(t, err, "mode=%s status=%q", mode, status)
					require.False(t, res.Degraded)
					if status != "" && status != current {
						require.Empty(t, res.Hits)
						continue
					}
					require.Len(t, res.Hits, 1)
					require.Equal(t, issue.UID, res.Hits[0].Issue.UID)
					require.Equal(t, current, res.Hits[0].Issue.Status)
					if mode == "hybrid" {
						require.Contains(t, res.Hits[0].MatchedIn, "semantic")
						require.Contains(t, res.Hits[0].MatchedIn, "title")
					}
				}
			}
		})
	}
}

// A status-only filter must trigger deep retrieval and report an exhausted
// candidate budget just as a label filter does.
func TestSearchStatusCandidateLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		closed  int
		bounded bool
	}{
		{name: "deep retry", closed: fetchCap + 1},
		{name: "exact ceiling", closed: knnDeepLimit - 1},
		{name: "beyond ceiling", closed: knnDeepLimit + 1, bounded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := newReconcilerTestStore(t)
			project, err := store.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: "target issue", Body: "x", Author: "a",
			})
			require.NoError(t, err)
			for i := range tc.closed {
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: project.ID, Title: fmt.Sprintf("distractor %d", i), Body: "x", Author: "a",
				})
				require.NoError(t, err)
				_, _, _, err = store.CloseIssue(ctx, issue.ID, "done", "a", "", nil)
				require.NoError(t, err)
			}
			idx := openTestVectorIndex(t)
			fillGeneration(ctx, t, store, idx, labelAxisEmbedClient(t))
			emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})
			for _, mode := range []string{"semantic", "hybrid", "auto"} {
				res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
					ProjectID: project.ID, Query: "semantic query", Limit: 10, Requested: mode, Status: "open",
				})
				if tc.bounded && mode != "auto" {
					var modeErr *modeError
					require.ErrorAs(t, err, &modeErr)
					require.Equal(t, http.StatusServiceUnavailable, modeErr.Status())
					continue
				}
				require.NoError(t, err)
				require.Equal(t, tc.bounded, res.Degraded)
				if tc.bounded {
					require.Empty(t, res.Hits)
					require.Equal(t, filterCeilingReason, res.DegradedReason)
				} else {
					require.Len(t, res.Hits, 1)
					require.Equal(t, target.UID, res.Hits[0].Issue.UID)
				}
			}
		})
	}
}
