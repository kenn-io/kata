package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayLocalDetach exercises detaching a relay replica whose upstream hub
// is gone for good. Undeliverable hop state for that binding is discarded,
// the project and its issues stay, and active downstream enrollments block it.
func RunRelayLocalDetach(t *testing.T, store db.Storage) {
	t.Run("discards undeliverable hop state", func(t *testing.T) {
		ctx := t.Context()
		project, config, _, _, child, _ := prepareRevokedRelayOutbox(t, store, "local-detach")
		revoker, ok := store.(db.RelaySelfRevoker)
		require.True(t, ok)
		require.NoError(t, revoker.RevokeOwnRelayEnrollment(ctx, child.Token, project.ID, child.Enrollment.SpokeInstanceUID))
		require.Empty(t, relayStateForBinding(t, store, child.Enrollment.RelayBindingUID), "self-revocation removes the downstream grant's rows")
		local, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Never delivered upstream", Author: "local-member"})
		require.NoError(t, err)
		pending, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
		require.NoError(t, err)
		require.NotEmpty(t, pending)
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "an ordinary leave still waits for delivery")
		require.NotEmpty(t, relayStateForBinding(t, store, config.BindingUID))

		detacher, ok := store.(db.RelayLocalDetacher)
		require.True(t, ok)
		result, err := detacher.DetachRelayReplicaLocally(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, db.FederationRoleSpoke, result.Role)
		require.Equal(t, project.UID, result.ProjectUID)

		_, err = store.FederationBindingByProject(ctx, project.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
		require.Empty(t, relayStateForBinding(t, store, config.BindingUID))
		kept, err := store.IssueByID(ctx, local.ID)
		require.NoError(t, err)
		require.Equal(t, "Never delivered upstream", kept.Title)
		_, err = store.ProjectByUID(ctx, project.UID)
		require.NoError(t, err)

		again, err := detacher.DetachRelayReplicaLocally(ctx, project.ID)
		require.NoError(t, err)
		require.Empty(t, again.Role, "a repeated detach finds nothing left to remove")
	})
	t.Run("refuses active downstream enrollments", func(t *testing.T) {
		ctx := t.Context()
		project, config, _, _, _, _ := prepareRevokedRelayOutbox(t, store, "local-detach-downstream")
		before := relayStateForBinding(t, store, config.BindingUID)
		require.NotEmpty(t, before)
		detacher, ok := store.(db.RelayLocalDetacher)
		require.True(t, ok)
		_, err := detacher.DetachRelayReplicaLocally(ctx, project.ID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByDownstream)
		binding, err := store.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		require.NotNil(t, binding.RelayConfig)
		require.Equal(t, before, relayStateForBinding(t, store, config.BindingUID))
	})
}

// relayStateForBinding returns every exported outbox, inbox and cursor record
// that belongs to one relay binding.
func relayStateForBinding(t *testing.T, store db.Storage, bindingUID string) []db.ImportRecord {
	t.Helper()
	records := []db.ImportRecord{}
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		switch value := record.(type) {
		case *db.RelayOutboxExport:
			if value.BindingUID == bindingUID {
				records = append(records, record)
			}
		case *db.RelayInboxExport:
			if value.BindingUID == bindingUID {
				records = append(records, record)
			}
		case *db.RelayCursorExport:
			if value.BindingUID == bindingUID {
				records = append(records, record)
			}
		}
	}
	return records
}
