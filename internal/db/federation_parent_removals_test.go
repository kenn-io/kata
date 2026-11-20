package db

import (
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectParentRemovalsRejectsStaleLegacyUnlink(t *testing.T) {
	pool := sql.OpenDB(referenceConnector{&referenceWork{}})
	t.Cleanup(func() { _ = pool.Close() })
	for _, query := range []string{
		`CREATE TABLE projects(id INTEGER PRIMARY KEY, uid TEXT)`,
		`CREATE TABLE events(uid TEXT, origin_instance_uid TEXT, project_id INTEGER, issue_uid TEXT, related_issue_uid TEXT, type TEXT, actor TEXT, payload TEXT, hlc_physical_ms INTEGER, hlc_counter INTEGER)`,
		`CREATE TABLE links(from_issue_uid TEXT, to_issue_uid TEXT, type TEXT)`,
		`INSERT INTO projects VALUES (1, 'project')`,
		`INSERT INTO events VALUES ('old','origin',1,'child','parent','issue.linked','actor','{"type":"parent","from_uid":"child","to_uid":"parent"}',1,0)`,
	} {
		_, err := pool.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	child := "child"
	_, err = DirectParentRemovals(t.Context(), tx, []RemoteEvent{{EventUID: "unlink", IssueUID: &child, Type: "issue.unlinked", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"type":"parent","from_uid":"child","to_uid":"parent"}`)}})
	require.ErrorIs(t, err, ErrFederationIngestValidation)
}
