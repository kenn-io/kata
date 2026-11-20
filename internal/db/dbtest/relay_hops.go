package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func setupRelayHop(t *testing.T, upstream, replica db.Storage, project db.Project, name string, downstream bool) (string, int64) {
	t.Helper()
	ctx := t.Context()
	policy, err := upstream.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	parent, _, err := upstream.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "member", AdminActor: "admin", TeamUIDs: policy.TeamUIDs,
		PlaintextToken: name + "-parent-test-token",
	})
	require.NoError(t, err)
	grant, err := upstream.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: replica.InstanceUID(),
		ProtocolVersion: db.RelayProtocolVersion, ServeDownstream: downstream,
		Token: name + "-relay-test-token",
	})
	require.NoError(t, err)
	local, err := replica.CreateProjectWithUID(ctx, name, project.UID)
	require.NoError(t, err)
	pin, err := upstream.RootAuthority(ctx, project.UID)
	require.NoError(t, err)
	require.NoError(t, replica.PinRootAuthority(ctx, pin))
	_, err = replica.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	path := []string{pin.AuthorityUID, replica.InstanceUID()}
	if upstream.InstanceUID() != pin.AuthorityUID {
		path = []string{pin.AuthorityUID, upstream.InstanceUID(), replica.InstanceUID()}
	}
	_, err = replica.SetRelayBindingConfig(ctx, local.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID,
		AuthorityUID: pin.AuthorityUID, UpstreamInstanceUID: upstream.InstanceUID(), HubPath: path,
		LocalActor: "member", ServeDownstream: downstream, ResetEpoch: grant.Enrollment.RelayResetEpoch,
	})
	require.NoError(t, err)
	return grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch
}

func syncRelayHop(features context.Context, t *testing.T, from, to db.Storage, binding string, epoch int64, streams ...string) {
	t.Helper()
	for _, stream := range streams {
		for {
			items, err := from.PendingRelayDeliveries(features, binding, stream, 100)
			require.NoError(t, err)
			if len(items) == 0 {
				break
			}
			if to != nil {
				_, err = to.AcceptRelayDeliveries(t.Context(), binding, db.RelayBatch{Stream: stream, Envelopes: items})
				require.NoError(t, err)
			}
			last := items[len(items)-1]
			require.NoError(t, from.AckRelayDeliveries(t.Context(), binding, epoch, stream, last.Sequence, last.Digest))
		}
	}
}
