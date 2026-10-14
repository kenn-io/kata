package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RelayedIssueCreation is a relay delivery of one labeled issue creation and
// one comment on it, accepted by a root hub in a single batch.
type RelayedIssueCreation struct {
	Project  db.Project
	IssueUID string
	Accepted db.RelayAcceptance
}

// AcceptRelayedIssueCreation enrolls a relay peer on a fresh root hub project
// and accepts a batch that creates a labeled issue and comments on it.
func AcceptRelayedIssueCreation(t *testing.T, store db.Storage) RelayedIssueCreation {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "relay-hook-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "relay-hook-parent-test-token", Actor: "relay-member", AdminActor: "admin"})
	require.NoError(t, err)
	peer, err := uid.New()
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "relay-hook-relay-test-token"})
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	created := newRemoteEvent(t, project, &issueUID, "issue.created", "relay-member", peer, 500, jsontext.Value(`{"uid":"`+issueUID+`","title":"Relayed hook issue","body":"","author":"relay-member","status":"open","metadata":{},"labels":["relayed"],"created_at":"2026-05-23T12:00:00.000Z"}`))
	commented := newRemoteEvent(t, project, &issueUID, "issue.commented", "relay-member", peer, 501, jsontext.Value(`{"comment_uid":"`+commentUID+`","author":"relay-member","body":"Relayed comment","created_at":"2026-05-23T12:01:00.000Z"}`))
	envelopes := []db.RelayEnvelope{}
	for sequence, event := range []db.RemoteEvent{created, commented} {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: int64(sequence + 1), Stream: db.RelayStreamEvent, Path: []string{peer}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		envelopes = append(envelopes, envelope)
	}
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "relay-member")
	accepted, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: envelopes})
	require.NoError(t, err)
	return RelayedIssueCreation{Project: project, IssueUID: issueUID, Accepted: accepted}
}

// RunRelayIngestReturnsLocalIssueIDs proves the events relay acceptance
// returns for publication name the local issue they materialized, as the
// events legacy federation ingest returns do. Hooks resolve their issue
// block from that ID.
func RunRelayIngestReturnsLocalIssueIDs(t *testing.T, store db.Storage) {
	relayed := AcceptRelayedIssueCreation(t, store)
	issue, err := store.IssueByUID(t.Context(), relayed.IssueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	types := map[string]bool{}
	for _, event := range relayed.Accepted.InsertedEvents {
		if event.IssueUID == nil || *event.IssueUID != relayed.IssueUID {
			continue
		}
		types[event.Type] = true
		require.NotNil(t, event.IssueID, "%s event names its local issue", event.Type)
		require.Equal(t, issue.ID, *event.IssueID, "%s event names its local issue", event.Type)
	}
	require.Equal(t, map[string]bool{"issue.created": true, "issue.commented": true}, types)
}
