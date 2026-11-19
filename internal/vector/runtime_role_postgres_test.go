package vector_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestPostgresRuntimeRoleSwitchesModelsWithoutCreate(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	owner, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "vector_runtime", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })
	_, err = owner.DB.ExecContext(ctx, `INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision) VALUES ('issue-1', 'project-1', 'example', 1)`)
	require.NoError(t, err)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `CREATE ROLE vector_runtime_user LOGIN PASSWORD 'runtime-password'; GRANT USAGE ON SCHEMA vector_runtime TO vector_runtime_user; GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA vector_runtime TO vector_runtime_user; GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA vector_runtime TO vector_runtime_user`)
	require.NoError(t, err)
	config, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	config.User, config.Password = "vector_runtime_user", "runtime-password"
	config.RuntimeParams["search_path"] = "vector_runtime, public"
	runtimeDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = runtimeDB.Close() })
	ix, err := vector.OpenPostgres(ctx, runtimeDB)
	require.NoError(t, err)
	release, err := ix.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, release()) })
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	for _, model := range []string{"a", "b", "a"} {
		gen := kitvec.Generation{Model: model, Dimensions: 2}
		key := gen.Fingerprint()
		require.NoError(t, ix.EnsureBuilding(ctx, key, gen))
		_, err := ix.Fill(ctx, key, encode, 0, nil, nil)
		require.NoError(t, err)
		require.NoError(t, ix.CutOver(ctx, key))
		active, ok, err := ix.ActiveGeneration(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, key, active)
		hits, err := ix.Query(ctx, key, kitvec.Vector{1, 0}, 1)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		require.Equal(t, "issue-1", hits[0].Doc)
	}
}
