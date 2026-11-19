package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestVectorCatalogMigrationPreservesVectors(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "vector_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	var issues []db.Issue
	for _, title := range []string{"first issue", "second issue", "skipped issue"} {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "operator"})
		require.NoError(t, err)
		issues = append(issues, issue)
	}
	key := (kitvec.Generation{Model: "model", Dimensions: 2, Params: map[string]string{"salt": "example"}}).Fingerprint()
	buildingKey := (kitvec.Generation{Model: "other", Dimensions: 2}).Fingerprint()
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `
		SET search_path TO vector_upgrade, public;
		DROP TABLE issue_vector_chunks, issue_vector_stamps, issue_vector_generations, issue_vector_mirror;
		CREATE TABLE issue_vector_mirror (issue_uid TEXT PRIMARY KEY, project_uid TEXT NOT NULL, content TEXT NOT NULL, content_revision BIGINT NOT NULL, embed_gen TEXT);
		CREATE TABLE issue_vector_generations (ordinal BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, gen_key TEXT NOT NULL UNIQUE, model TEXT NOT NULL, dimensions INTEGER NOT NULL CHECK(dimensions > 0), state TEXT NOT NULL CHECK(state IN ('building','active','retired')));
		CREATE TABLE issue_vector_stamps (gen_key TEXT NOT NULL REFERENCES issue_vector_generations(gen_key) ON DELETE CASCADE, issue_uid TEXT NOT NULL REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE, revision BIGINT NOT NULL, PRIMARY KEY(gen_key, issue_uid));
		CREATE TABLE issue_vector_chunks (gen_key TEXT NOT NULL REFERENCES issue_vector_generations(gen_key) ON DELETE CASCADE, issue_uid TEXT NOT NULL REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE, chunk_index INTEGER NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(gen_key, issue_uid, chunk_index));
		CREATE INDEX idx_issue_vector_chunks_generation ON issue_vector_chunks(gen_key);
		UPDATE meta SET value='51' WHERE key='schema_version'`)
	require.NoError(t, err)
	for _, issue := range issues {
		_, err := admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_mirror(issue_uid, project_uid, content, content_revision, embed_gen) VALUES ($1, $2, $3, $4, $5)`, issue.UID, project.UID, embedding.EmbedText(issue.Title, issue.Body), issue.ContentRevision, key)
		require.NoError(t, err)
	}
	retired := kitvec.Generation{Model: "retired-model", Dimensions: 2, Params: map[string]string{"salt": "retired-example"}}
	retiredKey := retired.Fingerprint()
	_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_generations(gen_key, model, dimensions, state) VALUES ($1, 'model', 2, 'active'), ($2, 'other', 2, 'building')`, key, buildingKey)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_generations(gen_key, model, dimensions, state) VALUES ($1, 'retired-model', 2, 'retired')`, retiredKey)
	require.NoError(t, err)
	for i, issue := range issues {
		_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_stamps(gen_key, issue_uid, revision) VALUES ($1, $2, $3)`, key, issue.UID, issue.ContentRevision)
		require.NoError(t, err)
		if i < 2 {
			value := "[1,0]"
			if i == 1 {
				value = "[0,1]"
			}
			_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_chunks(gen_key, issue_uid, chunk_index, embedding) VALUES ($1, $2, 0, $3::public.halfvec)`, key, issue.UID, value)
			require.NoError(t, err)
		}
	}
	_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_stamps(gen_key, issue_uid, revision) VALUES ($1, $2, $3)`, buildingKey, issues[0].UID, issues[0].ContentRevision-1)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "vector_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	idx, err := vector.OpenPostgres(ctx, upgraded.DB)
	require.NoError(t, err)
	release, err := idx.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		if release != nil {
			_ = release()
		}
	})
	require.NoError(t, idx.EnsureBuilding(ctx, retiredKey, retired))
	require.NoError(t, release())
	release = nil
	var retiredState string
	err = upgraded.QueryRowContext(ctx, `SELECT state FROM issue_vector_generations WHERE gen_key=$1`, retiredKey).Scan(&retiredState)
	require.NoError(t, err)
	assert.Equal(t, "building", retiredState)
	hits, err := idx.Query(ctx, key, kitvec.Vector{1, 0}, 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, issues[0].UID, hits[0].Doc)
	assert.InDelta(t, 1, hits[0].Score, 0.001)
	assert.Equal(t, issues[1].UID, hits[1].Doc)
	assert.InDelta(t, 0, hits[1].Score, 0.001)
	backlog, err := idx.Backlog(ctx, key)
	require.NoError(t, err)
	assert.Zero(t, backlog)
	backlog, err = idx.Backlog(ctx, buildingKey)
	require.NoError(t, err)
	assert.EqualValues(t, 3, backlog)
}
