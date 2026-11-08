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
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: dense.ID, Title: "dense issue", Author: "worker"})
	require.NoError(t, err)
	_, closed, _, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: first.ID, Reason: "done", Actor: "worker"})
	require.NoError(t, err)
	require.NotEmpty(t, closed)
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
		require.NotContains(t, features, agentsviewrelay.EventFeature)
	}
	_, err = query.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, payload, closed[0].UID)
	require.NoError(t, err)
	features, err := db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Contains(t, features, agentsviewrelay.EventFeature)
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
	features, err = db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Contains(t, features, agentsviewrelay.EventFeature)
	features, err = db.ProjectRequiredEventFeatures(ctx, query, other.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.NotContains(t, features, agentsviewrelay.EventFeature)
	tx, err := query.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, `{"transcript":{"session_id":"agentsview_association"}}`, closed[0].UID)
	require.NoError(t, err)
	features, err = db.ProjectRequiredEventFeatures(ctx, tx, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.NotContains(t, features, agentsviewrelay.EventFeature)
	require.NoError(t, tx.Rollback())
	features, err = db.ProjectRequiredEventFeatures(ctx, query, dense.UID, backend.Name == "postgres")
	require.NoError(t, err)
	require.Contains(t, features, agentsviewrelay.EventFeature)
	return nil
}
