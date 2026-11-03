package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestFederationMoveHistoryFillsPageAcrossCoveredHistory(t *testing.T) {
	pool := sql.OpenDB(referenceConnector{&referenceWork{}})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	for _, query := range []string{
		`CREATE TABLE events(id INTEGER PRIMARY KEY,issue_uid TEXT,type TEXT,payload TEXT)`,
		`CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL`,
		`WITH RECURSIVE sequence(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM sequence WHERE id<1005)
 INSERT INTO events SELECT id,'01M40000000000000000000004',
 CASE id WHEN 2 THEN 'issue.moved' WHEN 1003 THEN 'issue.snapshot' WHEN 1005 THEN 'issue.snapshot' ELSE 'issue.updated' END,
 '{}' FROM sequence`,
	} {
		_, err := pool.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	readPage := func(afterID int64, limit int) ([]Event, error) {
		rows, err := tx.QueryContext(t.Context(), `SELECT id,issue_uid,type,payload FROM events WHERE id>$1 ORDER BY id LIMIT $2`, afterID, limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var events []Event
		for rows.Next() {
			var event Event
			if err := rows.Scan(&event.ID, &event.IssueUID, &event.Type, &event.Payload); err != nil {
				return nil, err
			}
			events = append(events, event)
		}
		return events, rows.Err()
	}
	page, err := ReadFederationMoveHistory(t.Context(), tx, 0, 2, readPage)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, int64(1003), page[0].ID, "the first baseline supersedes more than one candidate page")
	require.Equal(t, int64(1004), page[1].ID, "a later snapshot must not hide the intervening delta")
	page, err = ReadFederationMoveHistory(t.Context(), tx, page[1].ID, 1, readPage)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, int64(1005), page[0].ID, "the returned cursor resumes without skipping later events")
}

func TestFederationMoveHistoryUsesLaterCoveringBaseline(t *testing.T) {
	pool := sql.OpenDB(referenceConnector{&referenceWork{}})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	for _, query := range []string{
		`CREATE TABLE events(id INTEGER PRIMARY KEY,issue_uid TEXT,type TEXT,payload TEXT)`,
		`CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL`,
		`INSERT INTO events VALUES
 (1,'01M40000000000000000000004','issue.snapshot','{}'),
 (2,'01M40000000000000000000004','issue.moved','{}'),
 (3,'01M40000000000000000000004','issue.snapshot','{}'),
 (4,'01M40000000000000000000004','issue.updated','{}'),
 (5,'01M40000000000000000000004','issue.snapshot','{}'),
 (6,'01M40000000000000000000004','issue.moved','{"snapshot":{}}'),
 (7,'01M40000000000000000000004','issue.moved','{}')`,
	} {
		_, err := pool.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	readPage := func(afterID int64, limit int) ([]Event, error) {
		rows, err := tx.QueryContext(t.Context(), `SELECT id,issue_uid,type,payload FROM events WHERE id>$1 ORDER BY id LIMIT $2`, afterID, limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var events []Event
		for rows.Next() {
			var event Event
			if err := rows.Scan(&event.ID, &event.IssueUID, &event.Type, &event.Payload); err != nil {
				return nil, err
			}
			events = append(events, event)
		}
		return events, rows.Err()
	}
	page, err := ReadFederationMoveHistory(t.Context(), tx, 0, 100, readPage)
	require.NoError(t, err)
	var ids []int64
	for _, event := range page {
		ids = append(ids, event.ID)
	}
	require.Equal(t, []int64{3, 4, 5, 6, 7}, ids, "the first covering later baseline excludes the standalone move, retains later deltas/native moves and leaves an uncovered trailing move visible")
}

func TestFederationMoveHistoryCutsSnapshotBearingMoveAtLaterBaseline(t *testing.T) {
	pool := sql.OpenDB(referenceConnector{&referenceWork{}})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	for _, query := range []string{
		`CREATE TABLE events(id INTEGER PRIMARY KEY,issue_uid TEXT,type TEXT,payload TEXT)`,
		`CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL`,
		`INSERT INTO events VALUES
 (1,'01M40000000000000000000004','issue.moved','{"snapshot":{}}'),
 (2,'01M40000000000000000000004','issue.snapshot','{}'),
 (3,'01M40000000000000000000004','issue.updated','{}'),
 (4,'01M40000000000000000000004','issue.moved','{"snapshot":{}}'),
 (5,'01M40000000000000000000004','issue.updated','{}')`,
	} {
		_, err := pool.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	readPage := func(afterID int64, limit int) ([]Event, error) {
		rows, err := tx.QueryContext(t.Context(), `SELECT id,issue_uid,type,payload FROM events WHERE id>$1 ORDER BY id LIMIT $2`, afterID, limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var events []Event
		for rows.Next() {
			var event Event
			if err := rows.Scan(&event.ID, &event.IssueUID, &event.Type, &event.Payload); err != nil {
				return nil, err
			}
			events = append(events, event)
		}
		return events, rows.Err()
	}

	page, err := ReadFederationMoveHistory(t.Context(), tx, 0, 100, readPage)
	require.NoError(t, err)
	var ids []int64
	for _, event := range page {
		ids = append(ids, event.ID)
	}
	require.Equal(t, []int64{2, 3, 4, 5}, ids,
		"a later enrollment baseline retires an older snapshot-bearing move without hiding subsequent history")
}

func TestFederationMoveHistoryRetiresSnapshotMovesAcrossInterveningUpdates(t *testing.T) {
	ctx := t.Context()
	hegel.Test(t, func(ht *hegel.T) {
		interveningUpdates := hegel.Draw(ht, hegel.Integers(0, 8))
		pool := sql.OpenDB(referenceConnector{&referenceWork{}})
		pool.SetMaxOpenConns(1)
		defer func() {
			if err := pool.Close(); err != nil {
				ht.Fatalf("close history database: %v", err)
			}
		}()
		if _, err := pool.ExecContext(ctx, `CREATE TABLE events(id INTEGER PRIMARY KEY,issue_uid TEXT,type TEXT,payload TEXT)`); err != nil {
			ht.Fatalf("create history table: %v", err)
		}
		if _, err := pool.ExecContext(ctx, `CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL`); err != nil {
			ht.Fatalf("index issue history: %v", err)
		}

		const issueUID = "01M40000000000000000000004"
		insertEvent := func(id int, eventType, payload string) {
			if _, err := pool.ExecContext(ctx,
				`INSERT INTO events VALUES($1,$2,$3,$4)`, id, issueUID, eventType, payload,
			); err != nil {
				ht.Fatalf("insert history event %d: %v", id, err)
			}
		}
		insertEvent(1, "issue.moved", `{"snapshot":{}}`)
		for id := 2; id < interveningUpdates+2; id++ {
			insertEvent(id, "issue.updated", `{}`)
		}
		baselineID := interveningUpdates + 2
		insertEvent(baselineID, "issue.snapshot", `{}`)
		insertEvent(baselineID+1, "issue.updated", `{}`)

		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			ht.Fatalf("begin history read: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		readPage := func(afterID int64, limit int) ([]Event, error) {
			rows, err := tx.QueryContext(ctx,
				`SELECT id,issue_uid,type,payload FROM events WHERE id>$1 ORDER BY id LIMIT $2`,
				afterID, limit,
			)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			var events []Event
			for rows.Next() {
				var event Event
				if err := rows.Scan(&event.ID, &event.IssueUID, &event.Type, &event.Payload); err != nil {
					return nil, err
				}
				events = append(events, event)
			}
			return events, rows.Err()
		}
		page, err := ReadFederationMoveHistory(ctx, tx, 0, baselineID+1, readPage)
		if err != nil {
			ht.Fatalf("read federation move history: %v", err)
		}
		for _, event := range page {
			if event.ID == 1 {
				ht.Fatalf("snapshot-bearing move survived later baseline after %d intervening updates", interveningUpdates)
			}
		}
	}, hegel.WithTestCases(32))
}
