package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// TestAllSchemaTablesExist guards against a future migration accidentally
// dropping a table that Plan 1 doesn't actively exercise. Plan 1 reads/writes
// projects, project_aliases, issues, comments, events, and meta. The other
// names below (links, issue_labels, purge_log, issues_fts) are scaffolded by
// 0001_init.sql for later plans; this test is the only thing that catches a
// silent removal.
func TestAllSchemaTablesExist(t *testing.T) {
	d := openTestDB(t)
	wanted := []string{
		"projects", "project_aliases", "issues", "comments",
		"links", "issue_labels", "events", "purge_log",
		"api_tokens", "federation_bindings", "federation_sync_status",
		"federation_quarantine", "federation_enrollments",
		"issue_claims", "pending_claim_requests",
		"meta", "issues_fts", "import_mappings", "recurrences",
	}
	for _, name := range wanted {
		assertSchemaObject(t, d, name)
	}

	rows, err := d.QueryContext(context.Background(), `
		SELECT name
		  FROM sqlite_master
		 WHERE type = 'table'
		   AND name NOT LIKE 'sqlite_%'
		   AND name NOT GLOB 'issues_fts_*'
		 ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	expected := make(map[string]bool, len(wanted))
	for _, name := range wanted {
		expected[name] = true
	}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		assert.Truef(t, expected[name], "schema table %q is missing from TestAllSchemaTablesExist wanted list", name)
	}
	require.NoError(t, rows.Err())
}

func TestSchemaUIDColumnsIndexesAndTriggers(t *testing.T) {
	d := openTestDB(t)
	assertColumn(t, d, "projects", "uid", "TEXT", true)
	assertColumn(t, d, "projects", "metadata", "TEXT", true)
	assertColumn(t, d, "projects", "revision", "INTEGER", true)
	assertColumn(t, d, "issues", "uid", "TEXT", true)
	assertColumn(t, d, "issues", "metadata", "TEXT", true)
	assertColumn(t, d, "issues", "revision", "INTEGER", true)
	assertColumn(t, d, "issues", "recurrence_id", "INTEGER", false) // nullable
	assertColumn(t, d, "issues", "occurrence_key", "TEXT", false)   // nullable
	assertColumn(t, d, "links", "from_issue_uid", "TEXT", true)
	assertColumn(t, d, "links", "to_issue_uid", "TEXT", true)
	assertColumn(t, d, "events", "uid", "TEXT", true)
	assertColumn(t, d, "events", "origin_instance_uid", "TEXT", true)
	assertColumn(t, d, "events", "issue_uid", "TEXT", false)
	assertColumn(t, d, "events", "related_issue_uid", "TEXT", false)
	assertColumn(t, d, "purge_log", "uid", "TEXT", true)
	assertColumn(t, d, "purge_log", "origin_instance_uid", "TEXT", true)
	assertColumn(t, d, "purge_log", "issue_uid", "TEXT", false)
	assertColumn(t, d, "purge_log", "project_uid", "TEXT", false)

	for _, name := range []string{
		"idx_links_from_uid",
		"idx_links_to_uid",
		"idx_events_issue_uid",
		"idx_events_related_issue_uid",
		"idx_events_origin_instance",
		"idx_purge_log_issue_uid",
		"idx_purge_log_project_uid",
		"idx_purge_log_origin_instance",
		"idx_import_mappings_issue",
		"idx_import_mappings_comment",
		"idx_import_mappings_link",
		"trg_links_uid_consistency_insert",
		"trg_links_uid_consistency_update",
		"trg_projects_uid_immutable",
		"trg_issues_uid_immutable",
	} {
		assertSchemaObject(t, d, name)
	}
}

func assertColumn(t *testing.T, d *sqlitestore.Store, table, column, typ string, notNull bool) {
	t.Helper()
	rows, err := d.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid        int
			name       string
			gotType    string
			gotNotNull int
			defaultVal any
			pk         int
		)
		require.NoError(t, rows.Scan(&cid, &name, &gotType, &gotNotNull, &defaultVal, &pk))
		if name == column {
			assert.Equal(t, typ, gotType, table+"."+column)
			assert.Equal(t, notNull, gotNotNull == 1, table+"."+column)
			return
		}
	}
	require.NoError(t, rows.Err())
	t.Fatalf("column %s.%s missing", table, column)
}

func assertSchemaObject(t *testing.T, d *sqlitestore.Store, name string) {
	t.Helper()
	var got string
	err := d.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE name = ?`, name).Scan(&got)
	require.NoErrorf(t, err, "schema object %q missing", name)
	assert.Equal(t, name, got)
}
