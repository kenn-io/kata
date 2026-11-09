package sqlitestore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestFederationIngestRechecksCurrentProjectAfterConcurrentMove(t *testing.T) {
	d, source, issue := incrementalTestStore(t)
	ctx := t.Context()
	target, err := d.CreateProject(ctx, "concurrent-target")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, target.ID, "tester")
	require.NoError(t, err)
	event := incrementalRemoteEvent(t, source, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"Unapproved moved update"}`, 9_000_000_000_000)
	grant, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: event.OriginInstanceUID, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester"})
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts == 1 {
			_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err, "optimistic preparation must release the writer for a competing move")
		}
	}
	_, err = d.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: source.ID, SpokeInstanceUID: event.OriginInstanceUID, BoundActor: "tester", FederationEnrollmentID: grant.Enrollment.ID, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
	require.Equal(t, 2, attempts, "retry must resolve the newly committed current owner")
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	require.Equal(t, issue.Title, got.Title)
	stored, err := d.EventsByUIDs(ctx, source.ID, []string{event.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Empty(t, stored)
}
