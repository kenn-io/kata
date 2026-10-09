package pgstore_test

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// R3/R4: revocation cannot commit while an admitted transaction retains the
// parent credential's authority. This observes an actual PostgreSQL row wait,
// without assuming a delay means the revoker has reached the database.
func TestRelayParentRevocationWaitsForTransaction(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "company-member", AdminActor: "admin", PlaintextToken: "race-parent-test-token"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "race-relay-test-token"})
	require.NoError(t, err)
	tx, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, store.FederationEnrollmentTransactionFence(grant.Enrollment, project.ID, "push")(ctx, tx))
	var holderPID int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
	completed := make(chan error, 1)
	go func() { _, _, err := store.RevokeAPIToken(ctx, parent.ID, "admin"); completed <- err }()
	blocked := false
	early := false
	//nolint:kennlint // PostgreSQL lock observation requires native server I/O outside a synctest bubble.
	require.Eventually(t, func() bool {
		select {
		case err := <-completed:
			require.NoError(t, err)
			early = true
			return true
		default:
		}
		err := store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked)
		require.NoError(t, err)
		return blocked
	}, 5*time.Second, 10*time.Millisecond)
	require.False(t, early, "parent revocation committed while the relay transaction still held admission")
	require.True(t, blocked)
	require.NoError(t, tx.Commit())
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("revocation did not resume after transaction commit")
	}
	_, err = store.AuthorizeFederationToken(ctx, grant.Token, project.ID, "pull")
	require.Error(t, err)
}
