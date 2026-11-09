package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayUnrelatedCheckpointCache verifies checkpoint capture in another project preserves cached history validation.
func RunRelayUnrelatedCheckpointCache(t *testing.T, backend Backend) {
	for _, retained := range []bool{false, true} {
		name := "initial history"
		if retained {
			name = "retained checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			var fixtures [2]relayLinkBoundaryFixture
			var grants [2]db.FederationEnrollment
			for i, projectName := range []string{"cached-project", "checkpoint-project"} {
				fixtures[i] = newRelayLinkBoundaryFixture(t, store, projectName)
				_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixtures[i].project.ID, Title: "Local history", Author: "member"})
				require.NoError(t, err)
				peer, err := uid.New()
				require.NoError(t, err)
				grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
					ProjectID: fixtures[i].project.ID, ParentTokenID: fixtures[i].parentTokenID,
					SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion,
				})
				require.NoError(t, err)
				grants[i] = grant.Enrollment
			}
			if retained {
				checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grants[0].RelayBindingUID, fixtures[0].signer)
				require.NoError(t, err)
				_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), grants[0].RelayBindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
			}
			bootstrap := store.(db.RelayResetBootstrapStore)
			cached, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grants[0].RelayBindingUID)
			require.NoError(t, err)
			require.False(t, cached)
			fullScans := 0
			defer backend.InstallRelayHistoryFullScanObserver(store, func() { fullScans++ })()

			// This already-federated project has no checkpoint for its binding.
			_, err = store.(db.RelayResetStore).CreateRelayReset(ctx, grants[1].RelayBindingUID, fixtures[1].signer)
			require.NoError(t, err)
			cached, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grants[0].RelayBindingUID)
			require.NoError(t, err)
			fresh, err := backend.OpenSame(t, store).(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grants[0].RelayBindingUID)
			require.NoError(t, err)
			require.False(t, fresh)
			require.Equal(t, fresh, cached, "checkpoint capture elsewhere preserves the reset decision")
			require.Zero(t, fullScans, "checkpoint capture elsewhere preserves incremental validation")
		})
	}
}
