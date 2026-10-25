package dbtest

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunScopedClosePreparation exercises actual native current/preparation and
// mutation hooks. Root trial/apply token consumption is a separate owning join.
func RunScopedClosePreparation(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	write := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[0].UID), f.Parent.Actor)
	ctx := scopedPreparationContext(t, f)
	ctx = db.WithTransactionFence(ctx, db.ActorTokenTransactionFence(f.Parent))
	ctx = db.WithAdditionalTransactionFence(ctx, s.ProjectAccessTransactionFence(f.Parent.Actor, []string{f.Projects[0].UID, f.Projects[1].UID}))
	preparation := db.ScopedMutationPreparation{Kind: "closeIssue", Subject: db.ScopedMutationSubject{Kind: "issue", ProjectUID: f.Projects[0].UID, IssueUID: f.Issue.UID}, Predicate: "close", Peers: []db.ScopedEndpoint{}}
	prepare := func() db.ScopedMutationPredicate {
		t.Helper()
		token, err := s.(scopedStablePreparationStore).PrepareScopedMutation(ctx, preparation)
		require.NoError(t, err)
		predicate := scopedPreparedPredicate(t, s, token)
		require.Equal(t, preparation, predicate.Preparation)
		current, err := s.IssueByID(t.Context(), f.Issue.ID)
		require.NoError(t, err)
		// Independent COMPLETE private native-CAS value. No event/status clock,
		// author, local row ID or public revision is substituted for this tuple.
		golden := fmt.Sprintf(`{"Current":{"Deleted":false,"ProjectDeleted":false,"Revision":%d},"Domain":"kata.scoped.root-predicate.v1","Kind":"issue.close"}`, current.Revision)
		require.Equal(t, db.ScopedOfferedDigest([]byte(golden)), predicate.CanonicalDigest)
		return predicate
	}
	cursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	first := prepare()
	require.Equal(t, first, prepare(), "preparation is not a native mutation/incarnation")
	currentCursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, cursor, currentCursor)
	command := db.ScopedCommand{Kind: "closeIssue", Subject: db.ScopedEndpoint{ProjectUID: f.Projects[0].UID, IssueUID: f.Issue.UID}, Actor: f.Parent.Actor,
		Close: &db.CloseIssueParams{IssueID: f.Issue.ID, ExpectedProjectID: f.Projects[0].ID, Reason: "done", Actor: f.Parent.Actor}}
	token, err := s.(scopedPreparationStore).PrepareMutation(ctx, command)
	require.NoError(t, err, "native command and stable DTO enter the SAME preparer")
	require.Equal(t, first, scopedPreparedPredicate(t, s, token))

	// Actual native CAS precedes already-closed no-op, including stale guards.
	current, err := s.IssueByID(t.Context(), f.Issue.ID)
	require.NoError(t, err)
	revision := current.Revision
	closed, events, changed, err := s.CloseIssueGuarded(write, db.CloseIssueParams{IssueID: current.ID, Reason: "done", Actor: f.Parent.Actor, IfMatchRev: &revision})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, events)
	scopedAssertPredicateCurrent(t, s, first, true)
	closedPredicate := prepare()
	proof := ScopedCurrentClose(t, s, current.UID)
	require.NoError(t, db.ValidateScopedCanonicalCloseState(proof, current.UID, "closed"))
	before := scopedAuthoritySnapshot(t, s)
	_, _, _, err = s.CloseIssueGuarded(write, db.CloseIssueParams{IssueID: current.ID, Reason: "done", Actor: f.Parent.Actor, IfMatchRev: &revision})
	var conflict *db.RevisionConflictError
	require.ErrorAs(t, err, &conflict, "a stale native numeric guard cannot turn into a successful closed no-op")
	require.Equal(t, before, scopedAuthoritySnapshot(t, s))
	_, events, changed, err = s.CloseIssueGuarded(write, db.CloseIssueParams{IssueID: current.ID, Reason: "done", Actor: f.Parent.Actor, IfMatchRev: &closed.Revision})
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	require.Equal(t, before, scopedAuthoritySnapshot(t, s))
	scopedAssertPredicateCurrent(t, s, closedPredicate, false)

	_, event, changed, err := s.ReopenIssue(write, current.ID, f.Parent.Actor)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, event)
	scopedAssertPredicateCurrent(t, s, closedPredicate, true)
	reopened := prepare()
	require.NotEqual(t, first.CanonicalDigest, reopened.CanonicalDigest, "open->closed->open cannot reuse a native version")
	statusProof := ScopedCurrentClose(t, s, current.UID)

	// These real native writes advance revision WITHOUT changing T03 status
	// provenance. A close guard must nevertheless be stale, as IfMatchRev is.
	owner := "assigned-member"
	_, _, changed, err = s.UpdateOwner(write, current.ID, &owner, f.Parent.Actor)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, statusProof, ScopedCurrentClose(t, s, current.UID))
	scopedAssertPredicateCurrent(t, s, reopened, true)
	assigned := prepare()
	_, err = s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{IssueID: current.ID, Actor: f.Parent.Actor, Patch: map[string]jsontext.Value{"close-guard": jsontext.Value(`"changed"`)}})
	require.NoError(t, err)
	require.Equal(t, statusProof, ScopedCurrentClose(t, s, current.UID))
	scopedAssertPredicateCurrent(t, s, assigned, true)
	metadataPredicate := prepare()
	before = scopedAuthoritySnapshot(t, s)
	_, _, changed, err = s.UpdateOwner(write, current.ID, &owner, f.Parent.Actor)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, before, scopedAuthoritySnapshot(t, s), "native no-op does not advance either counter")
	scopedAssertPredicateCurrent(t, s, metadataPredicate, false)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}
