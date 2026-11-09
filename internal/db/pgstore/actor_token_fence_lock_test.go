package pgstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Another transaction can hold the token row while its expiry passes. The
// fence that waited for that row must not admit the expired token afterwards.
func TestActorTokenFenceRechecksExpiryAfterRowLock(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	expiresAt := time.Now().UTC().Add(time.Second)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	token, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "fence-expiry-test-token", Actor: "worker", AdminActor: "admin", ExpiresAt: &expiresAt})
	require.NoError(t, err)

	holder, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var holderPID int
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
	_, err = holder.ExecContext(ctx, `SELECT 1 FROM api_tokens WHERE id=$1 FOR SHARE`, token.ID)
	require.NoError(t, err)

	fenced := make(chan error, 1)
	go func() {
		tx, err := store.BeginTx(ctx, nil)
		if err != nil {
			fenced <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		fenced <- db.ActorTokenTransactionFence(token)(ctx, tx)
	}()
	//nolint:kennlint // sleeptest: PostgreSQL lock observation requires native server I/O outside a synctest bubble.
	require.Eventually(t, func() bool {
		var blocked bool
		require.NoError(t, store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked))
		return blocked
	}, 5*time.Second, 10*time.Millisecond, "the fence waits on the held token row")
	expired := time.NewTimer(time.Until(expiresAt) + 10*time.Millisecond)
	<-expired.C
	require.NoError(t, holder.Commit())
	select {
	case err := <-fenced:
		require.ErrorIs(t, err, db.ErrNotFound, "a token that expired while the fence waited is refused")
	case <-time.After(5 * time.Second):
		t.Fatal("the fence did not resume after the row lock was released")
	}
}
