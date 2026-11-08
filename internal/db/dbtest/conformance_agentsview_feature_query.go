package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/db"
)

// Feature negotiation remains scoped to each project across different event
// ID layouts, and reads transaction-local changes from the caller's snapshot.
func checkAgentsViewFeatureQueryLayouts(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	query := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	root := store.InstanceUID()
	dense, err := store.CreateProject(ctx, "dense-project")
	require.NoError(t, err)
	first, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: dense.ID, Title: "dense issue", Author: "worker"})
	require.NoError(t, err)
	_, closed, _, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: first.ID, Reason: "done", Actor: "worker"})
	require.NoError(t, err)
	require.NotEmpty(t, closed)
	var lo, hi sql.NullInt64
	var count int64
	readBounds := func(projectID int64) {
		require.NoError(t, query.QueryRowContext(ctx, `SELECT MIN(id), MAX(id), COUNT(*) FROM events WHERE project_id=$1`, projectID).Scan(&lo, &hi, &count))
	}
	readBounds(dense.ID)
	require.Positive(t, count)
	require.True(t, lo.Valid && hi.Valid)
	require.Equal(t, hi.Int64, lo.Int64+(count-1), "first layout must be dense")
	association := agentsviewrelay.Association{Version: 1, IssueUID: first.UID, ProjectUID: dense.UID, AuthorityUID: root, ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: root, ProjectUID: dense.UID}}}
	raw, err := json.Marshal(association)
	require.NoError(t, err)
	payload := `{"agentsview_\u0061ssociation":` + string(raw) + `}`
	for _, ordinary := range []string{
		`{}`,
		`{"agentsview_association":null}`,
		`{"nested":{"agentsview_association":{}}}`,
		`{"transcript":{"session_id":"agentsview_association"}}`,
	} {
		_, err = query.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, ordinary, closed[0].UID)
		require.NoError(t, err)
		features, err := db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
		require.NoError(t, err)
		require.Empty(t, features)
	}
	// Event-only requirements must still be detected when feature negotiation
	// inspects a project with no matching native cron projection.
	for _, tc := range []struct {
		eventType string
		feature   string
	}{
		{"cron.fixture", db.CronEventFeature},
		{"issue.moved", db.IssueMoveEventFeature},
	} {
		_, err = query.ExecContext(ctx, `UPDATE events SET type=$1, payload='{}' WHERE uid=$2`, tc.eventType, closed[0].UID)
		require.NoError(t, err)
		features, err := db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
		require.NoError(t, err)
		require.Contains(t, strings.Split(features, ","), tc.feature)
	}
	_, err = query.ExecContext(ctx, `UPDATE events SET type='issue.closed' WHERE uid=$1`, closed[0].UID)
	require.NoError(t, err)
	_, err = query.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, payload, closed[0].UID)
	require.NoError(t, err)
	features, err := db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Equal(t, agentsviewrelay.EventFeature, features)
	_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: dense.ID, Title: "ordering fixture", Author: "worker"})
	require.NoError(t, err)
	for _, event := range []db.Event{created, later} {
		_, err = query.ExecContext(ctx, `UPDATE events SET type='issue.moved' WHERE uid=$1`, event.UID)
		require.NoError(t, err)
		features, err = db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
		require.NoError(t, err)
		require.Contains(t, strings.Split(features, ","), agentsviewrelay.EventFeature)
		require.Contains(t, strings.Split(features, ","), db.IssueMoveEventFeature)
		_, err = query.ExecContext(ctx, `UPDATE events SET type=$1 WHERE uid=$2`, event.Type, event.UID)
		require.NoError(t, err)
	}
	// Interleave a second project, then append another event in the first. The
	// second project's history must not inherit the first project's requirement.
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	for range 20 {
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "other issue", Author: "worker"})
		require.NoError(t, err)
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: dense.ID, Title: "later issue", Author: "worker"})
	require.NoError(t, err)
	readBounds(dense.ID)
	require.Greater(t, hi.Int64, lo.Int64+(count-1), "interleaved layout must be sparse")
	features, err = db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Equal(t, agentsviewrelay.EventFeature, features)
	features, err = db.ProjectRequiredEventFeatures(ctx, query, other.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Empty(t, features)
	// A project with no retained events must select neither SQLite traversal.
	empty, err := store.CreateProject(ctx, "empty-project")
	require.NoError(t, err)
	_, err = query.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, empty.ID)
	require.NoError(t, err)
	readBounds(empty.ID)
	require.Zero(t, count)
	features, err = db.ProjectRequiredEventFeatures(ctx, query, empty.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Empty(t, features)
	tx, err := query.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, `{"transcript":{"session_id":"agentsview_association"}}`, closed[0].UID)
	require.NoError(t, err)
	features, err = db.ProjectRequiredEventFeatures(ctx, tx, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Empty(t, features)
	require.NoError(t, tx.Rollback())
	features, err = db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Equal(t, agentsviewrelay.EventFeature, features)
	return nil
}
