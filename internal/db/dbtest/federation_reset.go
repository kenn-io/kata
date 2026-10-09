package dbtest

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunFederationResetIgnoresRejectedPendingClaims verifies terminal claim
// requests no longer block lifecycle cleanup after they stop awaiting a hub.
func RunFederationResetIgnoresRejectedPendingClaims(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "terminal-claim-project")
	require.NoError(t, err)
	issue, issueEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Retried request", Author: "member",
	})
	require.NoError(t, err)
	pending, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(), Holder: "bound-actor", ClientKind: "cli",
		},
		ClaimKind: "hard", Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, store.RejectPendingClaim(ctx, pending.RequestUID, "lease denied by hub", time.Now().UTC()))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 9, PullCursorEventID: 8, PushEnabled: true,
		PushCursorEventID: issueEvent.ID, Actor: "bound-actor", Enabled: true,
	})
	require.NoError(t, err)
	const rootUID = "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}))
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion,
		BindingUID:      "00000000000000000000000005", UpstreamInstanceUID: rootUID,
		AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()},
		LocalActor: "bound-actor", ServeDownstream: true, ResetEpoch: 1,
	})
	require.NoError(t, err)
	count, err := store.CountPendingClaims(ctx, project.ID)
	require.NoError(t, err)
	require.Zero(t, count, "rejected rows are no longer pending claim work")

	lifecycle, ok := store.(interface {
		ValidateRelayLifecycle(context.Context, int64) error
		FenceRelayDisconnect(context.Context, int64) error
	})
	require.True(t, ok, "native relay lifecycle guards are required")
	require.NoError(t, lifecycle.ValidateRelayLifecycle(ctx, project.ID),
		"a rejected claim must not block federation archive/reset cleanup")
	require.NoError(t, lifecycle.FenceRelayDisconnect(ctx, project.ID),
		"a rejected claim must not block relay disconnect")
}
