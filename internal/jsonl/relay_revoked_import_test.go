package jsonl

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// A backup written before revocation pruned delivery rows still carries a
// revoked grant's outbox, inbox and cursor rows. Restoring it must not bring
// those rows back while a live grant's rows are restored exactly.
func TestImportSkipsRevokedRelayBindingRows(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "backup-root-project")
	require.NoError(t, err)
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, source.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: source.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "backup-parent-test-token", Actor: "company-member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	revoked, err := source.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "backup-revoked-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	live, err := source.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000007", ProtocolVersion: db.RelayProtocolVersion, Token: "backup-live-test-token"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: source.InstanceUID(), PrivateKey: private}, "company-member")
	_, _, err = source.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Delivered everywhere", Author: "source-assistant"})
	require.NoError(t, err)
	for _, binding := range []string{revoked.Enrollment.RelayBindingUID, live.Enrollment.RelayBindingUID} {
		pending, err := source.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 10)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.NoError(t, source.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, pending[0].Sequence, pending[0].Digest))
	}
	// A build that kept revoked bindings' rows left exactly this state behind.
	_, err = source.ExecContext(ctx, `UPDATE federation_enrollments SET revoked_at='2026-10-01T00:00:00.000Z' WHERE id=?`, revoked.Enrollment.ID)
	require.NoError(t, err)
	require.NotEmpty(t, relayRowsForBinding(t, source, revoked.Enrollment.RelayBindingUID))
	liveRows := relayRowsForBinding(t, source, live.Enrollment.RelayBindingUID)
	require.NotEmpty(t, liveRows)
	var backup bytes.Buffer
	require.NoError(t, Export(ctx, source, &backup, ExportOptions{IncludeDeleted: true}))

	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgres.Close() })
	sqlite, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlite.Close() })
	for index, target := range []db.Storage{postgres, sqlite} {
		t.Run([]string{"postgres", "sqlite"}[index], func(t *testing.T) {
			require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), target))
			require.Empty(t, relayRowsForBinding(t, target, revoked.Enrollment.RelayBindingUID), "a revoked grant's rows are not restored")
			require.Equal(t, liveRows, relayRowsForBinding(t, target, live.Enrollment.RelayBindingUID), "a live grant's rows are restored exactly")
			enrollments, err := target.ListFederationEnrollments(ctx)
			require.NoError(t, err)
			revokedRestored := false
			for _, enrollment := range enrollments {
				if enrollment.RelayBindingUID == revoked.Enrollment.RelayBindingUID {
					revokedRestored = enrollment.RevokedAt != nil
				}
			}
			require.True(t, revokedRestored, "the revoked grant itself is restored as revoked")
		})
	}
}

func relayRowsForBinding(t *testing.T, store db.Storage, bindingUID string) []db.ImportRecord {
	t.Helper()
	rows := []db.ImportRecord{}
	for record, err := range store.ExportRelayState(context.Background()) {
		require.NoError(t, err)
		switch value := record.(type) {
		case *db.RelayOutboxExport:
			if value.BindingUID == bindingUID {
				rows = append(rows, record)
			}
		case *db.RelayInboxExport:
			if value.BindingUID == bindingUID {
				rows = append(rows, record)
			}
		case *db.RelayCursorExport:
			if value.BindingUID == bindingUID {
				rows = append(rows, record)
			}
		}
	}
	return rows
}
