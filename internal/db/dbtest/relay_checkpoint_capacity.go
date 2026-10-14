package dbtest

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCheckpointCapacity exercises the relay checkpoint capacity contract
// on the supplied native store. A root refuses new relay enrollments and
// checkpoint creation for a project whose signed checkpoint would exceed the
// capacity, reports the measured size, and leaves smaller projects unaffected.
func RunRelayCheckpointCapacity(t *testing.T, backend Backend) {
	require.NotNil(t, backend.OpenWithRelayCheckpointCapacity, "backend must open a store with a lowered capacity")
	const capacity = 8192
	store := backend.OpenWithRelayCheckpointCapacity(t, capacity)
	measurer, ok := store.(db.RelayCheckpointMeasurer)
	require.True(t, ok, "native checkpoint measurement is required")
	creator, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native root checkpoint generation is required")
	ctx := t.Context()
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	newRoot := func(name string) db.Project {
		project, err := store.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		return project
	}
	small := newRoot("small-project")
	large := newRoot("large-project")
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "capacity-parent-test-token", Actor: "company-member", AdminActor: "admin"})
	require.NoError(t, err)
	enroll := func(project db.Project, spoke, token string) (db.CreatedFederationEnrollment, error) {
		return store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: spoke, ProtocolVersion: db.RelayProtocolVersion, Token: token})
	}
	largeGrant, err := enroll(large, "00000000000000000000000008", "capacity-large-early-test-token")
	require.NoError(t, err, "a project enrolls while it still fits")
	largeIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: large.ID, Title: "Large body", Body: strings.Repeat("x", 3000), Author: "member"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: small.ID, Title: "Small body", Author: "member"})
	require.NoError(t, err)
	enrollmentsBefore, err := store.ListFederationEnrollments(ctx)
	require.NoError(t, err)

	smallSize, err := measurer.RelayCheckpointSize(ctx, small.ID)
	require.NoError(t, err)
	require.EqualValues(t, capacity, smallSize.Capacity)
	require.Positive(t, smallSize.Bytes)
	require.LessOrEqual(t, smallSize.Bytes, int64(capacity))
	// One 3000-byte body appears twice in a checkpoint: once in retained
	// history and once in the current-state record. Only both together, with
	// the receipts a capture signs, cross the capacity, so the measurement
	// must include current state.
	largeSize, err := measurer.RelayCheckpointSize(ctx, large.ID)
	require.NoError(t, err)
	require.EqualValues(t, capacity, largeSize.Capacity)
	require.Greater(t, largeSize.Bytes, int64(capacity))

	_, err = enroll(large, "00000000000000000000000009", "capacity-large-late-test-token")
	var tooLarge *db.RelayProjectTooLargeError
	require.ErrorAs(t, err, &tooLarge, "a project over capacity cannot enroll a new relay")
	require.NotNil(t, tooLarge)
	require.Equal(t, large.UID, tooLarge.ProjectUID)
	require.EqualValues(t, capacity, tooLarge.Capacity)
	require.Equal(t, largeSize.Bytes, tooLarge.Bytes)
	enrollmentsAfter, err := store.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Equal(t, enrollmentsBefore, enrollmentsAfter, "a refused enrollment leaves no grant behind")
	_, err = creator.CreateRelayReset(ctx, largeGrant.Enrollment.RelayBindingUID, signer)
	require.ErrorAs(t, err, &tooLarge, "an existing relay cannot receive a checkpoint the capacity forbids")
	require.NotNil(t, tooLarge)
	require.Equal(t, large.UID, tooLarge.ProjectUID)

	smallGrant, err := enroll(small, "00000000000000000000000009", "capacity-small-test-token")
	require.NoError(t, err, "a project under capacity still enrolls")
	checkpoint, err := creator.CreateRelayReset(ctx, smallGrant.Enrollment.RelayBindingUID, signer)
	require.NoError(t, err)
	require.LessOrEqual(t, db.RootResetSnapshotBytes(checkpoint.Snapshot), int64(capacity))

	// Purging the issue is the documented remedy: its history and current-state
	// record leave the checkpoint. Pending deliveries to the early relay are
	// acknowledged first because a purge waits for them.
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		for {
			batch, err := store.PendingRelayDeliveries(ctx, largeGrant.Enrollment.RelayBindingUID, stream, 100)
			require.NoError(t, err)
			if len(batch) == 0 {
				break
			}
			last := batch[len(batch)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, largeGrant.Enrollment.RelayBindingUID, 1, stream, last.Sequence, last.Digest))
		}
	}
	_, err = store.PurgeIssue(ctx, largeIssue.ID, "admin", nil)
	require.NoError(t, err)
	purgedSize, err := measurer.RelayCheckpointSize(ctx, large.ID)
	require.NoError(t, err)
	require.Less(t, purgedSize.Bytes, largeSize.Bytes/2, "a purge removes the issue's history and current-state record from the checkpoint")
	require.LessOrEqual(t, purgedSize.Bytes, int64(capacity))
	_, err = enroll(large, "00000000000000000000000009", "capacity-large-late-test-token")
	require.NoError(t, err, "the purged project enrolls a new relay again")
}
