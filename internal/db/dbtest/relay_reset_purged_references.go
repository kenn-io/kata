package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// RunRelayResetForwardsAfterRootPurgesReferences verifies that a fresh root
// checkpoint can cross two relay hops after purging issues referenced by
// retained parent and evidence history. Current closed state remains in the
// entity snapshots, and the relay forwards the exact signed root checkpoint.
func RunRelayResetForwardsAfterRootPurgesReferences(
	t *testing.T,
	root db.Storage,
	openRelay func(context.Context, db.Project) (db.Storage, db.Project, func(), error),
) {
	t.Helper()
	ctx := t.Context()
	rootProject, err := root.CreateProject(ctx, "relay-reset-root")
	require.NoError(t, err)
	_, err = root.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: rootProject.ID, Role: db.FederationRoleHub,
		HubProjectID: rootProject.ID, HubProjectUID: rootProject.UID, Enabled: true,
	})
	require.NoError(t, err)
	rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	rootPin := db.RootKeyPin{
		ProjectUID: rootProject.UID, AuthorityUID: root.InstanceUID(),
		KeyID: db.RootPublicKeyID(rootPublic), PublicKey: rootPublic,
	}
	require.NoError(t, root.PinRootAuthority(ctx, rootPin))
	rootSigner := db.RootAttributionSigner{AuthorityUID: root.InstanceUID(), PrivateKey: rootPrivate}
	rootWrite := db.WithRootAttribution(ctx, rootSigner, "member")

	parent, _, err := root.CreateIssue(rootWrite, db.CreateIssueParams{
		ProjectID: rootProject.ID, Author: "member", Title: "Parent target",
	})
	require.NoError(t, err)
	parentChild, _, err := root.CreateIssue(rootWrite, db.CreateIssueParams{
		ProjectID: rootProject.ID, Author: "member", Title: "Closed child with parent",
	})
	require.NoError(t, err)
	_, err = root.EditIssueAtomic(rootWrite, db.EditIssueAtomicParams{
		IssueID: parentChild.ID, Actor: "member", SetParent: &parent.ID,
	})
	require.NoError(t, err)
	parentComment, _, err := root.CreateComment(rootWrite, db.CreateCommentParams{
		IssueID: parent.ID, Author: "member", Body: "Comment on the parent before purge",
	})
	require.NoError(t, err)
	parentReply, _, err := root.CreateComment(rootWrite, db.CreateCommentParams{
		IssueID: parentChild.ID, Author: "member", Body: "Reply remains on the live child",
		ReplyToUID: parentComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	notificationSlot, err := json.Marshal(notification.Value{
		From: "member", Message: "Follow up about the purged comment", Re: parentComment.UID,
	})
	require.NoError(t, err)
	_, err = root.PatchIssueMetadata(rootWrite, db.PatchIssueMetadataIn{
		IssueID: parentChild.ID, Actor: "member",
		Patch: map[string]jsontext.Value{notification.MetadataKey("reader"): notificationSlot},
	})
	require.NoError(t, err)
	_, _, changed, err := root.CloseIssueWithEvents(
		rootWrite, parentChild.ID, "done", "member",
		"The child work completed before its parent was purged.", nil,
	)
	require.NoError(t, err)
	require.True(t, changed)

	evidenceTarget, _, err := root.CreateIssue(rootWrite, db.CreateIssueParams{
		ProjectID: rootProject.ID, Author: "member", Title: "Evidence target",
	})
	require.NoError(t, err)
	evidenceChild, _, err := root.CreateIssue(rootWrite, db.CreateIssueParams{
		ProjectID: rootProject.ID, Author: "member", Title: "Closed child with evidence",
	})
	require.NoError(t, err)
	_, _, changed, err = root.CloseIssueWithEvents(
		rootWrite, evidenceChild.ID, "duplicate", "member",
		"The duplicate was recorded before its evidence target was purged.",
		[]db.Evidence{{Type: "duplicate-of", IssueRef: evidenceTarget.ShortID}},
	)
	require.NoError(t, err)
	require.True(t, changed)
	_, err = root.PurgeIssue(ctx, parent.ID, "admin", nil)
	require.NoError(t, err)
	_, err = root.PurgeIssue(ctx, evidenceTarget.ID, "admin", nil)
	require.NoError(t, err)

	relay, relayProject, closeRelay, err := openRelay(ctx, rootProject)
	require.NoError(t, err)
	t.Cleanup(closeRelay)
	require.Equal(t, rootProject.UID, relayProject.UID)

	rootToken, _, err := root.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "member", AdminActor: "admin", PlaintextToken: "root-to-relay-reset-test-token", // #nosec G101 -- synthetic API token used only in this test.
	})
	require.NoError(t, err)
	rootGrant, err := root.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: rootProject.ID, ParentTokenID: rootToken.ID,
		SpokeInstanceUID: relay.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion,
		Token: "root-to-relay-enrollment-test-token", ServeDownstream: true, // #nosec G101 -- synthetic relay token used only in this test.
	})
	require.NoError(t, err)
	rootReset, ok := root.(db.RelayResetStore)
	require.True(t, ok, "native root checkpoint generation is required")
	checkpoint, err := rootReset.CreateRelayReset(ctx, rootGrant.Enrollment.RelayBindingUID, rootSigner)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(rootPin, checkpoint.Manifest, checkpoint.Snapshot))
	var checkpointEvents [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &checkpointEvents))
	for _, body := range checkpointEvents {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if event.Type != "issue.commented" {
			continue
		}
		var payload struct {
			CommentUID string `json:"comment_uid"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		require.NotEqual(t, parentReply.UID, payload.CommentUID, "history depending on the purged reply target is omitted")
	}
	var checkpointEntities [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &checkpointEntities))
	for _, body := range checkpointEntities {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if event.Type != "issue.snapshot" || event.IssueUID == nil || *event.IssueUID != parentChild.UID {
			continue
		}
		var payload struct {
			Comments []struct {
				CommentUID string `json:"comment_uid"`
				ReplyToUID string `json:"reply_to_uid"`
				ReplyKind  string `json:"reply_kind"`
				Body       string `json:"body"`
			} `json:"comments"`
			Metadata map[string]jsontext.Value `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		require.Contains(t, payload.Comments, struct {
			CommentUID string `json:"comment_uid"`
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
			Body       string `json:"body"`
		}{CommentUID: parentReply.UID, Body: "Reply remains on the live child"}, "the checkpoint preserves the live comment and drops only its dangling reply edge")
		value, _, err := notification.DecodeValue(payload.Metadata[notification.MetadataKey("reader")])
		require.NoError(t, err)
		require.Equal(t, "Follow up about the purged comment", value.Message)
		require.Empty(t, value.Re, "the checkpoint preserves notification content but drops its dangling comment reference")
	}

	require.NoError(t, relay.PinRootAuthority(ctx, rootPin))
	relayBinding, err := relay.FederationBindingByProject(ctx, relayProject.ID)
	require.NoError(t, err)
	relayBinding.PushEnabled = true
	_, err = relay.UpsertFederationBinding(ctx, relayBinding)
	require.NoError(t, err)
	_, err = relay.SetRelayBindingConfig(ctx, relayProject.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion,
		BindingUID:      rootGrant.Enrollment.RelayBindingUID,
		AuthorityUID:    root.InstanceUID(), UpstreamInstanceUID: root.InstanceUID(),
		HubPath:    []string{root.InstanceUID(), relay.InstanceUID()},
		LocalActor: "member", ServeDownstream: true, ResetEpoch: 1,
	})
	require.NoError(t, err)
	installer, ok := relay.(db.RelayResetStore)
	require.True(t, ok, "native relay checkpoint installation and forwarding are required")
	require.NoError(t, installer.InstallRelayReset(
		ctx, rootGrant.Enrollment.RelayBindingUID,
		checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation,
	))
	for _, issue := range []db.Issue{parentChild, evidenceChild} {
		installed, err := relay.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, "closed", installed.Status, "the entity snapshot retains current closed state")
	}
	for _, issue := range []db.Issue{parent, evidenceTarget} {
		_, err := relay.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound, "the fresh relay has no root-local purge ownership facts")
	}

	leafToken, _, err := relay.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "member", AdminActor: "admin", PlaintextToken: "relay-to-leaf-reset-test-token", // #nosec G101 -- synthetic API token used only in this test.
	})
	require.NoError(t, err)
	leaf, err := relay.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: relayProject.ID, ParentTokenID: leafToken.ID,
		SpokeInstanceUID: "00000000000000000000000008",
		ProtocolVersion:  db.RelayProtocolVersion, Token: "relay-to-leaf-enrollment-test-token", // #nosec G101 -- synthetic relay token used only in this test.
	})
	require.NoError(t, err)
	drainRelayGrant(t, relay, leaf.Enrollment.RelayBindingUID, leaf.Enrollment.RelayResetEpoch)
	forwarded, err := installer.CreateRelayReset(ctx, leaf.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.NoError(t, err, "root-purge references must not block the next hop's reset")
	require.Equal(t, checkpoint.Manifest, forwarded.Manifest, "forwarding retains the root's signed manifest")
	require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot, "forwarding retains immutable root checkpoint bytes")

	var history [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &history))
	closedHistory := map[string]bool{}
	for _, body := range history {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if event.Type == "issue.closed" && event.IssueUID != nil {
			closedHistory[*event.IssueUID] = true
		}
	}
	require.False(t, closedHistory[parentChild.UID], "historical close with a purged parent is omitted")
	require.False(t, closedHistory[evidenceChild.UID], "historical close with purged evidence is omitted")
	var entities [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &entities))
	closedSnapshots := map[string]bool{}
	commentsByIssue := map[string][]struct {
		CommentUID string `json:"comment_uid"`
		ReplyToUID string `json:"reply_to_uid"`
		ReplyKind  string `json:"reply_kind"`
		Body       string `json:"body"`
	}{}
	for _, body := range entities {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if event.Type != "issue.snapshot" || event.IssueUID == nil {
			continue
		}
		var state struct {
			Status   string `json:"status"`
			Comments []struct {
				CommentUID string `json:"comment_uid"`
				ReplyToUID string `json:"reply_to_uid"`
				ReplyKind  string `json:"reply_kind"`
				Body       string `json:"body"`
			} `json:"comments"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &state))
		commentsByIssue[*event.IssueUID] = state.Comments
		if state.Status == "closed" {
			closedSnapshots[*event.IssueUID] = true
		}
	}
	require.True(t, closedSnapshots[parentChild.UID], "parent child's closed state remains in the checkpoint")
	require.True(t, closedSnapshots[evidenceChild.UID], "evidence child's closed state remains in the checkpoint")
	require.Contains(t, commentsByIssue[parentChild.UID], struct {
		CommentUID string `json:"comment_uid"`
		ReplyToUID string `json:"reply_to_uid"`
		ReplyKind  string `json:"reply_kind"`
		Body       string `json:"body"`
	}{CommentUID: parentReply.UID, Body: "Reply remains on the live child"}, "the reply body remains in current entity state while its purged target is omitted")
}
