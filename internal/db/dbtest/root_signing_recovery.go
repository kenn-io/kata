package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func rootRecoveryHub(t *testing.T, store db.Storage, name string) db.Project {
	t.Helper()
	project, err := store.CreateProject(t.Context(), name)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	return project
}

func rootRecoveryKey(t *testing.T, store db.Storage, projectUID string) (db.RootKeyPin, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return db.RootKeyPin{ProjectUID: projectUID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, private
}

// RunRootSigningKeyUnavailable exercises writes when the daemon cannot sign for
// a local root project. A missing or mismatched key refuses the attributed
// write with an error naming the project and recovery command, never commits
// an unsigned event, and leaves projects that need no root receipt writable.
func RunRootSigningKeyUnavailable(t *testing.T, store db.Storage) {
	ctx := t.Context()
	hub := rootRecoveryHub(t, store, "hub-project")
	pin, _ := rootRecoveryKey(t, store, hub.UID)
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	standalone, err := store.CreateProject(ctx, "standalone-project")
	require.NoError(t, err)
	_, otherKey := rootRecoveryKey(t, store, hub.UID)
	for name, signer := range map[string]db.RootAttributionSigner{
		"missing":    {AuthorityUID: store.InstanceUID()},
		"mismatched": {AuthorityUID: store.InstanceUID(), PrivateKey: otherKey},
	} {
		t.Run(name, func(t *testing.T) {
			writeCtx := db.WithRootAttribution(ctx, signer, "member")
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			_, _, err = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: hub.ID, Title: "Needs a receipt", Author: "assistant"})
			keyErr, ok := errors.AsType[*db.RootSigningKeyError](err)
			require.True(t, ok, "want RootSigningKeyError, got %v", err)
			require.Equal(t, "hub-project", keyErr.Project)
			require.ErrorContains(t, err, "kata federation root-key repin --project hub-project")
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after, "a refused receipt must not leave its source event committed")
			_, _, err = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: standalone.ID, Title: "Local work", Author: "assistant"})
			require.NoError(t, err, "projects without root signing stay writable")
		})
	}
	fresh := rootRecoveryHub(t, store, "fresh-hub-project")
	_, _, err = store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID()}, "member"), db.CreateIssueParams{ProjectID: fresh.ID, Title: "First signed write", Author: "assistant"})
	keyErr, ok := errors.AsType[*db.RootSigningKeyError](err)
	require.True(t, ok, "want RootSigningKeyError, got %v", err)
	require.Equal(t, "fresh-hub-project", keyErr.Project)
	_, err = store.RootAuthority(ctx, fresh.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "a missing key never pins an empty identity")
}

// RunRootKeyRepin exercises the explicit unsigned repin of a local root. The
// replacement becomes the active pin, the previous key stays retained for
// historical receipts, and stale, foreign, and previously retained keys are
// refused without changing the pin.
func RunRootKeyRepin(t *testing.T, store db.Storage) {
	repin, ok := store.(db.RootRepinStorage)
	require.True(t, ok, "native storage must support explicit root repinning")
	ctx := t.Context()
	hub := rootRecoveryHub(t, store, "hub-project")
	oldPin, oldKey := rootRecoveryKey(t, store, hub.UID)
	require.NoError(t, store.PinRootAuthority(ctx, oldPin))
	issue, _, err := store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: oldKey}, "member"), db.CreateIssueParams{ProjectID: hub.ID, Title: "Signed before loss", Author: "assistant"})
	require.NoError(t, err)
	historical, err := store.EntityAttribution(ctx, hub.UID, "issue", issue.UID)
	require.NoError(t, err)
	nextPin, nextKey := rootRecoveryKey(t, store, hub.UID)

	require.Error(t, repin.RepinRootAuthority(ctx, nextPin.KeyID, nextPin, false), "stale previous key must not replace the pin")
	foreignNext := nextPin
	foreignNext.AuthorityUID = "00000000000000000000000002"
	require.Error(t, repin.RepinRootAuthority(ctx, oldPin.KeyID, foreignNext, false), "repin cannot change the root authority")
	current, err := store.RootAuthority(ctx, hub.UID)
	require.NoError(t, err)
	require.Equal(t, oldPin, current, "refused repins leave the active pin unchanged")

	// The relay check runs inside the repin transaction, so an enrollment
	// granted after any earlier count still blocks an unaccepted unsigned repin.
	// An enrollment likewise verifies the pin its handshake advertised inside
	// its own transaction, so a repin between handshake and grant is refused.
	//nolint:gosec // These are deterministic fixture credentials, never operational secrets.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "repin-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	enroll := db.CreateRelayEnrollmentParams{ProjectID: hub.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "repin-relay-test-token"}
	enroll.RootKeyID = nextPin.KeyID
	_, err = store.CreateRelayEnrollment(ctx, enroll)
	require.ErrorIs(t, err, db.ErrRelayRootChanged, "a handshake pinned to a key that is no longer active cannot be granted")
	count, err := store.CountActiveFederationEnrollments(ctx, hub.ID)
	require.NoError(t, err)
	require.Zero(t, count)
	enroll.RootKeyID = oldPin.KeyID
	_, err = store.CreateRelayEnrollment(ctx, enroll)
	require.NoError(t, err)
	err = repin.RepinRootAuthority(ctx, oldPin.KeyID, nextPin, false)
	strands, ok := errors.AsType[*db.RootRepinStrandsRelaysError](err)
	require.True(t, ok, "want RootRepinStrandsRelaysError, got %v", err)
	require.Equal(t, int64(1), strands.Enrollments)
	current, err = store.RootAuthority(ctx, hub.UID)
	require.NoError(t, err)
	require.Equal(t, oldPin, current, "a refused repin leaves the active pin unchanged")

	require.NoError(t, repin.RepinRootAuthority(ctx, oldPin.KeyID, nextPin, true))
	require.NoError(t, repin.RepinRootAuthority(ctx, oldPin.KeyID, nextPin, true), "exact retry is idempotent")
	require.NoError(t, repin.RepinRootAuthority(ctx, oldPin.KeyID, nextPin, false), "a retry that changes nothing strands nobody")
	current, err = store.RootAuthority(ctx, hub.UID)
	require.NoError(t, err)
	require.Equal(t, nextPin, current)
	transitions, err := store.RootKeyTransitions(ctx, hub.UID)
	require.NoError(t, err)
	require.Empty(t, transitions, "an unsigned repin never fabricates a signed transition")
	retained, err := store.EntityAttribution(ctx, hub.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, historical, retained)
	require.NoError(t, db.VerifyRootReceipt(oldPin, retained), "the retired key still verifies history")

	comment, _, err := store.CreateComment(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: nextKey}, "member"), db.CreateCommentParams{IssueID: issue.ID, Author: "assistant", Body: "Signed after repin"})
	require.NoError(t, err)
	proof, err := store.EntityAttribution(ctx, hub.UID, "comment", comment.UID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(nextPin, proof))

	require.Error(t, repin.RepinRootAuthority(ctx, nextPin.KeyID, oldPin, false), "a retained key is history, never a fallback")
	foreign := rootRecoveryHub(t, store, "relay-project")
	foreignPin, _ := rootRecoveryKey(t, store, foreign.UID)
	foreignPin.AuthorityUID = "00000000000000000000000002"
	require.NoError(t, store.PinRootAuthority(ctx, foreignPin))
	replacement, _ := rootRecoveryKey(t, store, foreign.UID)
	replacement.AuthorityUID = foreignPin.AuthorityUID
	require.Error(t, repin.RepinRootAuthority(ctx, foreignPin.KeyID, replacement, false), "only the local root may repin its own projects")
}
