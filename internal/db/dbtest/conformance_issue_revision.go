package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A missing owner revision increment would let a stale metadata CAS succeed
// after an A -> B -> A handoff.
func checkIssueRevisionOwner(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "dispatch work", Author: "worker-a",
	})
	require.NoError(t, err)
	initialRevision := issue.Revision
	a := "worker-a"
	b := "worker-b"

	assigned, event, changed, err := store.UpdateOwner(ctx, issue.ID, &a, a)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, event)
	require.Equal(t, initialRevision+1, assigned.Revision)
	issue = assigned

	_, event, changed, err = store.UpdateOwner(ctx, issue.ID, &a, a)
	require.NoError(t, err)
	require.False(t, changed)
	require.Nil(t, event)
	claim, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: a})
	require.NoError(t, err)
	require.False(t, claim.Changed)
	require.Equal(t, issue.Revision, claim.Issue.Revision)
	_, err = store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: b})
	require.ErrorIs(t, err, db.ErrAlreadyAssigned)

	claim, err = store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: b, Force: true})
	require.NoError(t, err)
	require.True(t, claim.Changed)
	require.Equal(t, issue.Revision+1, claim.Issue.Revision)
	issue = claim.Issue
	claim, err = store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: a, Force: true})
	require.NoError(t, err)
	require.Equal(t, issue.Revision+1, claim.Issue.Revision)
	issue = claim.Issue

	unassigned, event, changed, err := store.UnassignOwner(ctx, issue.ID, a, &a)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, event)
	require.Equal(t, issue.Revision+1, unassigned.Revision)
	issue = unassigned
	claim, err = store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: a, IfUnowned: true})
	require.NoError(t, err)
	require.True(t, claim.Changed)
	require.Equal(t, issue.Revision+1, claim.Issue.Revision)

	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, claim.Issue.Revision, stored.Revision)
	require.Equal(t, a, *stored.Owner)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: a, IfMatchRev: &initialRevision,
		Patch: map[string]jsontext.Value{"work.attention": jsontext.Value(`"ok"`)},
	})
	var conflict *db.RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	require.NotNil(t, conflict)
	require.Equal(t, stored.Revision, conflict.CurrentRevision)
	return nil
}

// An owner edit must invalidate the same issue revision used by metadata CAS,
// even when it is combined with a title or body edit.
func checkIssueRevisionEdits(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "original", Author: "worker-a",
	})
	require.NoError(t, err)
	a, b := "worker-a", "worker-b"
	title := "edited"
	edited, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &title, Owner: &a, Actor: a,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, issue.Revision+1, edited.Revision)
	issue = edited

	body := "body"
	atomic, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Body: &body, Owner: &b, Actor: a,
	})
	require.NoError(t, err)
	require.True(t, atomic.AnyChange)
	require.Equal(t, issue.Revision+1, atomic.Issue.Revision)
	issue = atomic.Issue

	title = "title only"
	edited, _, changed, err = store.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &title, Actor: a,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, issue.Revision, edited.Revision)

	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, edited.Revision, stored.Revision)

	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: a, Owner: &a,
		AddBlocks: []int64{issue.ID + 1_000_000},
	})
	var missingTarget *db.LinkTargetNotFoundError
	require.ErrorAs(t, err, &missingTarget)
	rolledBack, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, stored.Revision, rolledBack.Revision)
	require.Equal(t, b, *rolledBack.Owner)
	return nil
}

// A close followed by reopen leaves the status looking unchanged, but must
// still invalidate a revision captured before either transition.
func checkIssueRevisionStatus(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "dispatch work", Author: "worker-a",
	})
	require.NoError(t, err)
	initialRevision := issue.Revision

	closed, event, changed, err := store.CloseIssue(ctx, issue.ID, "done", "worker-a", "finished", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, event)
	require.Equal(t, "closed", closed.Status)
	require.Equal(t, initialRevision+1, closed.Revision)
	issue = closed

	closed, event, changed, err = store.CloseIssue(ctx, issue.ID, "done", "worker-a", "finished", nil)
	require.NoError(t, err)
	require.False(t, changed)
	require.Nil(t, event)
	require.Equal(t, issue.Revision, closed.Revision)

	reopened, event, changed, err := store.ReopenIssue(ctx, issue.ID, "worker-a")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, event)
	require.Equal(t, "open", reopened.Status)
	require.Equal(t, issue.Revision+1, reopened.Revision)
	issue = reopened

	reopened, event, changed, err = store.ReopenIssue(ctx, issue.ID, "worker-a")
	require.NoError(t, err)
	require.False(t, changed)
	require.Nil(t, event)
	require.Equal(t, issue.Revision, reopened.Revision)

	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "worker-a", IfMatchRev: &initialRevision,
		Patch: map[string]jsontext.Value{"work.attention": jsontext.Value(`"ok"`)},
	})
	var conflict *db.RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	require.NotNil(t, conflict)
	require.Equal(t, issue.Revision, conflict.CurrentRevision)
	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue.Revision, stored.Revision)
	return nil
}

func checkIssueRevisionImport(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)
	t4 := t3.Add(time.Hour)
	t5 := t4.Add(time.Hour)
	a, b, done := "worker-a", "worker-b", "done"
	item := db.ImportItem{
		ExternalID: "work-1", Title: "imported work", Author: a, Owner: &a,
		Status: "open", CreatedAt: t1, UpdatedAt: t1,
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, "example-tracker", "issue", "work-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	initialRevision := issue.Revision

	item.Owner, item.Status, item.ClosedReason, item.ClosedAt, item.UpdatedAt = &b, "closed", &done, &t2, t2
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	issue, err = store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, initialRevision+1, issue.Revision)
	require.Equal(t, b, *issue.Owner)
	require.Equal(t, "closed", issue.Status)

	item.Title, item.UpdatedAt = "title only", t3
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue.Revision, stored.Revision)

	item.Owner, item.UpdatedAt = &a, t4
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	ownerUpdated, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, stored.Revision+1, ownerUpdated.Revision)
	require.Equal(t, a, *ownerUpdated.Owner)

	item.Status, item.ClosedReason, item.ClosedAt, item.UpdatedAt = "open", nil, nil, t5
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	statusUpdated, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, ownerUpdated.Revision+1, statusUpdated.Revision)
	require.Equal(t, "open", statusUpdated.Status)

	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	replayed, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, statusUpdated.Revision, replayed.Revision)
	return nil
}

func checkIssueRevisionAuthorRewrite(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	a, b := "worker-a", "worker-b"
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "dispatch work", Author: a, Owner: &a,
	})
	require.NoError(t, err)
	initialRevision := issue.Revision
	_, err = store.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: project.ID, Actor: a, From: a, To: b,
	})
	require.NoError(t, err)
	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, b, *stored.Owner)
	require.Equal(t, initialRevision+1, stored.Revision)

	_, err = store.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: project.ID, Actor: b, From: b, To: b,
	})
	require.NoError(t, err)
	unchanged, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, stored.Revision, unchanged.Revision)
	return nil
}
