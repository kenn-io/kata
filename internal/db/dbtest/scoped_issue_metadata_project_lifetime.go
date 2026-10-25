package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunScopedIssueMetadataProjectLifetime exercises the existing opaque predicate
// law through real native project archive and restore, without token consumption.
func RunScopedIssueMetadataProjectLifetime(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	write := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Issue.ProjectUID), f.Parent.Actor)
	patched, err := s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{
		IssueID: f.Issue.ID,
		Actor:   f.Parent.Actor,
		Patch:   map[string]jsontext.Value{"guard": jsontext.Value(`"B"`)},
	})
	require.NoError(t, err)
	require.True(t, patched.Changed)
	ctx := scopedPreparationContext(t, f)
	ctx = db.WithTransactionFence(ctx, db.ActorTokenTransactionFence(f.Parent))
	ctx = db.WithAdditionalTransactionFence(ctx, s.ProjectAccessTransactionFence(f.Parent.Actor, []string{f.Projects[0].UID, f.Projects[1].UID}))
	native, ok := s.(scopedPreparationStore)
	require.True(t, ok)
	command := db.ScopedCommand{
		Kind:    "patchIssueMetadata",
		Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID},
		Actor:   f.Parent.Actor,
		Metadata: &db.PatchIssueMetadataIn{
			IssueID: f.Issue.ID,
			Actor:   f.Parent.Actor,
			Patch:   map[string]jsontext.Value{"guard": jsontext.Value(`"next"`)},
			Guard:   &db.MetadataPatchGuard{Key: "guard", IfValue: jsontext.Value(`"B"`)},
		},
	}
	token, err := native.PrepareMutation(ctx, command)
	require.NoError(t, err, "real live preparation must precede project lifetime changes")
	require.NotEmpty(t, token.Token)
	tokenDigest := db.ScopedOfferedDigest([]byte(token.Token))
	retained := func(records []db.ImportRecord) (db.ScopedPredicateIncarnationExport, db.ScopedMutationExpectationExport) {
		t.Helper()
		var issuer *db.ScopedPredicateIncarnationExport
		var expectation *db.ScopedMutationExpectationExport
		for _, record := range records {
			switch row := record.(type) {
			case *db.ScopedPredicateIncarnationExport:
				if row.ProjectUID == f.Issue.ProjectUID && row.SubjectUID == f.Issue.UID && row.PredicateKind == "issue.metadata" {
					require.Nil(t, issuer)
					issuer = row
				}
			case *db.ScopedMutationExpectationExport:
				if row.TokenDigest == tokenDigest {
					require.Nil(t, expectation)
					expectation = row
				}
			}
		}
		require.NotNil(t, issuer, "actual issue.metadata issuer must remain retained")
		require.NotNil(t, expectation, "original opaque token must remain retained")
		require.Equal(t, f.Issue.ProjectUID, expectation.ProjectUID)
		require.Equal(t, f.Issue.UID, expectation.SubjectUID)
		require.Equal(t, "issue.metadata", expectation.PredicateKind)
		return *issuer, *expectation
	}
	baseline := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(baseline), "real prepared owner graph is valid before archive")
	prepared, originalToken := retained(baseline)
	var predicate db.ScopedMutationPredicate
	require.NoError(t, db.DecodeScopedRecord(originalToken.PredicateBytes, &predicate))
	require.Equal(t, db.ScopedMutationSubject{Kind: "issue", ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, predicate.Preparation.Subject)
	require.Equal(t, prepared.Incarnation, predicate.Incarnation)
	require.Equal(t, prepared.CanonicalDigest, predicate.CanonicalDigest)
	beforeIssue, err := s.IssueByUID(write, f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.JSONEq(t, `{"guard":"B"}`, string(beforeIssue.Metadata))
	beforeProject, err := s.ProjectByUID(write, f.Issue.ProjectUID)
	require.NoError(t, err)
	require.Nil(t, beforeProject.DeletedAt)

	archivedProject, archiveEvent, err := s.RemoveProject(write, db.RemoveProjectParams{ProjectID: beforeProject.ID, Actor: f.Parent.Actor, Force: true})
	require.NoError(t, err, "actual native archive must succeed without changing owner authority")
	require.NotNil(t, archiveEvent)
	require.Equal(t, "project.removed", archiveEvent.Type)
	require.Equal(t, beforeProject.UID, archiveEvent.ProjectUID)
	require.Equal(t, beforeProject.ID, archivedProject.ID)
	require.Equal(t, beforeProject.UID, archivedProject.UID)
	require.NotNil(t, archivedProject.DeletedAt)
	archivedRecords := scopedAuthoritySnapshot(t, s)
	archived, archivedToken := retained(archivedRecords)

	restoredProject, restoreEvent, changed, err := s.RestoreProject(write, beforeProject.ID, f.Parent.Actor)
	require.NoError(t, err, "actual native restore must reach the return to B")
	require.True(t, changed)
	require.NotNil(t, restoreEvent)
	require.Equal(t, "project.restored", restoreEvent.Type)
	require.Equal(t, beforeProject.UID, restoreEvent.ProjectUID)
	require.Equal(t, beforeProject.ID, restoredProject.ID)
	require.Equal(t, beforeProject.UID, restoredProject.UID)
	require.Nil(t, restoredProject.DeletedAt)
	restoredIssue, err := s.IssueByUID(write, beforeIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, beforeIssue.ID, restoredIssue.ID)
	require.Equal(t, beforeIssue.UID, restoredIssue.UID)
	require.Equal(t, beforeIssue.ProjectUID, restoredIssue.ProjectUID)
	require.Equal(t, beforeIssue.Metadata, restoredIssue.Metadata)
	restoredRecords := scopedAuthoritySnapshot(t, s)
	restored, restoredToken := retained(restoredRecords)

	// Reach both native transitions and capture their rows before any new
	// preparation can repair or conceal a missing ordinary event hook.
	require.NoError(t, db.ValidateImportRecords(archivedRecords), "committed archived owner graph must remain valid")
	require.NoError(t, db.ValidateImportRecords(restoredRecords), "committed restored owner graph must remain valid")
	require.Greater(t, archived.Incarnation, prepared.Incarnation, "project archive invalidates the existing issue.metadata predicate")
	require.Greater(t, restored.Incarnation, archived.Incarnation, "project restore cannot revive the old opaque predicate")
	require.Equal(t, prepared.CanonicalDigest, restored.CanonicalDigest, "metadata B and live project state return to the same value")
	require.Equal(t, originalToken, archivedToken, "archive preserves the immutable token and predicate bytes")
	require.Equal(t, originalToken, restoredToken, "restore preserves the immutable token and predicate bytes")

	freshToken, err := native.PrepareMutation(ctx, command)
	require.NoError(t, err, "live preparation remains available after the actual lifetime transitions")
	require.NotEqual(t, token.Token, freshToken.Token)
	fresh := scopedPreparedPredicate(t, s, freshToken)
	require.Equal(t, restored.Incarnation, fresh.Incarnation)
	require.Equal(t, restored.CanonicalDigest, fresh.CanonicalDigest)
	finalRecords := scopedAuthoritySnapshot(t, s)
	_, finalToken := retained(finalRecords)
	require.Equal(t, originalToken, finalToken)
	require.NoError(t, db.ValidateImportRecords(finalRecords))
}
