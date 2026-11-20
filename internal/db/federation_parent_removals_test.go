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
	_, err := pool.ExecContext(t.Context(), `CREATE TABLE links(from_issue_uid TEXT, to_issue_uid TEXT, type TEXT)`)
	require.NoError(t, err)
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	child := "child"
	_, err = DirectParentRemovals(t.Context(), tx, []RemoteEvent{{EventUID: "unlink", IssueUID: &child, Type: "issue.unlinked", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"type":"parent","from_uid":"child","to_uid":"parent"}`)}})
	require.ErrorIs(t, err, ErrFederationIngestValidation)
}
