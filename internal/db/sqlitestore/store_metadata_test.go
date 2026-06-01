package sqlitestore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestPatchIssueMetadata_HappyPath(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)

	res, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: iss.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-05-20"`),
		},
	})
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, iss.Revision+1, res.NewRevision)
	assert.Contains(t, string(res.Issue.Metadata), `"scheduled_on":"2026-05-20"`)
	assert.NotZero(t, res.Event.ID)
	assert.Equal(t, "issue.metadata_updated", res.Event.Type)
}

func TestPatchIssueMetadata_StaleRevisionReturns409(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)

	_, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: 99, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-05-20"`),
		},
	})
	require.Error(t, err)
	var conflict *db.RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, iss.Revision, conflict.CurrentRevision)
}

func TestPatchIssueMetadata_EmptyDiffNoEvent(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)

	// First patch sets the key (revision bumps).
	res1, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: iss.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-05-20"`),
		},
	})
	require.NoError(t, err)
	require.True(t, res1.Changed)

	// Re-applying the same value is a no-op.
	res2, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: res1.NewRevision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-05-20"`),
		},
	})
	require.NoError(t, err)
	assert.False(t, res2.Changed)
	assert.Zero(t, res2.Event.ID, "no-op patch must not emit an event")
	assert.Equal(t, res1.NewRevision, res2.NewRevision, "revision unchanged on no-op")

	// Verify no second issue.metadata_updated event in the events table.
	var n int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM events WHERE type='issue.metadata_updated' AND issue_id = ?
	`, iss.ID).Scan(&n))
	assert.Equal(t, 1, n, "no-op patch must not append another event row")
}

func TestPatchIssueMetadata_InvalidKeyValueRejected(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)

	_, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: iss.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`123`), // reserved key, wrong JSON type
		},
	})
	require.Error(t, err)
}

// TestPatchIssueMetadata_UnknownKeyAccepted: keys outside the reserved set
// are accepted opaquely and persist into the metadata blob.
func TestPatchIssueMetadata_UnknownKeyAccepted(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)

	res, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: iss.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"definitely_not_a_key": json.RawMessage(`"yellow"`),
		},
	})
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Contains(t, string(res.Issue.Metadata), `"definitely_not_a_key":"yellow"`)
}

func TestPatchProjectMetadata_HappyPath(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)

	res, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: p.ID, IfMatchRev: p.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"area": json.RawMessage(`"Personal"`),
		},
	})
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, p.Revision+1, res.NewRevision)
	assert.Contains(t, string(res.Project.Metadata), `"area":"Personal"`)
	assert.NotZero(t, res.Event.ID)
	assert.Equal(t, "project.metadata_updated", res.Event.Type)
}

func TestPatchProjectMetadata_StaleRevisionReturns409(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, _ := d.CreateProject(ctx, "p")
	_, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: p.ID, IfMatchRev: 99, Actor: "tester",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"X"`)},
	})
	var conflict *db.RevisionConflictError
	require.ErrorAs(t, err, &conflict)
}

// TestPatchProjectMetadata_UnknownKeyAccepted: project metadata accepts
// unknown keys opaquely, matching the issue side.
func TestPatchProjectMetadata_UnknownKeyAccepted(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, _ := d.CreateProject(ctx, "p")
	res, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: p.ID, IfMatchRev: p.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{"definitely_not_a_key": json.RawMessage(`"yellow"`)},
	})
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Contains(t, string(res.Project.Metadata), `"definitely_not_a_key":"yellow"`)
}

func TestPatchProjectMetadata_EmptyDiffNoEvent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, _ := d.CreateProject(ctx, "p")

	res1, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: p.ID, IfMatchRev: p.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"X"`)},
	})
	require.NoError(t, err)

	res2, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: p.ID, IfMatchRev: res1.NewRevision, Actor: "tester",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"X"`)},
	})
	require.NoError(t, err)
	assert.False(t, res2.Changed)
	assert.Zero(t, res2.Event.ID)
	assert.Equal(t, res1.NewRevision, res2.NewRevision)
}

func TestPatchIssueMetadata_ClearKeyWithNull(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)

	// Set a key first.
	res1, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: iss.Revision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-05-20"`),
		},
	})
	require.NoError(t, err)

	// Clear it with null.
	res2, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: iss.ID, IfMatchRev: res1.NewRevision, Actor: "tester",
		Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`null`),
		},
	})
	require.NoError(t, err)
	assert.True(t, res2.Changed)
	assert.NotContains(t, string(res2.Issue.Metadata), "scheduled_on")
}
