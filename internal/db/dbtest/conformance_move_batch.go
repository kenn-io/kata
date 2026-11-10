package dbtest

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Both backends classify this synthetic driver failure as transaction-retryable.
type batchRetryProbe struct{}

func (batchRetryProbe) Error() string    { return "synthetic transient transaction failure" }
func (batchRetryProbe) Code() int        { return 5 } // SQLITE_BUSY
func (batchRetryProbe) SQLState() string { return "40001" }

// Contract: a batch isolates refusals and commits moves and comments together,
// while previews leave both events and access authority unchanged.
func checkMoveBatch(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	src, err := store.CreateProject(ctx, "batch-source")
	require.NoError(t, err)
	a, err := store.CreateProject(ctx, "batch-a")
	require.NoError(t, err)
	require.NoError(t, store.CheckIssueMovePair(ctx, src.ID, a.ID))
	b, err := store.CreateProject(ctx, "batch-b")
	require.NoError(t, err)
	var issues []db.Issue
	for range 3 {
		i, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: src.ID, Title: "route issue", Author: "worker"})
		require.NoError(t, err)
		issues = append(issues, i)
	}
	rec, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: src.ID, Actor: "worker", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned"}})
	require.NoError(t, err)
	mat, err := store.MaterializeNext(ctx, rec.ID, "2026-10-12", "worker")
	require.NoError(t, err)
	pinned, err := store.IssueByID(ctx, mat.NewIssueID)
	require.NoError(t, err)
	issues = append(issues[:1], append([]db.Issue{pinned}, issues[1:]...)...)
	in := db.MoveIssuesBatchIn{Actor: "worker", Comment: "routed", DryRun: true}
	for n, i := range issues {
		target := a.ID
		if n == 3 {
			target = b.ID
		}
		in.Items = append(in.Items, db.MoveIssueProjectIn{IssueID: i.ID, FromProjectID: src.ID, ToProjectID: target, IfMatchRev: i.Revision, Single: true})
	}
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	revision, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	preview, err := store.MoveIssuesBatch(ctx, in)
	require.NoError(t, err)
	require.Len(t, preview.Results, 4)
	require.ErrorAs(t, preview.Results[1].Err, new(*db.RecurrencePinnedError))
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, after)
	access, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, revision, access)
	in.DryRun = false
	out, err := store.MoveIssuesBatch(ctx, in)
	require.NoError(t, err)
	require.Len(t, out.Results, 4)
	require.ErrorAs(t, out.Results[1].Err, new(*db.RecurrencePinnedError))
	for n, i := range issues {
		got, err := store.IssueByID(ctx, i.ID)
		require.NoError(t, err)
		comments, err := store.CommentsByIssue(ctx, i.ID)
		require.NoError(t, err)
		if n == 1 {
			require.Equal(t, src.ID, got.ProjectID)
			require.Empty(t, comments)
			continue
		}
		require.NoError(t, out.Results[n].Err)
		require.Equal(t, in.Items[n].ToProjectID, got.ProjectID)
		require.Len(t, comments, 1)
		require.Equal(t, "routed", comments[0].Body)
		require.NotNil(t, out.Results[n].CommentEvent)
		require.Greater(t, out.Results[n].CommentEvent.ID, out.Results[n].Out.EventID)
	}
	access, err = store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, revision+1, access)
	// Contract: an authorization refusal rolls back only its savepoint, even
	// after admission has checked its source project inside the transaction.
	deniedSource, err := store.CreateProject(ctx, "batch-denied-source")
	require.NoError(t, err)
	denied, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: deniedSource.ID, Title: "denied", Author: "worker"})
	require.NoError(t, err)
	allowed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: src.ID, Title: "allowed", Author: "worker"})
	require.NoError(t, err)
	isolated, err := store.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{
		Actor: "worker", Comment: "routed with authorization",
		Items: []db.MoveIssueProjectIn{
			{IssueID: denied.ID, FromProjectID: deniedSource.ID, ToProjectID: a.ID, IfMatchRev: denied.Revision, Single: true},
			{IssueID: allowed.ID, FromProjectID: src.ID, ToProjectID: a.ID, IfMatchRev: allowed.Revision, Single: true},
		},
		AuthorizeProjects: func(uids []string) error {
			if slices.Contains(uids, deniedSource.UID) {
				return db.ErrNotFound
			}
			return nil
		},
	})
	require.NoError(t, err)
	require.ErrorIs(t, isolated.Results[0].Err, db.ErrNotFound)
	require.NoError(t, isolated.Results[1].Err)
	for _, entry := range []struct {
		issue    db.Issue
		project  int64
		comments int
	}{{denied, deniedSource.ID, 0}, {allowed, a.ID, 1}} {
		got, err := store.IssueByID(ctx, entry.issue.ID)
		require.NoError(t, err)
		require.Equal(t, entry.project, got.ProjectID)
		comments, err := store.CommentsByIssue(ctx, entry.issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, entry.comments)
	}
	// A transient failure after one item wrote retries the complete chunk and
	// exposes only the successful attempt's events and comments.
	retryA, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: src.ID, Title: "retry a", Author: "worker"})
	require.NoError(t, err)
	retryB, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: deniedSource.ID, Title: "retry b", Author: "worker"})
	require.NoError(t, err)
	injected := false
	retried, err := store.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Actor: "worker", Comment: "exactly once", Items: []db.MoveIssueProjectIn{
		{IssueID: retryA.ID, FromProjectID: src.ID, ToProjectID: a.ID, IfMatchRev: retryA.Revision, Single: true},
		{IssueID: retryB.ID, FromProjectID: deniedSource.ID, ToProjectID: a.ID, IfMatchRev: retryB.Revision, Single: true},
	}, AuthorizeProjects: func(uids []string) error {
		for _, uid := range uids {
			if uid == deniedSource.UID && !injected {
				injected = true
				return batchRetryProbe{}
			}
		}
		return nil
	}})
	require.NoError(t, err)
	require.True(t, injected)
	for n, issue := range []db.Issue{retryA, retryB} {
		require.NoError(t, retried.Results[n].Err)
		got, err := store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
		require.Equal(t, a.ID, got.ProjectID)
		require.Equal(t, issue.Revision+1, got.Revision)
		comments, err := store.CommentsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Len(t, retried.Results[n].Out.Events, 2)
	}
	return nil
}

// Contract: allocation in a chunk observes earlier members and target-local
// reservations exactly as sequential moves do.
func checkMoveBatchCollision(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	a, err := store.CreateProject(ctx, "collision-a")
	require.NoError(t, err)
	b, err := store.CreateProject(ctx, "collision-b")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "collision-target")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "reserved", Author: "worker", UID: "01K0000000000000000000ABCD"})
	require.NoError(t, err)
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: a.ID, Title: "first", Author: "worker", UID: "01K0000000000000000011ABCD"})
	require.NoError(t, err)
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: b.ID, Title: "second", Author: "worker", UID: "01K0000000000000000021ABCD"})
	require.NoError(t, err)
	out, err := store.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Actor: "worker", Items: []db.MoveIssueProjectIn{
		{IssueID: first.ID, FromProjectID: a.ID, ToProjectID: target.ID, IfMatchRev: first.Revision, Single: true},
		{IssueID: second.ID, FromProjectID: b.ID, ToProjectID: target.ID, IfMatchRev: second.Revision, Single: true},
	}})
	require.NoError(t, err)
	require.Len(t, out.Results, 2)
	for _, r := range out.Results {
		require.NoError(t, r.Err)
		require.True(t, r.ShortIDExtended)
	}
	require.NotEqual(t, out.Results[0].Out.NewShortID, out.Results[1].Out.NewShortID)
	require.Greater(t, len(out.Results[1].Out.NewShortID), len(out.Results[0].Out.NewShortID))
	return nil
}
