package sqlitestore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIngestMoveAuditsCompetingLease(t *testing.T) {
	store, ctx, source, spokeUID, issue, _ := setupIngestClaimIssue(t)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, target.ID, "worker")
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: target.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "holder"}, ClaimKind: "hard", Now: time.Now()})
	require.NoError(t, err)
	remote := remoteEvent(t, target.UID, target.Name, &issue.UID, nil, "issue.moved", "other", time.Now().Add(time.Second).UnixMilli(), string(moved.Event.Payload))
	remote.OriginInstanceUID = spokeUID
	remote.ContentHash = remoteEventHash(t, remote)
	params := ingestParams(target.ID, spokeUID, remote)
	params.EventFeatures = db.SupportedEventFeatures
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	violations, count, err := store.UnresolvedClaimViolationsForIssue(ctx, target.ID, issue.UID, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Len(t, violations, 1)
	require.Equal(t, "issue.moved", violations[0].OffendingEventType)
	require.Equal(t, remote.EventUID, violations[0].OffendingEventUID)
}
