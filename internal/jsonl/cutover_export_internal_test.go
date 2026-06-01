// Internal tests for exportForCutover (the SQLite-bound legacy exporter that
// reads pre-v10 on-disk databases). The public Export is backend-neutral and
// only understands the current schema, so pre-v10 source paths exercise
// exportForCutover via this internal test file (they cannot reach the
// unexported function from `package jsonl_test`).
package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// writeLegacyV1DBInternal creates a SQLite database at path by executing the
// v1 schema fixture. It is the internal-package twin of writeLegacyV1DB in
// fixtures_test.go (which lives in package jsonl_test and is unreachable from
// here).
func writeLegacyV1DBInternal(t *testing.T, path string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "legacy_v1.sql")) //nolint:gosec // testdata path is constant
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec(string(schema))
	require.NoError(t, err)
}

// openLegacyV1ForExport opens the v1 fixture DB read-only, used only by the
// legacy-export internal tests in this file.
func openLegacyV1ForExport(ctx context.Context, t *testing.T) *sqlitestore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyV1DBInternal(t, path)
	d, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// exportForCutoverDecode runs exportForCutover and decodes the resulting
// JSONL stream into records, matching the shape of exportAndDecode in
// export_test.go.
func exportForCutoverDecode(ctx context.Context, t *testing.T, d *sqlitestore.Store, opts ExportOptions) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, exportForCutover(ctx, d, &out, opts))
	scanner := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	var records []map[string]any
	for scanner.Scan() {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
		records = append(records, rec)
	}
	require.NoError(t, scanner.Err())
	return records
}

// TestExportForCutoverV1OmitsUIDFields pins the legacy-projection contract:
// a v1 source DB has no UID columns, so the exported records for project,
// issue, link, event, and purge_log must omit those keys. This test moved
// from TestExportLegacyV1OmitsUIDFields in export_test.go when the public
// Export became backend-neutral and lost its legacy-version dispatch.
func TestExportForCutoverV1OmitsUIDFields(t *testing.T) {
	ctx := context.Background()
	d := openLegacyV1ForExport(ctx, t)

	records := exportForCutoverDecode(ctx, t, d, ExportOptions{IncludeDeleted: true})

	require.NotEmpty(t, records)
	assert.Equal(t, map[string]any{"key": "export_version", "value": "1"}, records[0]["data"])
	for _, rec := range records {
		data, _ := rec["data"].(map[string]any)
		switch rec["kind"] {
		case "project", "issue":
			assert.NotContains(t, data, "uid")
		case "link":
			assert.NotContains(t, data, "from_issue_uid")
			assert.NotContains(t, data, "to_issue_uid")
		case "event":
			assert.NotContains(t, data, "issue_uid")
			assert.NotContains(t, data, "related_issue_uid")
		case "purge_log":
			assert.NotContains(t, data, "issue_uid")
			assert.NotContains(t, data, "project_uid")
		}
	}
}
