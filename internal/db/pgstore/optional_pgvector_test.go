package pgstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestPostgresBootstrapDoesNotRequirePgvector(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPlainPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	created, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "plain postgres remains usable",
		Author:    "operator",
	})
	require.NoError(t, err)

	issues, err := store.SearchFTS(ctx, project.ID, "plain postgres", 10, false)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, created.UID, issues[0].Issue.UID)
}
