package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// R4/A9: full owner backup retains the narrowed parent and immutable emitted
// retry bytes, including acknowledged state and independent stream cursors.
func TestRelayOwnerBackupRetainsDeliveryState(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "forward_cutover_export"}[legacy], func(t *testing.T) {
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			project, err := source.CreateProject(ctx, "shared-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: source.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, source.PinRootAuthority(ctx, pin))
			parent, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "backup-parent-test-token", Actor: "member", AdminActor: "admin"})
			require.NoError(t, err)
			//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
			grant, err := source.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "backup-relay-test-token"})
			require.NoError(t, err)
			writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: source.InstanceUID(), PrivateKey: private}, "member")
			_, _, err = source.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "First", Author: "assistant"})
			require.NoError(t, err)
			_, _, err = source.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Second", Author: "assistant"})
			require.NoError(t, err)
			pending, err := source.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.NoError(t, source.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, pending[0].Sequence, pending[0].Digest))
			retained, err := source.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
			require.NoError(t, err)
			require.Len(t, retained, 1)
			var backup bytes.Buffer
			if legacy {
				err = exportSnapshot(ctx, source, &backup, ExportOptions{})
			} else {
				err = Export(ctx, source, &backup, ExportOptions{})
			}
			require.NoError(t, err)
			dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
			t.Cleanup(cleanup)
			postgres, err := pgstore.Open(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = postgres.Close() })
			err = Import(ctx, bytes.NewReader(backup.Bytes()), postgres)
			require.NoError(t, err)
			assertRetained := func(store db.Storage) {
				actual, err := store.AuthorizeFederationToken(ctx, grant.Token, project.ID, "pull")
				require.NoError(t, err)
				require.Equal(t, grant.Enrollment, actual)
				offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
				require.NoError(t, err)
				require.Equal(t, retained, offered)
				receipts, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
				require.NoError(t, err)
				require.Len(t, receipts, 2)
				require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, pending[0].Sequence, pending[0].Digest), "already-acked endpoint survives restore")
			}
			assertRetained(postgres)
			var forwarded bytes.Buffer
			require.NoError(t, Export(ctx, postgres, &forwarded, ExportOptions{}))
			restored, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = restored.Close() })
			err = Import(ctx, bytes.NewReader(forwarded.Bytes()), restored)
			require.NoError(t, err)
			assertRetained(restored)
			_, _, err = restored.RevokeAPIToken(ctx, parent.ID, "admin")
			require.NoError(t, err)
			_, err = restored.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
			require.Error(t, err)
		})
	}
}
