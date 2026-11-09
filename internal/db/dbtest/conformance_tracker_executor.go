package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

// Contract: executor metadata can exist on a spoke without a sync binding;
// registry authority, exact predecessor and restored-history fences apply to
// genesis and dispatch. Completion acknowledgement needs no provider grant.
func checkTrackerExecutorAdmission(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "executor-project")
	require.NoError(t, err)
	root := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	selection := tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}
	reservation := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: root, ProjectUID: project.UID, Connection: selection, Sequence: 1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", SourceKey: "notion:source", Locator: "item", Target: "closed", ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2", Phase: db.TrackerWritePrepared, Remote: &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB3", ArgumentsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootSequence: 1, OwnerSequence: 1, RootEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2", OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB4"}}
	reservation.WriteUID, err = db.TrackerStatusWriteUID(root, project.UID, reservation.SourceKey, reservation.Locator, reservation.PendingEventUID, reservation.Target)
	require.NoError(t, err)
	executor := store.(db.TrackerExecutorStore)
	_, err = executor.PrepareTrackerExecutor(ctx, reservation, "notion", epoch)
	require.ErrorIs(t, err, tracker.ErrUnavailable, "transport enrollment is not registry authority")
	_, err = store.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, epoch, []tracker.RegistryPolicy{{Selection: selection, Provider: "notion", Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}})
	require.NoError(t, err)
	prepared, err := executor.PrepareTrackerExecutor(ctx, reservation, "notion", epoch)
	require.NoError(t, err)
	again, err := executor.PrepareTrackerExecutor(ctx, reservation, "notion", epoch)
	require.NoError(t, err)
	require.Equal(t, prepared, again, "preparation delivery is idempotent")
	// Contract P6: owner visibility can be revoked after preparation. The
	// retained request fence must recheck actual team membership in the native
	// admission transaction, on both backends, leaving the exact preparation.
	access := store.(db.ProjectAccessStorage)
	team, _, err := access.CreateTeam(ctx, "executor-team", "admin")
	require.NoError(t, err)
	_, err = access.SetTeamMembership(ctx, team.UID, "owner-service", true, "admin")
	require.NoError(t, err)
	policy, err := access.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	policy.Visibility, policy.TeamUIDs = "teams", []string{team.UID}
	_, _, err = access.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	admitted := db.WithAdditionalTransactionFence(db.WithAuthorizedProjects(ctx, []string{project.UID}), access.ProjectAccessTransactionFence("owner-service", []string{project.UID}))
	_, err = access.SetTeamMembership(ctx, team.UID, "owner-service", false, "admin")
	require.NoError(t, err)
	_, err = executor.AdmitTrackerExecutor(admitted, prepared, "notion", epoch)
	require.ErrorIs(t, err, db.ErrNotFound)
	unchanged, found, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, root, project.UID, "executor")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, prepared, unchanged, "denied admission cannot consume the one-use preparation")
	_, err = access.SetTeamMembership(ctx, team.UID, "owner-service", true, "admin")
	require.NoError(t, err)
	dispatched, err := executor.AdmitTrackerExecutor(admitted, prepared, "notion", epoch)
	require.NoError(t, err)
	_, err = executor.AdmitTrackerExecutor(ctx, prepared, "notion", epoch)
	require.Error(t, err, "one exact CAS consumes the permit")
	confirmed := dispatched
	confirmed.Phase = db.TrackerWriteConfirmed
	confirmed.ProviderAcknowledged = true
	confirmed.Result, err = json.Marshal(db.TrackerStatusResult{Status: "closed", Version: time.Unix(1900000000, 0).UTC()})
	require.NoError(t, err)
	require.NoError(t, store.(db.TrackerFenceStore).StoreTrackerExecutionFence(ctx, &dispatched, confirmed, epoch))
	// Removing provider grants/credentials must not strand original receipt ack.
	_, err = store.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, epoch, nil)
	require.NoError(t, err)
	checkpoint := db.TrackerOwnerCheckpoint{Version: 1, RootInstanceUID: root, ProjectUID: project.UID, OwnerInstanceUID: store.InstanceUID(), Selection: selection, Sequence: 1, Head: db.TrackerFenceHead(confirmed), Attempt: *confirmed.Remote, WriteUID: confirmed.WriteUID, Disposition: db.TrackerWriteAcknowledged, ResultDigest: tracker.WorkerArgumentsDigest(confirmed.Result)}
	require.NoError(t, executor.AcknowledgeTrackerExecutor(ctx, checkpoint))
	require.NoError(t, executor.AcknowledgeTrackerExecutor(ctx, checkpoint))
	head, found, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, root, project.UID, "executor")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, db.TrackerWriteAcknowledged, head.Phase)
	_, err = store.IssueSyncBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "executor did not create a spoke sync binding")

	// The runtime's final native fence rejects a disconnected/missing replica
	// in the same transaction, even when the provider registry is otherwise live.
	expected := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, Enabled: true, PushEnabled: true, Actor: "owner-service", HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: project.UID, RelayConfig: &db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "01ARZ3NDEKTSV4RRFFQ69G5FB5", UpstreamInstanceUID: root, AuthorityUID: root, HubPath: []string{root, store.InstanceUID()}, LocalActor: "owner-service", ResetEpoch: 1}}
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root, KeyID: "fixture-key", PublicKey: make([]byte, 32)}
	fenced := db.WithAdditionalTransactionFence(ctx, db.TrackerExecutorTransportFence(store.InstanceUID(), project.UID, expected, pin))
	_, _, err = store.CreateIssue(fenced, db.CreateIssueParams{ProjectID: project.ID, Title: "Must not be created", Author: "owner-service"})
	require.ErrorIs(t, err, db.ErrTrackerFenceChanged)
	initial := expected
	initial.RelayConfig = nil
	_, err = store.UpsertFederationBinding(ctx, initial)
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin.PublicKey, pin.KeyID = public, db.RootPublicKeyID(public)
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.SetRelayBindingConfig(ctx, project.ID, *expected.RelayConfig)
	require.NoError(t, err)
	beginner := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	check := db.TrackerExecutorTransportFence(store.InstanceUID(), project.UID, expected, pin)
	require.NoError(t, check(ctx, tx), "the persisted root pin permits the same live route")
	wrong := pin
	wrong.PublicKey = append([]byte(nil), pin.PublicKey...)
	wrong.PublicKey[0] ^= 1
	require.ErrorIs(t, db.TrackerExecutorTransportFence(store.InstanceUID(), project.UID, expected, wrong)(ctx, tx), db.ErrTrackerFenceChanged)
	return nil
}
