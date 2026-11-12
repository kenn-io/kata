package pgstore_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRehomeSnapshotsCachesProjectBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "snapshot-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true,
	})
	require.NoError(t, err)
	tx, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	var trackCounts string
	require.NoError(t, tx.QueryRowContext(ctx, `SHOW track_counts`).Scan(&trackCounts))
	require.Equal(t, "on", trackCounts, "scan-count regression guard requires PostgreSQL statistics")
	countScans := func() int64 {
		var scans int64
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT seq_scan+COALESCE(idx_scan,0) FROM pg_stat_xact_user_tables WHERE relid='federation_bindings'::regclass`).Scan(&scans))
		return scans
	}
	before := countScans()
	for range 3 {
		var joined bool
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)`, project.ID).Scan(&joined))
		require.True(t, joined)
	}
	require.GreaterOrEqual(t, countScans()-before, int64(3), "calibration must observe real binding scans")
	events := make([]db.FoldEvent, 1200)
	for i := range events {
		uid := fmt.Sprintf("%026d", i+100000)
		_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,author,status) VALUES($1,$2,$3,'baseline','worker','open')`, uid, project.ID, fmt.Sprintf("%08d", i+100000))
		require.NoError(t, err)
		events[i] = db.FoldEvent{UID: fmt.Sprintf("%026d", i+200000), IssueUID: uid, ProjectUID: project.UID, Type: "issue.snapshot", HLCPhysicalMS: int64(i + 1), Payload: []byte(fmt.Sprintf(`{"uid":%q,"title":"baseline","status":"open"}`, uid))}
	}
	projection := db.FoldEvents(events)
	before = countScans()
	_, err = db.RehomeFederatedMovedIssues(ctx, tx, events, projection, project.ID, []int64{project.ID}, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	delta := countScans() - before
	t.Logf("binding scans for %d snapshots: %d", len(events), delta)
	require.LessOrEqual(t, delta, int64(5), "binding facts should be shared across snapshots in the same project")
}
