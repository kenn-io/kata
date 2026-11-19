package pgstore_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestVectorCatalogMigrationPreservesVectors(t *testing.T) {
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
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected embedding", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	embedder, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "model", Dims: 2, Salt: "example"})
	require.NoError(t, err)
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
		_, err := admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_mirror(issue_uid, project_uid, content, content_revision) VALUES ($1, $2, $3, $4)`, issue.UID, project.UID, embedding.EmbedText(issue.Title, issue.Body), issue.ContentRevision)
		require.NoError(t, err)
	}
	key := embedder.Generation().Fingerprint()
	_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_generations(gen_key, model, dimensions, state) VALUES ($1, 'model', 2, 'active'), ('building', 'other', 2, 'building')`, key)
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
	_, err = admin.ExecContext(ctx, `INSERT INTO vector_upgrade.issue_vector_stamps(gen_key, issue_uid, revision) VALUES ('building', $1, $2)`, issues[0].UID, issues[0].ContentRevision-1)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "vector_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	var fingerprint, doc, embedding string
	var revision int
	err = upgraded.QueryRowContext(ctx, `SELECT g.fingerprint, s.doc_key, s.revision, c.embedding::text FROM issue_vector_generations g JOIN issue_vector_stamps s ON s.generation_id=g.id JOIN issue_vector_chunks c ON c.generation_id=g.id AND c.doc_key=s.doc_key WHERE s.doc_key=$1`, issues[0].UID).Scan(&fingerprint, &doc, &revision, &embedding)
	require.NoError(t, err)
	assert.Equal(t, key, fingerprint)
	assert.Equal(t, issues[0].UID, doc)
	assert.EqualValues(t, issues[0].ContentRevision, revision)
	assert.Equal(t, "[1,0]", embedding)
	idx, err := vector.OpenPostgres(ctx, upgraded.DB)
	require.NoError(t, err)
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
	backlog, err = idx.Backlog(ctx, "building")
	require.NoError(t, err)
	assert.EqualValues(t, 3, backlog)
	reconcileCtx, cancel := context.WithCancel(ctx)
	reconciler := daemon.NewReconciler(upgraded, idx, embedder, daemon.ReconcilerConfig{BatchSize: 64, SweepEvery: time.Hour})
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(reconcileCtx) }()
	require.Eventually(t, func() bool { return reconciler.Health().LastSuccessAt != nil }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not stop")
	}
	assert.Zero(t, calls.Load())
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "vector_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	assert.Equal(t, vectorCatalogShape(ctx, t, admin, "vector_fresh"), vectorCatalogShape(ctx, t, admin, "vector_upgrade"))
}

func vectorCatalogShape(ctx context.Context, t *testing.T, admin *sql.DB, schema string) []string {
	t.Helper()
	rows, err := admin.QueryContext(ctx, `SELECT value FROM (
	 SELECT 'column:' || table_name || ':' || column_name || ':' || udt_name || ':' || is_nullable AS value FROM information_schema.columns WHERE table_schema=$1
	 UNION ALL SELECT 'constraint:' || c.relname || ':' || con.conname || ':' || replace(pg_get_constraintdef(con.oid,true),$1||'.','') FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
	 UNION ALL SELECT 'index:' || tablename || ':' || indexname || ':' || replace(indexdef,$1||'.','') FROM pg_indexes WHERE schemaname=$1
	 ) shapes WHERE value ~ '^(column|constraint|index):issue_vector_' ORDER BY value`, schema)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}
