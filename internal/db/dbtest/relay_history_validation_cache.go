package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayEnrollmentHistoryValidationCache verifies validation is reused when
// the source history is unchanged and invalidated by new source boundaries or
// purge markers. Enrollment authority remains live on every call.
func RunRelayEnrollmentHistoryValidationCache(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "relay-history-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	publicKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: store.InstanceUID(),
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Original relay history", Author: "example-assistant",
	})
	require.NoError(t, err)
	parentToken, err := db.NewFederationToken()
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "example-account", AdminActor: "admin", PlaintextToken: parentToken,
	})
	require.NoError(t, err)
	peerUID, err := uid.New()
	require.NoError(t, err)
	grantToken, err := db.NewFederationToken()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peerUID,
		ProtocolVersion: db.RelayProtocolVersion, Token: grantToken,
	})
	require.NoError(t, err)

	fullScans := 0
	require.NotNil(t, backend.InstallRelayHistoryFullScanObserver)
	resetObserver := backend.InstallRelayHistoryFullScanObserver(store, func() { fullScans++ })
	t.Cleanup(resetObserver)
	bootstrap := store.(db.RelayResetBootstrapStore)
	for range 2 {
		required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		require.False(t, required)
	}
	require.Equal(t, 1, fullScans, "unchanged history should reuse its validated cursor and result")

	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "example-assistant", Body: "New relay event",
	})
	require.NoError(t, err)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.False(t, required)
	require.Equal(t, 1, fullScans, "new events should be inspected after the cached history cursor")

	other, err := store.CreateProject(ctx, "unrelated-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: other.ID, Title: "New endpoint", Author: "example-assistant",
	})
	require.NoError(t, err)
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.False(t, required)
	require.Equal(t, 2, fullScans, "new issue endpoints invalidate history whose references may resolve differently")

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: other.ID, Actor: "example-operator", Force: true,
	})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{
		ProjectID: other.ID, Actor: "example-operator",
	})
	require.NoError(t, err)
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.False(t, required)
	require.Equal(t, 3, fullScans, "purge markers invalidate previously validated history")

	require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
	_, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.ErrorIs(t, err, db.ErrNotFound, "live enrollment authority is checked even when history is cached")
}
