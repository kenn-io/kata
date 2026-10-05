package dbtest

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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
			legacy, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "historical-assistant", Title: "Historical work before negotiated relay"})
			require.NoError(t, err)
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
