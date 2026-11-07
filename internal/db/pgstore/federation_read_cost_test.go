package pgstore_test

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kit/search/sqlquery"
)

// Quiet polls must cost only the project's own history and issue seeks,
// with the normal planner and the same projection used by ReadFederation.
func TestReadFederationQuietPageBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "quiet-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Quiet issue", Author: "tester"})
	require.NoError(t, err)
	busy, err := store.CreateProject(t.Context(), "busy-project")
	require.NoError(t, err)
	after, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	// Keep autoanalyze from racing the explicit before/after statistics checks.
	_, err = store.ExecContext(t.Context(), `ALTER TABLE events SET (autovacuum_enabled=false)`)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT lpad(x::text,26,'0'),$1,$2,'busy-project','project.metadata_updated','tester','{}',1,0,repeat('a',64)
 FROM generate_series(1,200000) n(x)`, store.InstanceUID(), busy.ID)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES('00000000000000000000999999',$1,$2,'quiet-project',$3,'sync.reset_required','tester','{}',1,0,$4)`, store.InstanceUID(), project.ID, issue.UID, strings.Repeat("a", 64))
	require.NoError(t, err)

	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err = store.ExecContext(t.Context(), `ANALYZE`)
			require.NoError(t, err)
		}
		for _, subject := range []string{"", issue.UID} {
			t.Run(fmt.Sprintf("analyzed=%t/issue=%t", analyzed, subject != ""), func(t *testing.T) {
				tx, err := store.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				var visited float64
				events, err := db.ReadFederationEventPage(t.Context(), tx, project, subject, after, 1000, true, explainedEventPage(t, tx, &visited))
				require.NoError(t, err)
				require.Empty(t, events)
				t.Logf("%.0f event rows visited", visited)
				require.LessOrEqual(t, visited, float64(1200), "quiet polls must skip unrelated event history")
			})
		}
	}
}

// explainedEventPage runs each page query with the production projection and
// adds the event rows its plan visited.
func explainedEventPage(t *testing.T, tx *sql.Tx, visited *float64) func(string, ...any) ([]db.Event, error) {
	return func(where string, args ...any) ([]db.Event, error) {
		query := pgstore.EventSelectForTest + where //nolint:gosec // G202: helper-built SQL; values stay bound.
		var raw []byte
		require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE, FORMAT JSON) `+query, args...).Scan(&raw))
		var plans []struct{ Plan federationPagePlan }
		require.NoError(t, json.Unmarshal(raw, &plans))
		*visited += plans[0].Plan.visitedEvents()
		return (sqlquery.Query{SQL: query, Args: args}).All(t.Context(), tx, func(rows *sql.Rows) (db.Event, error) { return pgstore.ScanEventForTest(rows) })
	}
}

type federationPagePlan struct {
	Relation  string               `json:"Relation Name"`
	Rows      float64              `json:"Actual Rows"`
	Loops     float64              `json:"Actual Loops"`
	Filtered  float64              `json:"Rows Removed by Filter"`
	Rechecked float64              `json:"Rows Removed by Index Recheck"`
	Plans     []federationPagePlan `json:"Plans"`
}

func (p federationPagePlan) visitedEvents() float64 {
	var rows float64
	if p.Relation == "events" {
		rows = (p.Rows + p.Filtered + p.Rechecked) * p.Loops
	}
	for _, child := range p.Plans {
		rows += child.visitedEvents()
	}
	return rows
}

func TestReadFederationBulkMovePageBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	for _, interleaved := range []bool{false, true} {
		t.Run(fmt.Sprintf("interleaved=%t", interleaved), func(t *testing.T) {
			store, _ := openConformanceStore(t)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			project, after := dbtest.SeedFederatedReadBulkHistory(t, store, interleaved)
			tx, err := store.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			var visited float64
			events, err := db.ReadFederationEventPage(t.Context(), tx, project, "", after, 1, true, explainedEventPage(t, tx, &visited))
			require.NoError(t, err)
			require.Len(t, events, 1)
			stride := int64(1)
			if interleaved {
				stride = 2
			}
			require.Equal(t, after+stride, events[0].ID)
			t.Logf("%.0f event rows visited", visited)
			require.LessOrEqual(t, visited, float64(16), "bulk histories must cost a small multiple of the page size")
		})
	}
}
