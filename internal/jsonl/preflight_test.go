package jsonl_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/jsonl"
)

// TestPreflightSourceFKs_DeduplicatesPerRow verifies the rowid-set
// semantics: a links row with both endpoints missing shows up
// twice in foreign_key_check but counts as one drop, and an events
// row with both issue_id and related_issue_id missing counts as
// one drop with no scrub (drop precedence).
func TestPreflightSourceFKs_DeduplicatesPerRow(t *testing.T) {
	ctx := context.Background()

	t.Run("links both endpoints orphan + events both columns orphan", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kata.db")
		seedV3DBWithOrphans(t, path, orphanSpec{
			OrphanLinkBothEnds: 1,
			OrphanEventBoth:    1,
		})

		report, err := jsonl.PreflightSourceFKs(ctx, path)
		require.NoError(t, err)
		assert.Equal(t, 1, report.DropCount("links"),
			"links row with two violated FKs should count once")
		assert.Equal(t, 1, report.DropCount("events"),
			"events row with both columns orphaned should count as one drop")
		assert.Equal(t, 0, report.ScrubCount("events"),
			"drop precedence: same events rowid must NOT also appear in scrub bucket")
		assert.Empty(t, report.UnknownViolations)
	})

	t.Run("events related-only orphan goes to scrub bucket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kata.db")
		seedV3DBWithOrphans(t, path, orphanSpec{
			OrphanEventRelated: 1,
		})

		report, err := jsonl.PreflightSourceFKs(ctx, path)
		require.NoError(t, err)
		assert.Equal(t, 0, report.DropCount("events"),
			"events with valid issue_id but orphan related must NOT be dropped")
		assert.Equal(t, 1, report.ScrubCount("events"),
			"events with orphan related_issue_id should be scrubbed (preserved with NULL related)")
		assert.Empty(t, report.UnknownViolations)
	})

	t.Run("unknown class returns UnknownViolations", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kata.db")
		seedV3DBWithOrphans(t, path, orphanSpec{
			OrphanProjectAlias: true,
		})

		report, err := jsonl.PreflightSourceFKs(ctx, path)
		require.NoError(t, err)
		require.Len(t, report.UnknownViolations, 1)
		assert.Equal(t, "project_aliases", report.UnknownViolations[0].Table)
		assert.Equal(t, "projects", report.UnknownViolations[0].ParentTable)
		assert.Equal(t, "project_id", report.UnknownViolations[0].Column)
	})

	t.Run("clean DB returns empty report", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kata.db")
		seedV3DBWithOrphans(t, path, orphanSpec{})

		report, err := jsonl.PreflightSourceFKs(ctx, path)
		require.NoError(t, err)
		assert.Empty(t, report.Rows, "a clean DB files no row under any disposition")
		assert.Empty(t, report.UnknownViolations)
	})

	// PRAGMA foreign_key_check returns NULL for the rowid column on
	// WITHOUT ROWID tables. Scanning that into a plain int64 fails
	// with a type-conversion error, which would mask the actual
	// violation. Use sql.NullInt64 so the row scans cleanly and the
	// violation surfaces in UnknownViolations with RowID.Valid=false.
	t.Run("WITHOUT ROWID source table yields NULL rowid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kata.db")
		seedV3DBWithOrphans(t, path, orphanSpec{})
		addWithoutRowidOrphan(t, path)

		report, err := jsonl.PreflightSourceFKs(ctx, path)
		require.NoError(t, err)
		require.Len(t, report.UnknownViolations, 1)
		v := report.UnknownViolations[0]
		assert.Equal(t, "wr_child", v.Table)
		assert.Equal(t, "projects", v.ParentTable)
		assert.Equal(t, "project_id", v.Column)
		assert.False(t, v.RowID.Valid, "WITHOUT ROWID tables yield NULL rowid from foreign_key_check")
	})
}
