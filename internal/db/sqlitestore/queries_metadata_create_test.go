package sqlitestore_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// TestCreateIssue_MetadataPersistsAndRoundTrips pins that opaque and
// reserved-valid metadata supplied at creation is marshaled into the
// issues.metadata blob and round-trips verbatim on a subsequent read.
func TestCreateIssue_MetadataPersistsAndRoundTrips(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "with metadata",
		Author:    "tester",
		Metadata: map[string]jsontext.Value{
			"work.branch":  jsontext.Value(`"feature/x"`),
			"scheduled_on": jsontext.Value(`"2026-01-02"`),
		},
	})
	require.NoError(t, err)

	got, err := d.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	var m map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(got.Metadata), &m))
	assert.JSONEq(t, `"feature/x"`, string(m["work.branch"]))
	assert.JSONEq(t, `"2026-01-02"`, string(m["scheduled_on"]))

	// The issue.created event payload carries the same metadata (not the
	// hardcoded empty object).
	var payload struct {
		Metadata map[string]jsontext.Value `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(evt.Payload), &payload))
	assert.JSONEq(t, `"feature/x"`, string(payload.Metadata["work.branch"]))
}

// TestCreateIssue_EmptyMetadataKeepsDefault pins that absent/empty metadata
// keeps the schema's '{}' default rather than writing null or a literal.
func TestCreateIssue_EmptyMetadataKeepsDefault(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "no metadata",
		Author:    "tester",
	})
	require.NoError(t, err)

	got, err := d.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(got.Metadata))
}

// TestCreateIssue_RejectsInvalidReservedValue pins that a reserved key with a
// type-invalid value fails validation before the row is inserted.
func TestCreateIssue_RejectsInvalidReservedValue(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "bad reserved",
		Author:    "tester",
		Metadata: map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"not-a-date"`),
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, metadata.ErrInvalidValue)

	// Nothing was persisted.
	rows, err := d.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestCreateIssue_RejectsNullMetadataValue pins that a JSON null value at
// creation is rejected (nothing to clear) rather than stored.
func TestCreateIssue_RejectsNullMetadataValue(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "null value",
		Author:    "tester",
		Metadata: map[string]jsontext.Value{
			"work.branch": jsontext.Value(`null`),
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, metadata.ErrInvalidValue)
}
