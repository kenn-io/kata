package sqlitestore_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv/sqlcount"
)

func TestQuietFederationPollRetriesBusyRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	setup, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	project, err := setup.CreateProject(t.Context(), "quiet")
	require.NoError(t, err)
	_, err = setup.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	cursor, err := setup.MaxEventID(t.Context())
	require.NoError(t, err)
	require.NoError(t, setup.Close())

	locked, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	require.NoError(t, err)
	conn, err := locked.Conn(t.Context())
	require.NoError(t, err)
	released := make(chan error, 1)
	var timer *time.Timer
	t.Cleanup(func() {
		if timer != nil && !timer.Stop() {
			require.NoError(t, <-released)
		} else {
			require.NoError(t, conn.Close())
			require.NoError(t, locked.Close())
		}
	})
	_, err = conn.ExecContext(t.Context(), `PRAGMA locking_mode=EXCLUSIVE`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`)
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var value int
	err = raw.QueryRowContext(t.Context(), `SELECT 1 FROM projects LIMIT 1`).Scan(&value)
	require.True(t, sqlitestore.IsTransient(err), "exclusive lock must block the reader: %v", err)
	timer = time.AfterFunc(100*time.Millisecond, func() {
		_, err := conn.ExecContext(t.Context(), `COMMIT`)
		if err == nil {
			err = conn.Close()
		}
		if err == nil {
			err = locked.Close()
		}
		released <- err
	})

	reader := &sqlitestore.Store{DB: raw}
	page, err := reader.ReadFederation(t.Context(), db.FederationReadParams{ProjectID: project.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 100})
	require.NoError(t, err)
	require.Empty(t, page.Events)
	require.Equal(t, cursor, page.NextAfterID)
}

func TestQuietFederationPollTransactions(t *testing.T) {
	t.Cleanup(sqlitestore.CountStatementsForTest())
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	quiet, err := s.CreateProject(t.Context(), "quiet")
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(t.Context(), quiet.ID, "tester")
	require.NoError(t, err)
	cursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	var count sqlcount.Counter
	ctx := count.Context(t.Context())
	in := db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 100}
	page, err := s.ReadFederation(ctx, in)
	require.NoError(t, err)
	require.Empty(t, page.Events)
	require.Zero(t, count.Begins())
}
