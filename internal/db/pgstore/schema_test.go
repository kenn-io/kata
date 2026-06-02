package pgstore_test

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// expectedTables enumerates the structural surface schema.sql must produce.
// Full constraint/index name parity with sqlitestore belongs in the later
// conformance suite; this test pins the baseline acceptance subset.
var expectedTables = []string{
	"api_tokens",
	"comments",
	"events",
	"federation_bindings",
	"federation_enrollments",
	"federation_quarantine",
	"federation_sync_status",
	"import_mappings",
	"issue_claims",
	"issue_labels",
	"issues",
	"issues_search",
	"links",
	"meta",
	"pending_claim_requests",
	"project_aliases",
	"projects",
	"purge_log",
	"recurrences",
}

// expectedTriggers lists the named triggers that must exist after bootstrap.
// Counts the SQLite RAISE(ABORT, ...) ports + the FTS sync triggers, omitting
// the FK CASCADE that replaces SQLite's issue-delete FTS trigger.
var expectedTriggers = []string{
	// Same-project link enforcement.
	"trg_links_same_project_insert",
	"trg_links_same_project_update",
	// UID consistency on links.
	"trg_links_uid_consistency_insert",
	"trg_links_uid_consistency_update",
	// UID immutability.
	"trg_projects_uid_immutable",
	"trg_issues_uid_immutable",
	// FTS sync.
	"issues_search_after_issue_insert",
	"issues_search_after_issue_update",
	"issues_search_after_comment_insert",
	"issues_search_after_comment_delete",
}

// expectedFKCounts pins the per-table foreign-key counts. The conformance
// suite should compare names too; this subset checks arity so a missing FK is
// caught without forcing name parity.
var expectedFKCounts = map[string]int{
	"project_aliases":        1, // -> projects
	"recurrences":            1, // -> projects (CASCADE)
	"issues":                 2, // -> projects, -> recurrences
	"comments":               1, // -> issues
	"links":                  3, // -> projects, -> issues x2
	"issue_labels":           1, // -> issues
	"events":                 3, // -> projects, -> issues, -> issues (related)
	"federation_bindings":    1, // -> projects
	"federation_sync_status": 1, // -> projects
	"federation_quarantine":  1, // -> projects
	"federation_enrollments": 1, // -> projects
	"issue_claims":           2, // -> projects, -> issues
	"pending_claim_requests": 2, // -> projects, -> issues
	"issues_search":          1, // -> issues (CASCADE)
	"import_mappings":        4, // -> projects, issues, comments, links
}

// TestSchema_BaselineMatchesExpectedSurface opens a real PG and asserts the
// structural surface (tables, named triggers, idempotency
// UNIQUE index, FTS GIN index, per-table FK counts) matches the v12 baseline.
// The conformance suite should lift the bar to byte-level constraint-name parity.
func TestSchema_BaselineMatchesExpectedSurface(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// --- tables ---
	got := queryStrings(t, s, `
		SELECT table_name
		  FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_type = 'BASE TABLE'
		 ORDER BY table_name`)
	sort.Strings(got)
	for _, want := range expectedTables {
		assert.Contains(t, got, want, "missing table %q", want)
	}

	// --- triggers ---
	gotTriggers := queryStrings(t, s, `
		SELECT trigger_name
		  FROM information_schema.triggers
		 WHERE trigger_schema = current_schema()`)
	// information_schema.triggers double-counts triggers that fire for
	// INSERT OR UPDATE OR DELETE (one row per event); de-dup for the
	// presence check.
	seen := map[string]struct{}{}
	for _, n := range gotTriggers {
		seen[n] = struct{}{}
	}
	for _, want := range expectedTriggers {
		_, ok := seen[want]
		assert.True(t, ok, "missing trigger %q", want)
	}

	// --- idempotency partial index ---
	var idempotencyIndex string
	err = s.QueryRowContext(ctx, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND indexname = 'idx_events_idempotency'`).Scan(&idempotencyIndex)
	require.NoError(t, err)
	assert.Equal(t, "idx_events_idempotency", idempotencyIndex)

	// --- FTS GIN index over issues_search.tsv ---
	var ftsGinIndex string
	err = s.QueryRowContext(ctx, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND tablename = 'issues_search'
		   AND indexname = 'idx_issues_search_tsv'`).Scan(&ftsGinIndex)
	require.NoError(t, err)
	assert.Equal(t, "idx_issues_search_tsv", ftsGinIndex)

	// --- text search config ---
	var textSearchConfig string
	err = s.QueryRowContext(ctx, `
		SELECT cfgname FROM pg_ts_config
		 WHERE cfgname = 'kata_simple_unaccent'`).Scan(&textSearchConfig)
	require.NoError(t, err)
	assert.Equal(t, "kata_simple_unaccent", textSearchConfig)

	// --- per-table FK counts ---
	for table, want := range expectedFKCounts {
		var n int
		err := s.QueryRowContext(ctx, `
			SELECT COUNT(*)
			  FROM information_schema.table_constraints
			 WHERE table_schema = current_schema()
			   AND table_name = $1
			   AND constraint_type = 'FOREIGN KEY'`, table).Scan(&n)
		require.NoError(t, err, "fk count query for %s", table)
		assert.Equal(t, want, n, "fk count mismatch on %s", table)
	}

	// --- 3 PL/pgSQL trigger functions exist ---
	for _, fn := range []string{
		"enforce_links_same_project",
		"enforce_links_uid_consistency",
		"enforce_uid_immutable",
		"rebuild_issue_search",
		"issues_search_trigger_on_issue",
		"issues_search_trigger_on_comment_insert",
		"issues_search_trigger_on_comment_delete",
	} {
		var name string
		err := s.QueryRowContext(ctx, `
			SELECT proname FROM pg_proc p
			  JOIN pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = current_schema()
			   AND p.proname = $1`, fn).Scan(&name)
		require.NoError(t, err, "missing function %s", fn)
		assert.Equal(t, fn, name)
	}
}

// queryStrings runs a single-column SELECT and returns the values. Fails the
// test on error.
func queryStrings(t *testing.T, s *pgstore.Store, q string) []string {
	t.Helper()
	rows, err := s.QueryContext(context.Background(), q)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}
