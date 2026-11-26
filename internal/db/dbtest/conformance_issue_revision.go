package dbtest

import (
	"encoding/json/jsontext"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A missing owner revision increment would let a stale metadata CAS succeed
// after an A -> B -> A handoff.
func checkIssueRevisionOwner(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
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
	ctx := t.Context()
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
	ctx := t.Context()
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
	ctx := t.Context()
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

	prio1 := int64(1)
	t6 := t5.Add(time.Hour)
	item.Priority, item.UpdatedAt = &prio1, t6
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	prioUpdated, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, statusUpdated.Revision+1, prioUpdated.Revision)
	require.NotNil(t, prioUpdated.Priority)
	require.Equal(t, int64(1), *prioUpdated.Priority)

	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	replayed, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, prioUpdated.Revision, replayed.Revision)

	t7 := t6.Add(time.Hour)
	item.Labels, item.UpdatedAt = []string{"imported-tag"}, t7
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	labelUpdated, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, replayed.Revision+1, labelUpdated.Revision)

	peerItem := db.ImportItem{
		ExternalID: "work-2", Title: "imported peer", Author: a,
		Status: "open", CreatedAt: t1, UpdatedAt: t1,
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{peerItem},
	})
	require.NoError(t, err)
	peerMapping, err := store.ImportMappingBySource(ctx, project.ID, "example-tracker", "issue", "work-2")
	require.NoError(t, err)
	require.NotNil(t, peerMapping.IssueID)
	peerBefore, err := store.IssueByID(ctx, *peerMapping.IssueID)
	require.NoError(t, err)

	t8 := t7.Add(time.Hour)
	item.Links = []db.ImportLink{{Type: "blocks", TargetExternalID: "work-2"}}
	item.UpdatedAt = t8
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	linkUpdated, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	peerAfter, err := store.IssueByID(ctx, peerBefore.ID)
	require.NoError(t, err)
	require.Equal(t, labelUpdated.Revision+1, linkUpdated.Revision)
	require.Equal(t, peerBefore.Revision+1, peerAfter.Revision)

	// Replaying identical labels and links must not bump revision on either endpoint.
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "example-tracker", Actor: a, Items: []db.ImportItem{item},
	})
	require.NoError(t, err)
	linkReplayed, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	peerReplayed, err := store.IssueByID(ctx, peerBefore.ID)
	require.NoError(t, err)
	require.Equal(t, linkUpdated.Revision, linkReplayed.Revision)
	require.Equal(t, peerAfter.Revision, peerReplayed.Revision)
	return nil
}

func checkIssueRevisionAuthorRewrite(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
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

// An edit that names the content revision it was drafted against is refused
// when a title or body edit has landed since, and a refused edit writes
// nothing: not the body, not the priority or link changes sent with it.
func checkEditExpectedContentRevision(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "original", Body: "first body", Author: "worker-a",
	})
	require.NoError(t, err)
	opened := issue.ContentRevision
	require.Zero(t, opened, "a created issue starts at content revision 0, so the guard must tell 0 from absent")

	first := "first editor's body"
	result, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-a", Body: &first, ExpectedContentRevision: &opened,
	})
	require.NoError(t, err)
	require.True(t, result.AnyChange)
	require.Equal(t, opened+1, result.Issue.ContentRevision)

	eventsBefore, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)

	second := "second editor's body"
	priority, other := int64(1), "worker-b"
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-b", Body: &second, Owner: &other, SetPriority: &priority,
		ExpectedContentRevision: &opened,
	})
	var conflict *db.ContentRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale edit must fail with a content revision conflict, got %v", err)
	}
	require.Equal(t, opened+1, conflict.CurrentContentRevision)

	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, first, stored.Body)
	require.Nil(t, stored.Priority)
	require.Nil(t, stored.Owner)
	require.Equal(t, result.Issue.Revision, stored.Revision)
	require.Equal(t, opened+1, stored.ContentRevision)
	eventsAfter, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, eventsAfter, len(eventsBefore), "a refused edit appends no event")

	// A stale value also refuses an edit that touches no title or body.
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-b", SetPriority: &priority, ExpectedContentRevision: &opened,
	})
	require.ErrorAs(t, err, &conflict)

	// Owner and priority edits leave the content revision alone, so the
	// current value keeps working across them.
	current := opened + 1
	result, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-b", Owner: &other, SetPriority: &priority,
		ExpectedContentRevision: &current,
	})
	require.NoError(t, err)
	require.Equal(t, current, result.Issue.ContentRevision)

	// An edit that names no revision stays unconditional.
	third := "third editor's body"
	result, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-c", Body: &third,
	})
	require.NoError(t, err)
	require.Equal(t, third, result.Issue.Body)
	require.Equal(t, current+1, result.Issue.ContentRevision)
	return nil
}

func checkIssueRevisionPriority(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "prioritized work", Author: "worker-a",
	})
	require.NoError(t, err)

	p2 := int64(2)
	stale := issue.Revision + 10
	_, _, _, err = store.UpdatePriorityGuarded(ctx, db.UpdatePriorityParams{
		IssueID: issue.ID, Priority: &p2, Actor: "worker-a", IfMatchRev: &stale,
	})
	var revConflict *db.RevisionConflictError
	require.ErrorAs(t, err, &revConflict)
	require.NotNil(t, revConflict)
	require.Equal(t, issue.Revision, revConflict.CurrentRevision)

	// Stale guard rejects even a no-op clear (nil -> nil).
	_, _, _, err = store.UpdatePriorityGuarded(ctx, db.UpdatePriorityParams{
		IssueID: issue.ID, Priority: nil, Actor: "worker-a", IfMatchRev: &stale,
	})
	require.ErrorAs(t, err, &revConflict)

	updated, ev, changed, err := store.UpdatePriorityGuarded(ctx, db.UpdatePriorityParams{
		IssueID: issue.ID, Priority: &p2, Actor: "worker-a", IfMatchRev: &issue.Revision,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, ev)
	require.Equal(t, issue.Revision+1, updated.Revision)
	issue = updated

	// Matching guard on no-op does not bump revision.
	unchanged, ev, changed, err := store.UpdatePriorityGuarded(ctx, db.UpdatePriorityParams{
		IssueID: issue.ID, Priority: &p2, Actor: "worker-a", IfMatchRev: &issue.Revision,
	})
	require.NoError(t, err)
	require.False(t, changed)
	require.Nil(t, ev)
	require.Equal(t, issue.Revision, unchanged.Revision)

	// Atomic edit priority clear advances revision; combined owner+priority+link advances once.
	cleared, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-a", ClearPriority: true, IfMatchRev: &issue.Revision,
	})
	require.NoError(t, err)
	require.True(t, cleared.AnyChange)
	require.Nil(t, cleared.Issue.Priority)
	require.Equal(t, issue.Revision+1, cleared.Issue.Revision)
	issue = cleared.Issue

	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "peer", Author: "worker-a",
	})
	require.NoError(t, err)
	owner := "worker-b"
	p1 := int64(1)
	combined, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "worker-a", Owner: &owner, SetPriority: &p1,
		AddBlocks: []int64{peer.ID}, IfMatchRev: &issue.Revision,
	})
	require.NoError(t, err)
	require.True(t, combined.AnyChange)
	require.Equal(t, issue.Revision+1, combined.Issue.Revision)
	issue = combined.Issue

	// Concurrent EditIssueAtomic priority writes with the same IfMatchRev: exactly one wins.
	start := make(chan struct{})
	type outcome struct {
		prio int64
		err  error
	}
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, target := range []int64{0, 4} {
		go func(val int64) {
			ready.Done()
			<-start
			v := val
			_, editErr := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
				IssueID: issue.ID, Actor: "worker-race", SetPriority: &v, IfMatchRev: &issue.Revision,
			})
			results <- outcome{prio: val, err: editErr}
		}(target)
	}
	ready.Wait()
	close(start)

	wins, conflicts := 0, 0
	var winnerPrio int64
	for range 2 {
		res := <-results
		if res.err == nil {
			wins++
			winnerPrio = res.prio
			continue
		}
		var conflict *db.RevisionConflictError
		require.ErrorAs(t, res.err, &conflict)
		conflicts++
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 1, conflicts)
	finalIssue, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NotNil(t, finalIssue.Priority)
	require.Equal(t, winnerPrio, *finalIssue.Priority)
	require.Equal(t, issue.Revision+1, finalIssue.Revision)
	return nil
}

func checkIssueRevisionLabels(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "labeled work", Author: "worker-a",
	})
	require.NoError(t, err)

	stale := issue.Revision + 7
	var conflict *db.RevisionConflictError

	_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "review-ready", Actor: "worker-a", IfMatchRev: &stale,
	})
	require.ErrorAs(t, err, &conflict)
	require.NotNil(t, conflict)
	require.Equal(t, issue.Revision, conflict.CurrentRevision)

	_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "review-ready", Actor: "worker-a", IfMatchRev: &issue.Revision,
	})
	require.NoError(t, err)
	afterAdd, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue.Revision+1, afterAdd.Revision)

	// Duplicate add with stale revision must fail with RevisionConflictError (not ErrLabelExists).
	_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "review-ready", Actor: "worker-a", IfMatchRev: &issue.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	// Duplicate add with matching revision returns ErrLabelExists and leaves revision unchanged.
	_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "review-ready", Actor: "worker-a", IfMatchRev: &afterAdd.Revision,
	})
	require.ErrorIs(t, err, db.ErrLabelExists)
	afterNoOpAdd, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, afterAdd.Revision, afterNoOpAdd.Revision)

	// Batch change with stale revision fails without writing.
	_, _, err = store.ChangeLabelsAndEvents(ctx, issue.ID, db.ChangeLabelsParams{
		Add: []string{"next"}, Remove: []string{"review-ready"}, Actor: "worker-a", IfMatchRev: &issue.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	// Batch no-op with matching revision leaves revision unchanged.
	labels, events, err := store.ChangeLabelsAndEvents(ctx, issue.ID, db.ChangeLabelsParams{
		Add: []string{"review-ready"}, Remove: []string{"absent"}, Actor: "worker-a", IfMatchRev: &afterAdd.Revision,
	})
	require.NoError(t, err)
	require.Empty(t, events)
	require.Equal(t, []string{"review-ready"}, labels)
	afterNoOpBatch, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, afterAdd.Revision, afterNoOpBatch.Revision)

	// Batch real change advances revision once even when multiple labels change.
	labels, events, err = store.ChangeLabelsAndEvents(ctx, issue.ID, db.ChangeLabelsParams{
		Add: []string{"triaged", "queued"}, Remove: []string{"review-ready"}, Actor: "worker-a", IfMatchRev: &afterAdd.Revision,
	})
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, []string{"queued", "triaged"}, labels)
	afterBatch, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, afterAdd.Revision+1, afterBatch.Revision)

	// Remove absent label with stale revision fails with RevisionConflictError (not ErrNotFound).
	_, err = store.RemoveLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.unlabeled", Label: "never-there", Actor: "worker-a", IfMatchRev: &afterAdd.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	// Remove present label advances revision.
	_, err = store.RemoveLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.unlabeled", Label: "queued", Actor: "worker-a", IfMatchRev: &afterBatch.Revision,
	})
	require.NoError(t, err)
	afterRemove, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, afterBatch.Revision+1, afterRemove.Revision)
	return nil
}

func checkIssueRevisionLinks(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	a, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "a", Author: "worker"})
	require.NoError(t, err)
	b, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "b", Author: "worker"})
	require.NoError(t, err)
	c, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "c", Author: "worker"})
	require.NoError(t, err)

	stale := a.Revision + 9
	var conflict *db.RevisionConflictError

	// Stale CreateLinkAndEvent writes nothing.
	_, _, err = store.CreateLinkAndEvent(ctx,
		db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: b.ID, Type: "parent", Author: "worker"},
		db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: a.ID,
			FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID,
			Actor: "worker", IfMatchRev: &stale,
		})
	require.ErrorAs(t, err, &conflict)

	// Matching CreateLinkAndEvent advances both endpoints.
	parentLink, _, err := store.CreateLinkAndEvent(ctx,
		db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: b.ID, Type: "parent", Author: "worker"},
		db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: a.ID,
			FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID,
			Actor: "worker", IfMatchRev: &a.Revision,
		})
	require.NoError(t, err)
	a1, err := store.IssueByID(ctx, a.ID)
	require.NoError(t, err)
	b1, err := store.IssueByID(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, a.Revision+1, a1.Revision)
	require.Equal(t, b.Revision+1, b1.Revision)

	// ReplaceParentAndEvents with stale revision fails and leaves parent untouched.
	replacer, ok := store.(db.ParentLinkReplacementStorage)
	require.True(t, ok)
	_, err = replacer.ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
		ExpectedParentLinkID:  parentLink.ID,
		ExpectedParentIssueID: b.ID,
		Link:                  db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: c.ID, Type: "parent", Author: "worker"},
		UnlinkEvent:           db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: a.ID, FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID, Actor: "worker"},
		LinkEvent:             db.LinkEventParams{EventType: "issue.linked", EventIssueID: a.ID, FromShortID: a.ShortID, FromUID: a.UID, ToShortID: c.ShortID, ToUID: c.UID, Actor: "worker"},
		IfMatchRev:            &a.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	// ReplaceParentAndEvents with current revision advances child, old parent, and new parent.
	replaced, err := replacer.ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
		ExpectedParentLinkID:  parentLink.ID,
		ExpectedParentIssueID: b.ID,
		Link:                  db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: c.ID, Type: "parent", Author: "worker"},
		UnlinkEvent:           db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: a.ID, FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID, Actor: "worker"},
		LinkEvent:             db.LinkEventParams{EventType: "issue.linked", EventIssueID: a.ID, FromShortID: a.ShortID, FromUID: a.UID, ToShortID: c.ShortID, ToUID: c.UID, Actor: "worker"},
		IfMatchRev:            &a1.Revision,
	})
	require.NoError(t, err)
	a2, err := store.IssueByID(ctx, a.ID)
	require.NoError(t, err)
	b2, err := store.IssueByID(ctx, b.ID)
	require.NoError(t, err)
	c1, err := store.IssueByID(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, a1.Revision+1, a2.Revision)
	require.Equal(t, b1.Revision+1, b2.Revision)
	require.Equal(t, c.Revision+1, c1.Revision)

	// DeleteLinkAndEvent with stale revision fails and leaves link intact.
	_, err = store.DeleteLinkAndEvent(ctx, replaced.Link, db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: a.ID,
		FromShortID: a.ShortID, FromUID: a.UID, ToShortID: c.ShortID, ToUID: c.UID,
		Actor: "worker", IfMatchRev: &a1.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	// DeleteLinkAndEvent with current revision advances both endpoints.
	_, err = store.DeleteLinkAndEvent(ctx, replaced.Link, db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: a.ID,
		FromShortID: a.ShortID, FromUID: a.UID, ToShortID: c.ShortID, ToUID: c.UID,
		Actor: "worker", IfMatchRev: &a2.Revision,
	})
	require.NoError(t, err)
	a3, err := store.IssueByID(ctx, a.ID)
	require.NoError(t, err)
	c2, err := store.IssueByID(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, a2.Revision+1, a3.Revision)
	require.Equal(t, c1.Revision+1, c2.Revision)

	// No-op EditIssueAtomic link removal leaves both endpoints unchanged.
	noOp, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: a.ID, Actor: "worker", RemoveBlocks: []int64{b.ID}, IfMatchRev: &a3.Revision,
	})
	require.NoError(t, err)
	require.False(t, noOp.AnyChange)
	require.Equal(t, a3.Revision, noOp.Issue.Revision)
	bCheck, err := store.IssueByID(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, b2.Revision, bCheck.Revision)

	// Concurrent opposing blocks links (a->b and b->a) complete cleanly without deadlocks.
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		_, _, linkErr := store.CreateLinkAndEvent(ctx,
			db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "worker"},
			db.LinkEventParams{EventType: "issue.linked", EventIssueID: a.ID, FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID, Actor: "worker"})
		errs <- linkErr
	}()
	go func() {
		ready.Done()
		<-start
		_, _, linkErr := store.CreateLinkAndEvent(ctx,
			db.CreateLinkParams{FromIssueID: b.ID, ToIssueID: a.ID, Type: "blocks", Author: "worker"},
			db.LinkEventParams{EventType: "issue.linked", EventIssueID: b.ID, FromShortID: b.ShortID, FromUID: b.UID, ToShortID: a.ShortID, ToUID: a.UID, Actor: "worker"})
		errs <- linkErr
	}()
	ready.Wait()
	close(start)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	a4, err := store.IssueByID(ctx, a.ID)
	require.NoError(t, err)
	b4, err := store.IssueByID(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, a3.Revision+2, a4.Revision)
	require.Equal(t, b2.Revision+2, b4.Revision)
	return nil
}

func checkIssueRevisionGuardedLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "guarded", Author: "worker-a",
	})
	require.NoError(t, err)

	stale := issue.Revision + 5
	var conflict *db.RevisionConflictError

	// Stale ReopenIssueGuarded fails even when already open.
	_, _, _, err = store.ReopenIssueGuarded(ctx, db.ReopenIssueParams{
		IssueID: issue.ID, Actor: "worker-a", IfMatchRev: &stale,
	})
	require.ErrorAs(t, err, &conflict)

	closed, _, changed, err := store.CloseIssue(ctx, issue.ID, "done", "worker-a", "done", nil)
	require.NoError(t, err)
	require.True(t, changed)

	_, _, _, err = store.ReopenIssueGuarded(ctx, db.ReopenIssueParams{
		IssueID: issue.ID, Actor: "worker-a", IfMatchRev: &issue.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	reopened, _, changed, err := store.ReopenIssueGuarded(ctx, db.ReopenIssueParams{
		IssueID: issue.ID, Actor: "worker-a", IfMatchRev: &closed.Revision,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, closed.Revision+1, reopened.Revision)

	alice := "alice"
	_, _, _, err = store.UpdateOwnerGuarded(ctx, db.UpdateOwnerParams{
		IssueID: issue.ID, Owner: &alice, Actor: "worker-a", IfMatchRev: &closed.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	assigned, _, changed, err := store.UpdateOwnerGuarded(ctx, db.UpdateOwnerParams{
		IssueID: issue.ID, Owner: &alice, Actor: "worker-a", IfMatchRev: &reopened.Revision,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, reopened.Revision+1, assigned.Revision)

	// Stale IfMatchRev beats ExpectedOwner mismatch.
	bob := "bob"
	_, _, _, err = store.UpdateOwnerGuarded(ctx, db.UpdateOwnerParams{
		IssueID: issue.ID, Actor: "worker-a", ExpectedOwner: &bob, IfMatchRev: &reopened.Revision,
	})
	require.ErrorAs(t, err, &conflict)

	// Current IfMatchRev still enforces ExpectedOwner.
	_, _, _, err = store.UpdateOwnerGuarded(ctx, db.UpdateOwnerParams{
		IssueID: issue.ID, Actor: "worker-a", ExpectedOwner: &bob, IfMatchRev: &assigned.Revision,
	})
	require.ErrorIs(t, err, db.ErrOwnerMismatch)

	unassigned, _, changed, err := store.UpdateOwnerGuarded(ctx, db.UpdateOwnerParams{
		IssueID: issue.ID, Actor: "worker-a", ExpectedOwner: &alice, IfMatchRev: &assigned.Revision,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Nil(t, unassigned.Owner)
	require.Equal(t, assigned.Revision+1, unassigned.Revision)
	return nil
}

func checkIssueRevisionFederationParity(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: project.UID,
		Enabled: true,
	})
	require.NoError(t, err)

	origin, err := uid.New()
	require.NoError(t, err)
	uidA, err := uid.New()
	require.NoError(t, err)
	uidB, err := uid.New()
	require.NoError(t, err)

	seed := func(issueUID, title string, clock int64) {
		fields := issueDocumentFields(issueUID, "remote")
		fields["title"] = title
		fields["updated_at"] = "2026-05-23T12:10:00.000Z"
		ev := newRemoteEvent(t, project, &issueUID, "issue.snapshot", "remote", origin, clock, marshalIssueDocument(t, fields))
		inserted, insErr := store.InsertRemoteEvent(ctx, project.ID, ev)
		require.NoError(t, insErr)
		require.True(t, inserted)
	}
	seed(uidA, "issue-a", 100)
	seed(uidB, "issue-b", 101)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))

	a0, err := store.IssueByUID(ctx, uidA, db.IncludeDeletedNo)
	require.NoError(t, err)
	b0, err := store.IssueByUID(ctx, uidB, db.IncludeDeletedNo)
	require.NoError(t, err)

	// Remote label with an older updated_at (so issues.updated_at stays unchanged)
	// must still advance A's revision on materialization.
	labelEv := newRemoteEvent(t, project, &uidA, "issue.labeled", "remote", origin, 102,
		jsontext.Value(`{"issue_uid":"`+uidA+`","label":"review-ready","updated_at":"2026-05-23T12:05:00.000Z"}`))
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, labelEv)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))

	a1, err := store.IssueByUID(ctx, uidA, db.IncludeDeletedNo)
	require.NoError(t, err)
	b1, err := store.IssueByUID(ctx, uidB, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, a0.Revision+1, a1.Revision, "materialized label change must advance issue revision")
	require.Equal(t, b0.Revision, b1.Revision)

	// Re-materializing with no new events must not bump revision.
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	a1Repeat, err := store.IssueByUID(ctx, uidA, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, a1.Revision, a1Repeat.Revision)

	// Remote link A -> B must advance BOTH A and B revisions on materialization.
	linkEv := newRemoteEvent(t, project, &uidA, "issue.linked", "remote", origin, 103,
		jsontext.Value(`{"type":"blocks","from_uid":"`+uidA+`","to_uid":"`+uidB+`","updated_at":"2026-05-23T12:15:00.000Z"}`))
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, linkEv)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))

	a2, err := store.IssueByUID(ctx, uidA, db.IncludeDeletedNo)
	require.NoError(t, err)
	b2, err := store.IssueByUID(ctx, uidB, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, a1.Revision+1, a2.Revision, "materialized link must advance source revision once")
	require.Equal(t, b1.Revision+1, b2.Revision, "materialized link must advance peer revision")

	// Re-materializing with no new events must not bump either revision.
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	a2Repeat, err := store.IssueByUID(ctx, uidA, db.IncludeDeletedNo)
	require.NoError(t, err)
	b2Repeat, err := store.IssueByUID(ctx, uidB, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, a2.Revision, a2Repeat.Revision)
	require.Equal(t, b2.Revision, b2Repeat.Revision)

	// Remote unlink with older updated_at must still advance both endpoints.
	unlinkEv := newRemoteEvent(t, project, &uidA, "issue.unlinked", "remote", origin, 104,
		jsontext.Value(`{"type":"blocks","from_uid":"`+uidA+`","to_uid":"`+uidB+`","link_from_uid":"`+uidA+`","link_to_uid":"`+uidB+`","updated_at":"2026-05-23T12:12:00.000Z"}`))
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, unlinkEv)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))

	a3, err := store.IssueByUID(ctx, uidA, db.IncludeDeletedNo)
	require.NoError(t, err)
	b3, err := store.IssueByUID(ctx, uidB, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, a2.Revision+1, a3.Revision)
	require.Equal(t, b2.Revision+1, b3.Revision)
	return nil
}
