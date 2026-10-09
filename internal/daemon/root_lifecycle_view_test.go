package daemon_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R5 creation attribution survives soft-delete/restore and their no-op retries.
func TestAttributionLifecycleNoopProjection(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "root-project")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		_, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		ctx = db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}, "member")
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Root task", Author: "assistant"})
		require.NoError(t, err)
		require.Equal(t, "member", issue.AccountableActor, "creation projection prerequisite")
		deleted, _, changed, err := store.SoftDeleteIssue(ctx, issue.ID, "member")
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "member", deleted.AccountableActor)
		again, _, changed, err := store.SoftDeleteIssue(ctx, issue.ID, "member")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, "member", again.AccountableActor)
		require.Equal(t, "assistant", again.SourceActor)
		require.Equal(t, "verified", again.Verification)
		restored, _, changed, err := store.RestoreIssue(ctx, issue.ID, "member")
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "member", restored.AccountableActor)
		restored, _, changed, err = store.RestoreIssue(ctx, issue.ID, "member")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, "member", restored.AccountableActor)
	})
}
