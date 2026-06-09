package sqlitestore_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestWriteMethods_RetryTransientSQLiteBusy(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *testing.T) (*sqlitestore.Store, string, func(context.Context, *testing.T))
	}{
		{
			name: "direct write",
			run: func(ctx context.Context, t *testing.T) (*sqlitestore.Store, string, func(context.Context, *testing.T)) {
				d, path := openTestDBWithPath(t)
				p := createProject(ctx, t, d, "direct-write")
				return d, path, func(ctx context.Context, t *testing.T) {
					renamed, err := d.RenameProject(ctx, p.ID, "direct-write-renamed")
					require.NoError(t, err)
					assert.Equal(t, "direct-write-renamed", renamed.Name)
				}
			},
		},
		{
			name: "transaction write",
			run: func(ctx context.Context, t *testing.T) (*sqlitestore.Store, string, func(context.Context, *testing.T)) {
				d, path := openTestDBWithPath(t)
				p := createProject(ctx, t, d, "transaction-write")
				issue := makeIssue(t, ctx, d, p.ID, "needs comment", "tester")
				return d, path, func(ctx context.Context, t *testing.T) {
					comment, evt, err := d.CreateComment(ctx, db.CreateCommentParams{
						IssueID: issue.ID,
						Author:  "tester",
						Body:    "retry this comment",
					})
					require.NoError(t, err)
					assert.Equal(t, "retry this comment", comment.Body)
					assert.Equal(t, "issue.commented", evt.Type)
				}
			},
		},
		{
			name: "claim status refresh error write",
			run: func(ctx context.Context, t *testing.T) (*sqlitestore.Store, string, func(context.Context, *testing.T)) {
				d, path := openTestDBWithPath(t)
				p := createProject(ctx, t, d, "claim-status-write")
				issue := makeIssue(t, ctx, d, p.ID, "claimed", "tester")
				return d, path, func(ctx context.Context, t *testing.T) {
					err := d.MarkClaimStatusRefreshError(ctx, p.ID, issue.UID, 503, "remote busy", time.Now().UTC())
					require.NoError(t, err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			d, path, run := tt.run(ctx, t)
			useFastSQLiteBusyTimeout(ctx, t, d)
			lockConn := holdSQLiteWriteLock(ctx, t, path)
			releaseSQLiteWriteLockAfter(ctx, t, lockConn, 50*time.Millisecond)

			run(ctx, t)
		})
	}
}

func useFastSQLiteBusyTimeout(ctx context.Context, t *testing.T, d *sqlitestore.Store) {
	t.Helper()
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)
	_, err := d.ExecContext(ctx, `PRAGMA busy_timeout=1`)
	require.NoError(t, err)
}

func holdSQLiteWriteLock(ctx context.Context, t *testing.T, path string) *sql.Conn {
	t.Helper()
	lockDB, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lockDB.Close() })
	conn, err := lockDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE TRANSACTION")
	require.NoError(t, err)
	return conn
}

func releaseSQLiteWriteLockAfter(ctx context.Context, t *testing.T, conn *sql.Conn, delay time.Duration) {
	t.Helper()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			_, _ = conn.ExecContext(ctx, "COMMIT")
		})
	}
	timer := time.AfterFunc(delay, release)
	t.Cleanup(func() {
		if timer.Stop() {
			release()
		}
	})
}
