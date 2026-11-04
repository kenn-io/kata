package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Contract: committed deletion alone invalidates cached relay history, including
// an enrollment which never had a checkpoint. No post-commit refresh is needed.
func checkCronPruneRelayReset(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "prune-root")
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: fixture.project.ID, Name: "Review", Actor: "operator", Definition: def})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for i := range 2 {
		id, err := uid.New()
		require.NoError(t, err)
		_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status,created_at,updated_at) VALUES($1,$2,$3,$4,'operator','succeeded',$5,$5)`, id, fixture.project.ID, job.UID, job.DefinitionEventUID, time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC).Format(db.CronRunCreatedAtFormat))
		require.NoError(t, err)
	}
	var grants [2]db.FederationEnrollment
	var initial db.RelayResetCheckpoint
	replica := backend.Open(t)
	t.Cleanup(func() { _ = replica.Close() })
	var replicaProject db.Project
	for i := range grants {
		peer, err := uid.New()
		require.NoError(t, err)
		if i == 0 {
			peer = replica.InstanceUID()
		}
		created, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, ServeDownstream: i == 0})
		require.NoError(t, err)
		grants[i] = created.Enrollment
		_, err = store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: fixture.project.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1, DryRun: true})
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "a preview cannot discard pending relay delivery")
		if i == 0 {
			// Leave only the prepared checkpoint to exercise the reset guard.
			drainRelayGrant(t, store, grants[i].RelayBindingUID, grants[i].RelayResetEpoch)
			initial, err = store.(db.RelayResetStore).CreateRelayReset(ctx, grants[i].RelayBindingUID, fixture.signer)
			require.NoError(t, err)
			_, err = store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: fixture.project.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1})
			require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "a prepared reset blocks pruning until activation")
			_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, initial.Translation.Authority.Epoch), grants[i].RelayBindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
		}
	}
	replicaProject = installPruneReplica(t, replica, store, fixture.project, grants[0].RelayBindingUID, initial, true)
	leaf := backend.Open(t)
	t.Cleanup(func() { _ = leaf.Close() })
	parentToken, err := uid.New()
	require.NoError(t, err)
	parent, _, err := replica.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: parentToken, Actor: "relay-member", AdminActor: "admin"})
	require.NoError(t, err)
	child, err := replica.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: replicaProject.ID, ParentTokenID: parent.ID, SpokeInstanceUID: leaf.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion})
	require.NoError(t, err)
	drainRelayGrant(t, replica, child.Enrollment.RelayBindingUID, child.Enrollment.RelayResetEpoch)
	forwarded, err := replica.(db.RelayResetStore).CreateRelayReset(ctx, child.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.NoError(t, err)
	leafProject := installPruneReplica(t, leaf, replica, replicaProject, child.Enrollment.RelayBindingUID, forwarded, false)
	_, err = replica.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch), child.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	drainRelayGrant(t, replica, child.Enrollment.RelayBindingUID, forwarded.Translation.Authority.Epoch)
	for i, grant := range grants {
		epoch := int64(1)
		if i == 0 {
			epoch = initial.Translation.Authority.Epoch
		}
		drainRelayGrant(t, store, grant.RelayBindingUID, epoch)
	}
	bootstrap := store.(db.RelayResetBootstrapStore)
	for range 2 {
		for _, grant := range grants {
			required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.RelayBindingUID)
			require.NoError(t, err)
			require.False(t, required)
		}
	}
	pruned, err := store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: fixture.project.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1})
	require.NoError(t, err)
	require.Equal(t, 1, pruned.Pruned)
	for _, grant := range grants {
		required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.RelayBindingUID)
		require.NoError(t, err)
		require.True(t, required, "committed reset must bypass the warm history cache")
	}
	fresh, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grants[0].RelayBindingUID, fixture.signer)
	require.NoError(t, err)
	require.NotEqual(t, initial.Manifest.SnapshotUID, fresh.Manifest.SnapshotUID)
	retry, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grants[0].RelayBindingUID, fixture.signer)
	require.NoError(t, err)
	require.Equal(t, fresh, retry, "captured boundary does not endlessly refresh")
	events, _, err := db.DecodeRootResetPayload(mustRootPin(t, store, fixture.project.UID), fresh.Snapshot)
	require.NoError(t, err)
	distinct := map[string]bool{}
	for _, e := range events {
		if e.Type == "cron.run.snapshot" {
			var observation db.CronRunObservation
			require.NoError(t, json.Unmarshal([]byte(e.Payload), &observation))
			distinct[observation.UID] = true
		}
	}
	require.Len(t, distinct, 1, "superseded snapshots of the kept UID remain valid history")
	require.NoError(t, replica.(db.RelayResetStore).InstallRelayReset(ctx, grants[0].RelayBindingUID, fresh.Manifest, fresh.Snapshot, fresh.Translation))
	forwarded, err = replica.(db.RelayResetStore).CreateRelayReset(ctx, child.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.NoError(t, leaf.(db.RelayResetStore).InstallRelayReset(ctx, child.Enrollment.RelayBindingUID, forwarded.Manifest, forwarded.Snapshot, forwarded.Translation))
	for _, target := range []struct {
		store   db.Storage
		project db.Project
	}{{replica, replicaProject}, {leaf, leafProject}} {
		retained, err := target.store.ListCronRuns(ctx, db.CronRunList{ProjectID: target.project.ID})
		require.NoError(t, err)
		require.Len(t, retained, 1)
		require.True(t, distinct[retained[0].UID])
	}

	return nil
}

func mustRootPin(t *testing.T, store db.Storage, projectUID string) db.RootKeyPin {
	t.Helper()
	pin, err := store.RootAuthority(t.Context(), projectUID)
	require.NoError(t, err)
	return pin
}

// installPruneReplica starts with the old checkpoint too, proving replacement
// clears existing rows at both the relay and its leaf.
func installPruneReplica(t *testing.T, target, upstream db.Storage, source db.Project, binding string, checkpoint db.RelayResetCheckpoint, serve bool) db.Project {
	t.Helper()
	ctx := t.Context()
	project, err := target.CreateProjectWithUID(ctx, "replica-project", source.UID)
	require.NoError(t, err)
	pin, err := upstream.RootAuthority(ctx, source.UID)
	require.NoError(t, err)
	require.NoError(t, target.PinRootAuthority(ctx, pin))
	_, err = target.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: source.ID, HubProjectUID: source.UID, Actor: "relay-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	path := []string{pin.AuthorityUID}
	if upstream.InstanceUID() != pin.AuthorityUID {
		path = append(path, upstream.InstanceUID())
	}
	path = append(path, target.InstanceUID())
	_, err = target.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: binding, AuthorityUID: pin.AuthorityUID, UpstreamInstanceUID: upstream.InstanceUID(), HubPath: path, LocalActor: "relay-member", ServeDownstream: serve, ResetEpoch: 1})
	require.NoError(t, err)
	require.NoError(t, target.(db.RelayResetStore).InstallRelayReset(ctx, binding, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))
	runs, err := target.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	return project
}
