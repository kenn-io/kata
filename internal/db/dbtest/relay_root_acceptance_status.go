package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayRootAcceptanceStatus exercises relay root acceptance status on the supplied native store.
// R4: immediate-hop delivery does not mean the root accepted the source.
// Durable intent remains pending through event compaction until its exact proof.
func RunRelayRootAcceptanceStatus(t *testing.T, store db.Storage, compactSource func(context.Context, int64) error) {
	for _, scenario := range []struct {
		name           string
		compact, relay bool
	}{
		{"retained-source", false, false}, {"compacted-source", true, false}, {"compacted-relay-source", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, scenario.name)
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, store.PinRootAuthority(ctx, pin))
			config := db.RelayBindingConfig{ProtocolVersion: 1, BindingUID: project.UID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "personal-member", ResetEpoch: 1}
			_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			require.NoError(t, err)
			issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "source-assistant", Title: "Awaiting root acceptance"})
			require.NoError(t, err)
			offered, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
			require.NoError(t, err)
			require.Len(t, offered, 1)
			require.NoError(t, store.AckRelayDeliveries(ctx, config.BindingUID, config.ResetEpoch, db.RelayStreamEvent, offered[0].Sequence, offered[0].Digest))
			count, _, err := store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), 0)
			require.NoError(t, err)
			require.Equal(t, int64(1), count, "a hop ACK cannot certify root acceptance")
			count, _, err = store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), event.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), count, "a legacy cursor cannot certify negotiated root acceptance")
			count, high, err := store.PendingFederationPushStats(db.WithAuthorizedProjects(ctx, []string{}), project.ID, store.InstanceUID(), 0)
			require.NoError(t, err)
			require.Zero(t, count, "status must not expose a hidden project's retained sources")
			require.Zero(t, high)
			if scenario.compact {
				require.NoError(t, compactSource(ctx, project.ID))
			}
			count, _, err = store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), 0)
			require.NoError(t, err)
			require.Equal(t, int64(1), count, "compaction cannot discard durable root-pending intent")
			pendingView, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "pending", pendingView.Verification, "root-pending creation survives source compaction")
			_, err = store.LeaveFederationReplica(ctx, project.ID)
			require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "hop delivery cannot discard root-pending intent during detach")
			proof, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: event.UID, ContentHash: event.ContentHash, AuthorityUID: rootUID, KeyID: pin.KeyID, AccountableActor: "company-member", SourceActor: event.Actor, IngressInstanceUID: store.InstanceUID(), AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1}, private)
			require.NoError(t, err)
			if scenario.relay {
				body, err := json.Marshal(proof)
				require.NoError(t, err)
				envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: config.BindingUID, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: config.ResetEpoch, Sequence: 71, Stream: db.RelayStreamReceipt, Path: []string{rootUID}, SourceUID: event.UID, SourceHash: event.ContentHash, Body: body})
				require.NoError(t, err)
				_, err = store.AcceptRelayDeliveries(ctx, config.BindingUID, db.RelayBatch{Stream: db.RelayStreamReceipt, Envelopes: []db.RelayEnvelope{envelope}})
				require.NoError(t, err)
			} else {
				require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, proof))
			}
			creator, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
			require.NoError(t, err, "root proof attaches the original creator using durable source intent after compaction")
			require.Equal(t, proof, creator)
			verifiedView, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "verified", verifiedView.Verification)
			require.Equal(t, "company-member", verifiedView.AccountableActor)
			count, high, err = store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), 0)
			require.NoError(t, err)
			require.Zero(t, count, "only the retained exact root proof clears pending state")
			require.Zero(t, high)
			receipts, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamReceipt, 1024)
			require.NoError(t, err)
			if len(receipts) > 0 {
				_, err = store.LeaveFederationReplica(ctx, project.ID)
				require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "root acceptance does not discard another unacknowledged stream")
				last := receipts[len(receipts)-1]
				require.NoError(t, store.AckRelayDeliveries(ctx, config.BindingUID, last.Epoch, last.Stream, last.Sequence, last.Digest))
			}
			_, err = store.LeaveFederationReplica(ctx, project.ID)
			require.NoError(t, err, "resolved delivery permits explicit detach")
			_, err = store.LeaveFederationReplica(ctx, project.ID)
			require.NoError(t, err, "detach resumes idempotently after lost response")
		})
	}
}
