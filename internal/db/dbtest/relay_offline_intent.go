package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayOfflineIntent exercises relay offline intent on the supplied native store.
// R4: ordinary offline writes retain exact delivery intent while transport is
// paused or revoked. A current grant is still required to emit or accept it.
func RunRelayOfflineIntent(t *testing.T, store db.Storage) {
	for _, state := range []string{"paused", "push-disabled", "revoked"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "offline-"+state)
			require.NoError(t, err)
			binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true}
			_, err = store.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			public, _, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			legacy, legacyEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "historical-assistant", Title: "Historical work before negotiated relay"})
			require.NoError(t, err)
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, legacyEvent.ID), "the existing standalone history was already accepted by the hub")
			binding.PushCursorEventID = legacyEvent.ID
			config := db.RelayBindingConfig{ProtocolVersion: 1, BindingUID: project.UID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "personal-member", ResetEpoch: 1}
			_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			require.NoError(t, err)
			switch state {
			case "paused":
				binding.Enabled = false
				_, err = store.UpsertFederationBinding(ctx, binding)
			case "push-disabled":
				binding.PushEnabled = false
				_, err = store.UpsertFederationBinding(ctx, binding)
			case "revoked":
				config.UpstreamRevoked = true
				_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			}
			require.NoError(t, err)
			issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "source-assistant", Title: "Local offline write"})
			if state == "push-disabled" {
				require.ErrorIs(t, err, db.ErrFederatedReadOnly, "explicit read-only configuration still rejects local writes")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "pending", issue.Verification, "fresh offline work awaits root accountability")
			require.Equal(t, "source-assistant", issue.SourceActor)
			require.Empty(t, issue.AccountableActor, "no root receipt means no certified accountable actor")
			current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "pending", current.Verification)
			historical, err := store.IssueByUID(ctx, legacy.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "legacy", historical.Verification, "enrollment cannot promote historical source labels")
			var found *db.RelayOutboxExport
			for record, err := range store.ExportRelayState(ctx) {
				require.NoError(t, err)
				outbox, ok := record.(*db.RelayOutboxExport)
				if ok && outbox.BindingUID == config.BindingUID && outbox.SourceUID == event.UID {
					found = outbox
				}
			}
			require.NotNil(t, found, "transport denial must not discard the committed source intent")
			require.False(t, found.Emitted)
			require.False(t, found.Acknowledged)
			require.Equal(t, event.ContentHash, found.SourceHash)
			var envelope db.RelayEnvelope
			require.NoError(t, json.Unmarshal([]byte(found.Envelope), &envelope))
			source, err := db.DecodeRelaySourceEvent(envelope.Body)
			require.NoError(t, err)
			require.Equal(t, issue.UID, *source.IssueUID)
			require.Equal(t, event.UID, source.EventUID)
			require.Equal(t, "source-assistant", source.Actor)
			_, err = store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
			require.Error(t, err, "retaining intent grants no transport authority")
			binding.Enabled = true
			binding.PushEnabled = true
			_, err = store.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			config.UpstreamRevoked = false
			_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			require.NoError(t, err)
			offered, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
			require.NoError(t, err)
			require.Equal(t, []db.RelayEnvelope{envelope}, offered, "resume uses the same original identity and bytes")
			comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-assistant", Teammate: "researcher", Body: "New comment awaiting company attribution"})
			require.NoError(t, err)
			require.Equal(t, "pending", comment.Verification)
			require.Equal(t, "comment-assistant", comment.SourceActor)
			require.Equal(t, "researcher", comment.Teammate)
			require.Empty(t, comment.AccountableActor)
		})
	}
}

// RunRelayReconnectOfflineIntent keeps standalone edits visible to relay
// delivery and blocks a signed reset until those edits receive root attribution.
func RunRelayReconnectOfflineIntent(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	for _, scenario := range []struct {
		name         string
		requireReset bool
	}{{name: "without-reset"}, {name: "reset-required", requireReset: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "reconnected-project-"+scenario.name)
			require.NoError(t, err)
			binding := db.FederationBinding{
				ProjectID: project.ID, Role: db.FederationRoleSpoke,
				HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
				Actor: "personal-member", Enabled: true, PushEnabled: true,
			}
			_, err = store.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			pin := db.RootKeyPin{
				ProjectUID: project.UID, AuthorityUID: rootUID,
				KeyID: db.RootPublicKeyID(rootPublic), PublicKey: rootPublic,
			}
			require.NoError(t, store.PinRootAuthority(ctx, pin))
			relay := db.RelayBindingConfig{
				ProtocolVersion: db.RelayProtocolVersion, BindingUID: project.UID,
				AuthorityUID: rootUID, UpstreamInstanceUID: rootUID,
				HubPath:    []string{rootUID, store.InstanceUID()},
				LocalActor: binding.Actor, ResetEpoch: 1,
			}
			_, err = store.SetRelayBindingConfig(ctx, project.ID, relay)
			require.NoError(t, err)

			_, err = store.LeaveFederationReplica(ctx, project.ID)
			require.NoError(t, err)
			issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Author: "source-assistant", Title: "standalone edit",
			})
			require.NoError(t, err)
			retainedTitle := issue.Title
			if scenario.requireReset {
				require.NoError(t, compact(ctx, project.ID))
				issueTitle := "retained post-compaction edit"
				_, editEvent, _, err := store.EditIssue(ctx, db.EditIssueParams{
					IssueID: issue.ID, Actor: "source-assistant", Title: &issueTitle,
				})
				require.NoError(t, err)
				require.NotNil(t, editEvent)
				event = *editEvent
				retainedTitle = issueTitle
			}

			binding.PushCursorEventID = 0
			_, err = store.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			_, err = store.SetRelayBindingConfig(ctx, project.ID, relay)
			require.NoError(t, err)
			pending, err := store.PendingRelayDeliveries(ctx, relay.BindingUID, db.RelayStreamEvent, 10)
			require.NoError(t, err)
			require.Len(t, pending, 1, "reconnect must queue the retained standalone source event")
			require.Equal(t, event.UID, pending[0].SourceUID)
			require.NoError(t, store.AckRelayDeliveries(ctx, relay.BindingUID, pending[0].Epoch, pending[0].Stream, pending[0].Sequence, pending[0].Digest))
			count, highWater, err := store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), 0)
			require.NoError(t, err)
			require.Equal(t, int64(1), count, "a reconnected edit remains pending until root acceptance")
			require.Equal(t, event.ID, highWater)

			if !scenario.requireReset {
				return
			}

			snapshot := db.RootResetSnapshot{
				Events: []byte("[]"), Entities: []byte("[]"), Artifacts: []byte("[]"),
			}
			snapshot.Provenance, err = json.Marshal(db.RootResetProvenance{
				Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{},
			})
			require.NoError(t, err)
			snapshotUID, err := uid.New()
			require.NoError(t, err)
			manifest, err := db.SignRootResetManifest(db.RootResetManifest{
				Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID,
				KeyID: pin.KeyID, ResetEpoch: 1, SnapshotUID: snapshotUID,
			}, snapshot, rootPrivate)
			require.NoError(t, err)
			translation := db.RelayResetTranslation{
				Authority: db.RelayHopAuthority{
					BindingUID: relay.BindingUID, ProjectUID: project.UID, AuthorityUID: rootUID,
					SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 2,
				},
				SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest,
			}
			installer, ok := store.(interface {
				InstallRelayReset(context.Context, string, db.RootResetManifest, db.RootResetSnapshot, db.RelayResetTranslation) error
			})
			require.True(t, ok, "native atomic signed reset installation is required")
			require.ErrorIs(t, installer.InstallRelayReset(ctx, relay.BindingUID, manifest, snapshot, translation),
				db.ErrFederationResetBlockedByPendingPush)
			retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, retainedTitle, retained.Title)
		})
	}
}
