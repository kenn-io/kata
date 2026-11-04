package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayCheckpointHistoryValidationCache checks activated root checkpoints
// reuse validation independently for each grant, scan only new ordinary events,
// and retain the existing identity-change, purge and live-authority fences.
func RunRelayCheckpointHistoryValidationCache(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "checkpoint-cache")
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Cached root issue", Author: "member"})
	require.NoError(t, err)
	var grants [2]db.FederationEnrollment
	for i := range grants {
		peer, err := uid.New()
		require.NoError(t, err)
		created, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
			ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID,
			SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion,
		})
		require.NoError(t, err)
		grant := created.Enrollment
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.RelayBindingUID, fixture.signer)
		require.NoError(t, err)
		_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), grant.RelayBindingUID, db.RelayStreamEvent, 100)
		require.NoError(t, err)
		grants[i] = grant
	}
	fullScans := 0
	require.NotNil(t, backend.InstallRelayHistoryFullScanObserver)
	t.Cleanup(backend.InstallRelayHistoryFullScanObserver(store, func() { fullScans++ }))
	check := func() {
		t.Helper()
		for _, grant := range grants {
			required, err := store.(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grant.RelayBindingUID)
			require.NoError(t, err)
			require.False(t, required)
		}
	}
	check()
	require.Equal(t, 2, fullScans, "each retained checkpoint validates its own history boundary once")
	check()
	check()
	require.Equal(t, 2, fullScans, "alternating grants with different checkpoints must reuse unchanged history")
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "member", Body: "New ordinary history"})
	require.NoError(t, err)
	check()
	check()
	require.Equal(t, 2, fullScans, "ordinary history advances each grant's validated cursor without a full scan")

	other, err := store.CreateProject(ctx, "unrelated-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "New identity", Author: "member"})
	require.NoError(t, err)
	check()
	require.Equal(t, 2, fullScans, "unrelated creations preserve both retained-checkpoint validations")
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: other.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: other.ID, Actor: "operator"})
	require.NoError(t, err)
	check()
	check()
	require.Equal(t, 4, fullScans, "purge invalidation remains active for retained checkpoints")

	unrelated, err := store.CreateProject(ctx, "snapshot-project")
	require.NoError(t, err)
	snapshotOther := newRemoteEvent(t, unrelated, &issue.UID, "issue.snapshot", "member", grants[0].SpokeInstanceUID, 650, jsontext.Value(`{}`))
	_, err = store.InsertRemoteEvents(ctx, unrelated.ID, []db.RemoteEvent{snapshotOther})
	require.NoError(t, err)
	check()
	require.Equal(t, 4, fullScans, "unrelated snapshots preserve both checkpoint validations")

	// A snapshot in the checkpoints' own project requires fresh validation.
	snapshot := newRemoteEvent(t, fixture.project, &issue.UID, "issue.snapshot", "member", grants[0].SpokeInstanceUID, 700, jsontext.Value(`{}`))
	_, err = store.InsertRemoteEvents(ctx, fixture.project.ID, []db.RemoteEvent{snapshot})
	require.NoError(t, err)
	bootstrap := store.(db.RelayResetBootstrapStore)
	_, _, err = requireWriteDuringRelayScan(t, store, backend, fixture.sibling.ID, func() (bool, error) {
		_, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grants[1].RelayBindingUID, fixture.signer)
		return false, err
	}, func() {
		otherKeyCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := bootstrap.RelayEnrollmentNeedsReset(otherKeyCtx, grants[0].RelayBindingUID)
		require.NoError(t, err, "another cache key must progress during the scan")
	})
	require.NoError(t, err)
	require.NoError(t, store.RevokeFederationEnrollment(ctx, grants[0].ID))
	_, err = store.(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grants[0].RelayBindingUID)
	require.ErrorIs(t, err, db.ErrNotFound, "cached validation cannot bypass revocation")
}
