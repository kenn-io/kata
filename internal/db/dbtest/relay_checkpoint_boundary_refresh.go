package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayRootCheckpointLaterBoundary exercises legacy source history arriving
// after a child has activated its first signed checkpoint. The original private
// source bytes must stay at the root; the child needs a fresh current snapshot.
func RunRelayRootCheckpointLaterBoundary(t *testing.T, store db.Storage, openReplica func(*testing.T) db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "checkpoint-boundary")
	private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Private endpoint", Author: "member"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, fixture.signer, "relay-member")
	original, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Original issue", Author: "member"})
	require.NoError(t, err)
	replica := openReplica(t)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: replica.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion, ServeDownstream: true,
	})
	require.NoError(t, err)
	bindingUID := grant.Enrollment.RelayBindingUID
	resetter := store.(db.RelayResetStore)
	initial, err := resetter.CreateRelayReset(ctx, bindingUID, fixture.signer)
	require.NoError(t, err)
	pin, err := store.RootAuthority(ctx, fixture.project.UID)
	require.NoError(t, err)
	project, err := replica.CreateProjectWithUID(ctx, "spoke-project", fixture.project.UID)
	require.NoError(t, err)
	require.NoError(t, replica.PinRootAuthority(ctx, pin))
	_, err = replica.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example",
		HubProjectID: fixture.project.ID, HubProjectUID: fixture.project.UID,
		Actor: "relay-member", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	_, err = replica.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		AuthorityUID: store.InstanceUID(), UpstreamInstanceUID: store.InstanceUID(),
		HubPath: []string{store.InstanceUID(), replica.InstanceUID()}, LocalActor: "relay-member",
		ServeDownstream: true, ResetEpoch: 1,
	})
	require.NoError(t, err)
	installer := replica.(db.RelayResetStore)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, initial.Manifest, initial.Snapshot, initial.Translation))
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, initial.Translation.Authority.Epoch), bindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err, "activate the initial checkpoint before later history arrives")
	bootstrap := store.(db.RelayResetBootstrapStore)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, bindingUID)
	require.NoError(t, err)
	require.False(t, required)

	legacyUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	payload := jsontext.Value(`{"uid":"` + issueUID + `","title":"Later legacy creation","body":"","author":"member","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"` + private.UID + `","author":"member"}]}`)
	event := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "member", legacyUID, 600, payload)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: legacyUID,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}},
	})
	require.NoError(t, err)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	metadata, err := store.PatchIssueMetadata(writeCtx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"adapter.state": jsontext.Value(`"running"`)},
	})
	require.NoError(t, err)
	owner := "member"
	_, _, _, err = store.UpdateOwner(writeCtx, issue.ID, &owner, "member")
	require.NoError(t, err)
	links, err := store.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	_, err = resetter.CreateRelayReset(ctx, bindingUID, fixture.signer)
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary, "a live private link must be refused before signing shared current state")
	require.Equal(t, []string{private.UID}, linkTargetUIDs(t, store, issue.ID), "a refused reset must preserve the owner's private link")
	_, err = store.DeleteLinkAndEvent(writeCtx, links[0], db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: issue.ID, Actor: "member",
		FromUID: issue.UID, ToUID: private.UID, FromShortID: issue.ShortID, ToShortID: private.ShortID,
	})
	require.NoError(t, err)
	require.Empty(t, linkTargetUIDs(t, store, issue.ID), "remove the live boundary without rewriting private history")

	later, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Later ordinary issue", Author: "member"})
	require.NoError(t, err)
	// Model an already-rejected pull using the authentic metadata envelope
	// whose issue introduction the old feed withheld. Legacy ingress queues
	// different prefixes on the native backends; retain the complete old
	// offer as well to check that checkpoint recovery covers every row.
	batch, err := store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, batch)
	var rejectedBatch []db.RelayEnvelope
	for _, envelope := range batch {
		if envelope.SourceUID == metadata.Event.UID {
			rejectedBatch = append(rejectedBatch, envelope)
		}
	}
	require.Len(t, rejectedBatch, 1)
	_, rejected := replica.AcceptRelayDeliveries(ctx, bindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: rejectedBatch})
	require.ErrorIs(t, rejected, db.ErrFederationIngestValidation)
	require.ErrorContains(t, rejected, "issue.metadata_updated references unknown issue "+issue.UID)
	var eventUIDs []string
	for _, envelope := range rejectedBatch {
		eventUIDs = append(eventUIDs, envelope.SourceUID)
	}
	require.Contains(t, eventUIDs, metadata.Event.UID)
	quarantine, err := replica.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: project.ID, Direction: db.FederationQuarantineDirectionPull,
		FirstEventID: rejectedBatch[0].Sequence, LastEventID: rejectedBatch[len(rejectedBatch)-1].Sequence,
		EventUIDs: eventUIDs, Error: rejected.Error(), RelayBindingUID: bindingUID,
		RelayResetEpoch: initial.Translation.Authority.Epoch,
	})
	require.NoError(t, err)
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, bindingUID)
	require.NoError(t, err)
	require.True(t, required, "a retained checkpoint cannot hide a later private creation boundary")
	replayed, err := store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, batch, replayed, "the rejected batch retains its original emitted source identities")
	refreshed, err := resetter.CreateRelayReset(ctx, bindingUID, fixture.signer)
	require.NoError(t, err)
	require.NotEqual(t, initial.Manifest.SnapshotUID, refreshed.Manifest.SnapshotUID)
	require.NoError(t, db.VerifyRootResetManifest(pin, refreshed.Manifest, refreshed.Snapshot))
	requireResetOmitsIssue(t, refreshed, private)
	covered, err := store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Empty(t, covered, "the prepared checkpoint must replace old private event offers")
	receipts, err := store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamReceipt, 100)
	require.NoError(t, err)
	require.NotEmpty(t, receipts, "root proofs remain owed while event history is covered by the checkpoint")
	late, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Written after preparation", Author: "member"})
	require.NoError(t, err)
	require.ErrorIs(t, installer.InstallRelayReset(ctx, bindingUID, refreshed.Manifest, refreshed.Snapshot, refreshed.Translation), db.ErrFederationResetBlockedByQuarantine)
	before, err := replica.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	_, err = replica.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{
		ID: quarantine.ID, ProjectID: project.ID, Actor: "operator", Reason: "root upgraded for withheld creation recovery",
	})
	require.NoError(t, err)
	after, err := replica.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, before.PullCursorEventID, after.PullCursorEventID)
	require.Equal(t, before.RelayConfig.ResetEpoch, after.RelayConfig.ResetEpoch)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, refreshed.Manifest, refreshed.Snapshot, refreshed.Translation))
	installed, err := replica.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, issue.Title, installed.Title)
	require.JSONEq(t, `{"adapter.state":"running"}`, string(installed.Metadata))
	require.Equal(t, &owner, installed.Owner)
	require.Empty(t, linkTargetUIDs(t, replica, installed.ID))
	for _, expected := range []db.Issue{original, later} {
		installed, err := replica.IssueByUID(ctx, expected.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, expected.Title, installed.Title)
	}
	_, err = replica.IssueByUID(ctx, private.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, refreshed.Translation.Authority.Epoch), bindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	resumed, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "After recovery", Author: "member"})
	require.NoError(t, err)
	batch, err = store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, batch)
	for _, envelope := range batch {
		source, err := db.DecodeRelaySourceEvent(envelope.Body)
		require.NoError(t, err)
		require.NotContains(t, string(source.Payload), private.UID, "activation cannot reoffer private history")
	}
	_, err = replica.AcceptRelayDeliveries(ctx, bindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: batch})
	require.NoError(t, err)
	for _, expected := range []db.Issue{late, resumed} {
		_, err = replica.IssueByUID(ctx, expected.UID, db.IncludeDeletedYes)
		require.NoError(t, err, "writes after checkpoint preparation must still arrive")
	}
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, bindingUID)
	require.NoError(t, err)
	require.False(t, required, "ordinary new history does not force another checkpoint")
	retry, err := resetter.CreateRelayReset(ctx, bindingUID, fixture.signer)
	require.NoError(t, err)
	require.Equal(t, refreshed.Manifest.SnapshotUID, retry.Manifest.SnapshotUID)
}
