package jsonl

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestExportSnapshotV14FederationEnrollmentPreservesAdoptionMarker(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `
		INSERT INTO federation_enrollments(
			token_hash, spoke_instance_uid, project_id, capabilities, bound_actor,
			allow_adoption_snapshot_authors
		)
		VALUES(?, ?, ?, 'pull,push', 'tester', 1)`,
		strings.Repeat("c", 64), "01HZZZZZZZZZZZZZZZZZZZZZ04", project.ID)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, exportSnapshot(ctx, source, &out, ExportOptions{}))

	assert.Contains(t, out.String(), `"allow_adoption_snapshot_authors":true`)
	assert.Contains(t, out.String(), `"bound_actor":"tester"`)
}
