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
	t.Cleanup(func() { require.NoError(t, locked.Close()) })
	conn, err := locked.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=DELETE`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`)
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var value int
	err = raw.QueryRowContext(t.Context(), `SELECT 1 FROM projects LIMIT 1`).Scan(&value)
	require.True(t, sqlitestore.IsTransient(err), "exclusive lock must block the reader: %v", err)
	releaseSQLiteWriteLockAfter(t.Context(), t, conn, 100*time.Millisecond)

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

func TestFederationMoveSourceLifecycleQueryCount(t *testing.T) {
	t.Cleanup(sqlitestore.CountStatementsForTest())
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := t.Context()
	var projects []db.Project
	for _, name := range []string{"source", "destination", "parked"} {
		p, err := s.CreateProject(ctx, name)
		require.NoError(t, err)
		projects = append(projects, p)
		if name != "parked" {
			_, err = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: p.ID, HubProjectUID: p.UID, Actor: "tester", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
		}
	}
	addMove := func() {
		issue, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moved", Author: "tester"})
		require.NoError(t, err)
		_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "tester"})
		require.NoError(t, err)
		_, err = s.ExecContext(ctx, `UPDATE issues SET project_id=$1 WHERE id=$2`, projects[2].ID, issue.ID)
		require.NoError(t, err)
	}
	var counter sqlcount.Counter
	count := func() int64 {
		counted := counter.Context(ctx)
		tx, err := s.BeginTx(counted, nil)
		require.NoError(t, err)
		counter.Reset()
		require.NoError(t, db.CheckFederationMoveSourceLifecycle(counted, tx, projects[0].ID, s.InstanceUID(), false))
		got := counter.Count()
		require.NoError(t, tx.Rollback())
		return got
	}
	addMove()
	one := count()
	for range 9 {
		addMove()
	}
	require.Equal(t, one, count())
}
