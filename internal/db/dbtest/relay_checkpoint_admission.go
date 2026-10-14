package dbtest

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCheckpointAdmissionMatchesReset proves that a relay enrollment the
// capacity admits can receive its reset checkpoint. Reset creation under a
// root-attribution context signs receipts for the records it captures and
// writes them with the enrolling account's actor, so the admission estimate
// must count both; otherwise a project near the capacity enrolls a relay that
// can never recover.
func RunRelayCheckpointAdmissionMatchesReset(t *testing.T, backend Backend) {
	require.NotNil(t, backend.OpenWithRelayCheckpointCapacity, "backend must open a store with a lowered capacity")
	const actor = "company-member-with-a-long-account-name"
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	// Build the project once at the default capacity to learn its estimate,
	// then rebuild it in a store whose capacity leaves no room for a missing
	// receipt or actor.
	build := func(store db.Storage) (db.Project, db.RootAttributionSigner, int64) {
		ctx := t.Context()
		signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
		project, err := store.CreateProject(ctx, "near-capacity-project")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		attributed := db.WithRootAttribution(ctx, signer, actor)
		for _, title := range []string{"First retained issue", "Second retained issue"} {
			_, _, err := store.CreateIssue(attributed, db.CreateIssueParams{ProjectID: project.ID, Title: title, Body: strings.Repeat("y", 1500), Author: "assistant"})
			require.NoError(t, err)
		}
		parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "admission-parent-test-token", Actor: actor, AdminActor: "admin"})
		require.NoError(t, err)
		return project, signer, parent.ID
	}
	enroll := func(store db.Storage, project db.Project, parentID int64) db.CreatedFederationEnrollment {
		grant, err := store.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parentID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "admission-relay-test-token"})
		require.NoError(t, err, "the project fits the capacity by its own estimate")
		return grant
	}
	reference := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, reference.Close()) })
	project, _, parentID := build(reference)
	// The status measurement bounds the actor by the project's live relay
	// enrollments, so enroll the same account before taking the reference.
	enroll(reference, project, parentID)
	measurer, ok := reference.(db.RelayCheckpointMeasurer)
	require.True(t, ok, "native checkpoint measurement is required")
	estimate, err := measurer.RelayCheckpointSize(t.Context(), project.ID)
	require.NoError(t, err)

	// The rebuilt project's estimate differs from the reference by a few bytes
	// of timestamp encoding, so the capacity allows exactly that variation.
	// It cannot hide an underestimate: the bound below compares the reset
	// with its own store's estimate and allows no slack.
	const rebuildVariation = 16
	bounded := backend.OpenWithRelayCheckpointCapacity(t, estimate.Bytes+rebuildVariation)
	creator, ok := bounded.(db.RelayResetStore)
	require.True(t, ok, "native root checkpoint generation is required")
	boundedMeasurer, ok := bounded.(db.RelayCheckpointMeasurer)
	require.True(t, ok)
	project, signer, parentID := build(bounded)
	ctx := t.Context()
	grant := enroll(bounded, project, parentID)
	drainRelayDeliveries(t, bounded, grant.Enrollment.RelayBindingUID)
	// The estimate that admits a reset is the one taken immediately before
	// it; the reset then writes its own records and receipts.
	preReset, err := boundedMeasurer.RelayCheckpointSize(ctx, project.ID)
	require.NoError(t, err)
	checkpoint, err := creator.CreateRelayReset(db.WithRootAttribution(ctx, signer, actor), grant.Enrollment.RelayBindingUID, signer)
	require.NoError(t, err, "a reset the admission promised must fit the same capacity")
	require.GreaterOrEqual(t, preReset.Bytes, db.RootResetSnapshotBytes(checkpoint.Snapshot), "the estimate bounds the signed checkpoint from above")
	_, provenance, err := db.DecodeRootResetPayload(db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: bounded.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, checkpoint.Snapshot)
	require.NoError(t, err)
	require.Greater(t, len(provenance.Receipts), 2, "capture signs receipts for the records it writes")
}

// drainRelayDeliveries acknowledges every pending event and receipt delivery
// for a binding so later operations do not wait on it.
func drainRelayDeliveries(t *testing.T, store db.Storage, bindingUID string) {
	t.Helper()
	ctx := t.Context()
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		for {
			batch, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 100)
			require.NoError(t, err)
			if len(batch) == 0 {
				break
			}
			last := batch[len(batch)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, 1, stream, last.Sequence, last.Digest))
		}
	}
}
