package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayHistoryOrder exercises retained mutations whose introduction arrives
// only in a later enable snapshot. Both native stores must offer a signed reset
// instead of replaying that invalid prefix to a fresh child.
func RunRelayHistoryOrder(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "late-introduction-project")
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Original issue", Author: "source-agent"})
	require.NoError(t, err)
	title := "Retained mutation before snapshot"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Actor: "source-agent", Title: &title})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "root-member")
	require.NoError(t, err)
	// Model an introduction withheld at a historical project boundary. Keep the
	// mutation and later snapshot exactly as stored, without rewriting hashes.
	query := "DELETE FROM events WHERE id=?"
	if backend.Name == "postgres" {
		query = "DELETE FROM events WHERE id=$1"
	}
	executor := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = executor.ExecContext(ctx, query, created.ID)
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "root-member", AdminActor: "admin", PlaintextToken: "history-parent-test-token"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000003", ProtocolVersion: db.RelayProtocolVersion, Token: "history-child-test-token"})
	require.NoError(t, err)
	bootstrap := store.(db.RelayResetBootstrapStore)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.True(t, required, "a late snapshot cannot repair the retained mutation prefix")
	offers, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Empty(t, offers, "unsafe history must not be seeded before signed reset")
	checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, title, current.Title)
}
