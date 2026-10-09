package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederationAdoptionAfterDisconnect(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		Actor: "example-actor", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)

	bindingUID, err := uid.New()
	require.NoError(t, err)
	config := db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "example-actor",
		ServeDownstream: false, ResetEpoch: 1,
	}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Relay source", Author: "example-actor",
	})
	require.NoError(t, err)
	proof, err := db.SignRootReceipt(db.AttributionReceipt{
		Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: pin.KeyID,
		EventUID: event.UID, ContentHash: event.ContentHash,
		AccountableActor: "example-actor", SourceActor: event.Actor,
		IngressInstanceUID: store.InstanceUID(), AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1,
	}, private)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, proof),
		"the detached namespace retains the original signed receipt")
	require.Equal(t, project.ID, issue.ProjectID)
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		queued, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 10)
		require.NoError(t, err)
		if len(queued) == 0 {
			continue
		}
		last := queued[len(queued)-1]
		require.NoError(t, store.AckRelayDeliveries(
			ctx, bindingUID, config.ResetEpoch, stream, last.Sequence, last.Digest,
		))
	}

	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	nextPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	nextPin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic,
	}
	transition, err := db.SignRootKeyTransition(pin, nextPin, private)
	require.NoError(t, err)
	require.NoError(t, store.RotateRootAuthority(ctx, transition))

	var retainedRelayRows int
	for record, err := range store.ExportRelayState(ctx) {
		require.NoError(t, err)
		switch row := record.(type) {
		case *db.RelayOutboxExport:
			if row.ProjectUID == project.UID {
				retainedRelayRows++
			}
		case *db.RelayCursorExport:
			if row.ProjectUID == project.UID {
				retainedRelayRows++
			}
		}
	}
	require.Positive(t, retainedRelayRows, "detaching retains relay delivery history")

	newHubUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://other-hub.example", HubProjectID: 43,
		HubProjectUID: newHubUID, ReplayHorizonEventID: 1, Actor: "example-actor",
	})
	require.NoError(t, err,
		"adoption after disconnect must retire the old signed-provenance and relay namespace transactionally")

	for record, err := range store.ExportRelayState(ctx) {
		require.NoError(t, err)
		switch row := record.(type) {
		case *db.RelayOutboxExport:
			require.NotEqual(t, project.UID, row.ProjectUID)
			require.NotEqual(t, newHubUID, row.ProjectUID,
				"old relay envelopes cannot be rewritten into the adopted project namespace")
		case *db.RelayCursorExport:
			require.NotEqual(t, project.UID, row.ProjectUID)
			require.NotEqual(t, newHubUID, row.ProjectUID,
				"old relay cursors cannot be rewritten into the adopted project namespace")
		}
	}
	var retainedProofs int
	for record, err := range store.ExportAttribution(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		switch proof := record.(type) {
		case *db.RootKeyPin:
			retainedProofs++
		case *db.AttributionReceipt:
			retainedProofs++
			require.Equal(t, project.UID, proof.ProjectUID,
				"the signed receipt must retain its original project UID")
		}
	}
	require.Zero(t, retainedProofs,
		"adoption retires old signed provenance instead of rewriting it for the new UID")
	return nil
}
