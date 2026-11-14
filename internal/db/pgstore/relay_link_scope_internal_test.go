package pgstore

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestRelayLinkEndpointProjectsScopesReferences(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "scope-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Source", Author: "tester"})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Referenced", Author: "tester"})
	require.NoError(t, err)
	unrelated, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Unrelated", Author: "tester"})
	require.NoError(t, err)
	refs := make([]string, 0, 502)
	for i := range 501 {
		refs = append(refs, fmt.Sprintf("missing-%03d", i))
	}
	refs = append(refs, peer.UID)
	payload, err := json.Marshal(map[string]any{"related_added_uids": refs})
	require.NoError(t, err)
	events := []db.FoldEvent{{UID: "event", IssueUID: source.UID, ProjectUID: project.UID, Type: "issue.links_changed", Payload: payload}}
	tx, err := store.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	owners, err := relayLinkEndpointProjects(ctx, tx, events)
	require.NoError(t, err)
	require.Equal(t, project.UID, owners[source.UID])
	require.Equal(t, project.UID, owners[peer.UID])
	require.Contains(t, owners, "missing-500")
	require.Empty(t, owners["missing-500"])
	require.NotContains(t, owners, unrelated.UID)
}
