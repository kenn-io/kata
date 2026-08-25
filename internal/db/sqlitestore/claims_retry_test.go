package sqlitestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
	sqlite3 "modernc.org/sqlite/lib"
)

func newLeaseRetryStore(t *testing.T) (*Store, db.Project, db.Issue) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	project, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "lease retry subject", Author: "tester",
	})
	require.NoError(t, err)
	return d, project, issue
}

func newLeasePrincipal(t *testing.T, holder string) db.ClaimPrincipal {
	t.Helper()
	instanceUID, err := katauid.New()
	require.NoError(t, err)
	return db.ClaimPrincipal{
		HolderInstanceUID: instanceUID, Holder: holder, ClientKind: "agent",
	}
}

func countClaimExpired(events []db.Event) int {
	n := 0
	for _, evt := range events {
		if evt.Type == "claim.expired" {
			n++
		}
	}
	return n
}

// TestAcquireClaimRetryDoesNotDuplicateExpiryEvents pins that a lease attempt
// rolled back by a transient failure contributes none of its events to the
// result. The expiry events of a rolled-back attempt were never committed, so
// broadcasting them would emit phantom SSE frames and phantom hook deliveries.
func TestAcquireClaimRetryDoesNotDuplicateExpiryEvents(t *testing.T) {
	ctx := context.Background()
	d, project, issue := newLeaseRetryStore(t)
	start := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: newLeasePrincipal(t, "holder-a"),
		ClaimKind: "timed", TTL: time.Minute, Now: start,
	})
	require.NoError(t, err)

	original := commitClaimTx
	commits := 0
	commitClaimTx = func(ctx context.Context, conn *sql.Conn) error {
		commits++
		if commits == 1 {
			return codedSQLiteErr(sqlite3.SQLITE_BUSY)
		}
		return original(ctx, conn)
	}
	t.Cleanup(func() { commitClaimTx = original })

	result, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: newLeasePrincipal(t, "holder-b"),
		ClaimKind: "hard", Now: start.Add(2 * time.Minute),
	})

	require.NoError(t, err)
	require.Equal(t, 2, commits, "the busy commit must have forced exactly one retry")
	assert.Equal(t, 1, countClaimExpired(result.Events),
		"the rolled-back attempt's claim.expired event must not survive into the result")

	var expiredRows int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE project_id = ? AND type = 'claim.expired'`,
		project.ID).Scan(&expiredRows))
	assert.Equal(t, 1, expiredRows, "only the committed attempt's expiry may persist")
}

// TestRenewClaimRetriedIntoSuccessDoesNotReturnExpired pins that the
// "committed but failed" signal is per-attempt. An attempt that saw the claim
// expire, then lost its commit to a busy lock and retried into a clean renew,
// must report success.
func TestRenewClaimRetriedIntoSuccessDoesNotReturnExpired(t *testing.T) {
	ctx := context.Background()
	d, project, issue := newLeaseRetryStore(t)
	start := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	holder := newLeasePrincipal(t, "holder-a")

	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: holder, ClaimKind: "timed", TTL: time.Minute, Now: start,
	})
	require.NoError(t, err)

	original := commitClaimTx
	commits := 0
	commitClaimTx = func(ctx context.Context, conn *sql.Conn) error {
		commits++
		if commits > 1 {
			return original(ctx, conn)
		}
		// End the failing attempt here so the follow-up write lands outside
		// it, push the expiry out of reach, then report a busy condition so
		// RetryTransient re-runs the closure against the refreshed row.
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE issue_claims SET expires_at = ? WHERE issue_id = ? AND released_at IS NULL`,
			start.Add(time.Hour).UTC().Format(sqliteTimeFormat), issue.ID); err != nil {
			return err
		}
		return codedSQLiteErr(sqlite3.SQLITE_BUSY)
	}
	t.Cleanup(func() { commitClaimTx = original })

	result, err := d.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: holder, TTL: time.Minute, Now: start.Add(2 * time.Minute),
	})

	require.NoError(t, err,
		"a renew retried into a clean success must not return the rolled-back attempt's ErrClaimExpired")
	require.Equal(t, 2, commits, "the busy commit must have forced exactly one retry")
	assert.True(t, result.Granted)
	assert.Zero(t, countClaimExpired(result.Events),
		"the rolled-back attempt's expiry event must not survive into the result")
}
