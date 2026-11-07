package sqlitestore

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
	"modernc.org/sqlite"
)

// A one-issue batch must not reallocate short IDs and reconcile projections for
// every unrelated issue. The budget permits the remaining feature/history SQL
// scans; it catches the much larger per-issue replay cost without timing noise.
func TestFederationIngestUnrelatedHistoryPageBudget(t *testing.T) {
	d, p, issue := incrementalTestStore(t)
	d.SetMaxOpenConns(1)
	pages := func(reset bool) int {
		conn, err := d.Conn(t.Context())
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		total := 0
		require.NoError(t, conn.Raw(func(raw any) error {
			for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
				n, _, err := raw.(*queuedConn).inner.(sqlite.DBStatus).Status(op, reset)
				if err != nil {
					return err
				}
				total += n
			}
			return nil
		}))
		return total
	}
	var small int
	for _, history := range []int{100, 10_000} {
		seedFederationBenchmarkHistory(t, d, p, history)
		ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":"batch-%d"}`, issue.UID, history), int64(9_000_001_000_000+history))
		pages(true)
		result, err := d.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID, Events: []db.FederationIngestEvent{{SourceEventID: int64(history), Event: ev}}})
		require.NoError(t, err)
		require.Equal(t, 1, result.Accepted)
		count := pages(false)
		t.Logf("history=%d: %d page fetches", history, count)
		if history == 100 {
			small = count
		} else {
			require.LessOrEqual(t, count, small+2_000, "one-issue ingest must not replay every unrelated issue")
		}
	}
}

// Snapshot links may reference issues whose creation has not arrived yet. A
// large reference set must retain that admission behavior without exceeding
// SQLite's bound-parameter limit.
func TestFederationIngestKnownIssuesWithLargeSnapshot(t *testing.T) {
	d, p, issue := incrementalTestStore(t)
	var payload strings.Builder
	fmt.Fprintf(&payload, `{"uid":%q,"links":[`, issue.UID)
	for i := range 32_768 {
		if i > 0 {
			payload.WriteByte(',')
		}
		peer, err := katauid.New()
		require.NoError(t, err)
		fmt.Fprintf(&payload, `{"type":"related","to_issue_uid":%q}`, peer)
	}
	payload.WriteString(`]}`)
	ev := incrementalRemoteEvent(t, p, issue.UID, "issue.snapshot", payload.String(), 9_000_001_000_000)
	tx, err := d.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	known, err := federationIngestKnownIssueUIDSet(t.Context(), tx, p.ID, []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}})
	require.NoError(t, err)
	require.Contains(t, known, issue.UID)
	require.Len(t, known, 1)
}

// Pushes have no event-count cap. Preparation must also avoid SQLite's bind
// limit when many distinct issue subjects appear in a single batch.
func TestFederationPreparationLargeIssueScope(t *testing.T) {
	d, p, _ := incrementalTestStore(t)
	prepared := make([]preparedFederationIngestEvent, 32_768)
	for i := range prepared {
		uid := fmt.Sprintf("%026d", i+1)
		prepared[i] = preparedFederationIngestEvent{Event: db.RemoteEvent{
			EventUID: fmt.Sprintf("%026d", i+40_000), ProjectUID: p.UID,
			IssueUID: &uid, Type: "issue.updated", Actor: "tester",
			Payload:       []byte(fmt.Sprintf(`{"issue_uid":%q,"title":"update"}`, uid)),
			HLCPhysicalMS: 9_000_001_000_000 + int64(i),
		}}
	}
	tx, err := d.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	_, err = prepareFederationMaterializationTx(t.Context(), tx, p.ID, prepared)
	require.NoError(t, err)
}
