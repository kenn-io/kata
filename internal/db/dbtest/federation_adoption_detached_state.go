package dbtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"strings"
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
	creator, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, proof, creator)
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
	var retainedProofs int
	for record, err := range store.ExportAttribution(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		switch value := record.(type) {
		case *db.RootKeyPin:
			require.Equal(t, project.UID, value.ProjectUID)
			retainedProofs++
		case *db.AttributionReceipt:
			require.Equal(t, proof, *value, "the original signed receipt remains unchanged before adoption")
			retainedProofs++
		case *db.EntityProvenance:
			require.Equal(t, project.UID, value.ProjectUID)
			retainedProofs++
		}
	}
	require.Equal(t, 4, retainedProofs,
		"both root keys, the original receipt, and its entity reference belong to the old UID")
	metadata, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	require.True(t, ok)
	uidMetadataKeys := []string{
		db.AttributionUIResetMetadataPrefix + project.UID,
		db.PendingCreationMetadataPrefix + project.UID + ".issue." + issue.UID,
		db.RelayResetMetadataPrefix + project.UID,
		db.RootKeyTransitionMetadataKey(transition),
	}
	for _, key := range uidMetadataKeys {
		quotedKey := "'" + strings.ReplaceAll(key, "'", "''") + "'"
		_, err := metadata.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES(`+quotedKey+`,'stale') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
		require.NoError(t, err)
	}

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
	retainedProofs = 0
	for record, err := range store.ExportAttribution(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		switch proof := record.(type) {
		case *db.RootKeyPin:
			retainedProofs++
		case *db.AttributionReceipt:
			retainedProofs++
			require.Equal(t, project.UID, proof.ProjectUID,
				"the signed receipt must retain its original project UID")
		case *db.EntityProvenance:
			retainedProofs++
			require.Equal(t, project.UID, proof.ProjectUID,
				"the entity reference must retain its original project UID")
		}
	}
	require.Zero(t, retainedProofs,
		"adoption retires old signed provenance instead of rewriting it for the new UID")
	_, err = store.RootKeyTransitions(ctx, newHubUID)
	require.ErrorIs(t, err, db.ErrNotFound,
		"old root transition metadata must not be attributed to the adopted UID")
	for _, key := range uidMetadataKeys {
		quotedKey := "'" + strings.ReplaceAll(key, "'", "''") + "'"
		var count int
		err := metadata.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key=`+quotedKey).Scan(&count)
		require.NoError(t, err)
		require.Zero(t, count, "old UID-scoped federation metadata must be retired")
	}
	return nil
}
