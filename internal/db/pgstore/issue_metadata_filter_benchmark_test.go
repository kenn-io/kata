package pgstore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
)

const (
	metadataBenchProjects         = 50
	metadataBenchIssuesPerProject = 2000
	// One issue in metadataBenchKeyEvery carries the filtered key; one in
	// metadataBenchOtherEvery carries an unrelated key; the rest have none.
	metadataBenchKeyEvery   = 500
	metadataBenchOtherEvery = 20
)

// BenchmarkListAllIssuesMetaKey times the query behind
// `kata list --all --status all --meta work.attention` on 100,000 issues in
// 50 projects, without and with idx_issues_metadata, and reports the
// index's build time and size.
func BenchmarkListAllIssuesMetaKey(b *testing.B) {
	ctx := b.Context()
	dsn, cleanup := conformanceTemplate.NewDatabase(b, ctx)
	b.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, conformanceConfig())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	admin, err := sql.Open("pgx", dsn)
	require.NoError(b, err)
	b.Cleanup(func() { _ = admin.Close() })
	// Issue triggers call schema-local functions by bare name.
	seed, err := admin.Conn(ctx)
	require.NoError(b, err)
	_, err = seed.ExecContext(ctx, `SET search_path TO conformance`)
	require.NoError(b, err)

	for project := range metadataBenchProjects {
		created, err := store.CreateProject(ctx, fmt.Sprintf("metadata-bench-%02d", project))
		require.NoError(b, err)
		_, err = seed.ExecContext(ctx, `
			INSERT INTO conformance.issues (uid, project_id, short_id, title, author, created_at, updated_at, metadata)
			SELECT lpad(n::text, 26, '0'), $1, right(lpad(n::text, 26, '0'), 8), 'Issue ' || n, 'bench', ts, ts,
			       CASE WHEN n % $3 = 0 THEN '{"work.attention":"stuck"}'
			            WHEN n % $4 = 0 THEN '{"work.branch":"b' || n || '"}'
			            ELSE '{}' END
			FROM (
			  SELECT $2::bigint * 100000 + i AS n,
			         to_char(timestamp '2026-01-01' + (($2::bigint * 100000 + i) * interval '1 second'),
			                 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS ts
			  FROM generate_series(1, $5::bigint) AS i
			) seed`,
			created.ID, project, metadataBenchKeyEvery, metadataBenchOtherEvery, metadataBenchIssuesPerProject)
		require.NoError(b, err)
	}
	require.NoError(b, seed.Close())
	_, err = admin.ExecContext(ctx, `ANALYZE conformance.issues`)
	require.NoError(b, err)
	var tableBytes int64
	require.NoError(b, admin.QueryRowContext(ctx, `SELECT pg_relation_size('conformance.issues')`).Scan(&tableBytes))
	b.Logf("issues table: %d bytes", tableBytes)

	// The parameters the daemon builds for the CLI command above
	// (internal/daemon/handlers_issues.go): no status, no limit, newest first,
	// every project, one key-only metadata filter.
	params := db.ListAllIssuesParams{
		CreatedFirst: true,
		Meta:         []db.MetaFilter{{Key: "work.attention"}},
	}
	want := metadataBenchProjects * metadataBenchIssuesPerProject / metadataBenchKeyEvery

	run := func(b *testing.B) {
		explainListAll(b, admin, params)
		b.ResetTimer()
		for range b.N {
			issues, err := store.ListAllIssues(ctx, params)
			if err != nil {
				b.Fatal(err)
			}
			if len(issues) != want {
				b.Fatalf("got %d issues, want %d", len(issues), want)
			}
		}
	}

	// Fresh schemas include the index; time the filter without it first.
	_, err = admin.ExecContext(ctx, `DROP INDEX conformance.idx_issues_metadata`)
	require.NoError(b, err)
	_, err = admin.ExecContext(ctx, `ANALYZE conformance.issues`)
	require.NoError(b, err)
	b.Run("no-index", run)

	start := time.Now()
	_, err = admin.ExecContext(ctx, `CREATE INDEX idx_issues_metadata ON conformance.issues USING GIN ((metadata::jsonb))`)
	require.NoError(b, err)
	build := time.Since(start)
	_, err = admin.ExecContext(ctx, `ANALYZE conformance.issues`)
	require.NoError(b, err)
	var indexBytes int64
	require.NoError(b, admin.QueryRowContext(ctx, `SELECT pg_relation_size('conformance.idx_issues_metadata')`).Scan(&indexBytes))
	b.Logf("idx_issues_metadata: built in %s, %d bytes", build, indexBytes)

	b.Run("gin-index", func(b *testing.B) {
		run(b)
		b.ReportMetric(float64(build.Milliseconds()), "index-build-ms")
		b.ReportMetric(float64(indexBytes), "index-bytes")
	})
}

// explainListAll logs the executed plan of the exact SQL ListAllIssues runs.
func explainListAll(b *testing.B, admin *sql.DB, params db.ListAllIssuesParams) {
	b.Helper()
	query, args := pgstore.BuildListAllIssuesQueryForTest(b.Context(), params)
	conn, err := admin.Conn(b.Context())
	require.NoError(b, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(b.Context(), `SET search_path TO conformance`)
	require.NoError(b, err)
	rows, err := conn.QueryContext(b.Context(), `EXPLAIN (ANALYZE, BUFFERS) `+query, args...)
	require.NoError(b, err)
	var lines []string
	for rows.Next() {
		var line string
		require.NoError(b, rows.Scan(&line))
		lines = append(lines, line)
	}
	require.NoError(b, rows.Err())
	require.NoError(b, rows.Close())
	b.Logf("plan:\n%s", strings.Join(lines, "\n"))
}
