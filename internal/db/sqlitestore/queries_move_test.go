package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMoveIssueProject_HappyPath(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	srcP, err := d.CreateProject(ctx, "src")
	require.NoError(t, err)
	tgtP, err := d.CreateProject(ctx, "target")
	require.NoError(t, err)
	srcI := seedIssueInProject(t, d, srcP.ID, "Move me", "tester")

	res, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID:       srcI.ID,
		FromProjectID: srcP.ID,
		ToProjectID:   tgtP.ID,
		IfMatchRev:    1,
		Actor:         "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, tgtP.ID, res.Issue.ProjectID)
	assert.NotEmpty(t, res.NewShortID)
	assert.Equal(t, int64(2), res.NewRevision)

	var projectUID string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT p.uid FROM issues i JOIN projects p ON p.id = i.project_id
           WHERE i.id = ?`, srcI.ID,
	).Scan(&projectUID))
	assert.Equal(t, tgtP.UID, projectUID)

	var payload string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT payload FROM events WHERE type='issue.moved'
           ORDER BY id DESC LIMIT 1`).Scan(&payload))
	assert.Contains(t, payload, `"from_project_uid":"`+srcP.UID+`"`)
	assert.Contains(t, payload, `"to_project_uid":"`+tgtP.UID+`"`)
	assert.Contains(t, payload, `"from_short_id":"`+srcI.ShortID+`"`)
	assert.Contains(t, payload, `"to_short_id":"`+res.NewShortID+`"`)
}

func TestMoveIssueProject_RefusesCrossProjectLinks(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	srcP, _ := d.CreateProject(ctx, "src")
	tgtP, _ := d.CreateProject(ctx, "target")
	a := seedIssueInProject(t, d, srcP.ID, "A", "tester")
	b := seedIssueInProject(t, d, srcP.ID, "B", "tester")
	seedLink(t, d, srcP.ID, a.ID, b.ID, "blocks", "tester")

	_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: a.ID, FromProjectID: srcP.ID, ToProjectID: tgtP.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	var cpe *db.CrossProjectLinksError
	require.ErrorAs(t, err, &cpe)
	assert.GreaterOrEqual(t, len(cpe.Blockers), 1)
}

func TestMoveIssueProject_RefusesRecurrencePinned(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src, _ := d.CreateProject(ctx, "src")
	tgt, _ := d.CreateProject(ctx, "target")
	iss := seedIssueInProject(t, d, src.ID, "weekly", "tester")
	rid := seedRecurrence(t, d, src.ID, "FREQ=WEEKLY", "2026-05-11", "UTC", "tpl")
	_, err := d.ExecContext(ctx,
		`UPDATE issues SET recurrence_id = ?, occurrence_key = '2026-05-11' WHERE id = ?`,
		rid, iss.ID)
	require.NoError(t, err)

	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: src.ID, ToProjectID: tgt.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	var rpe *db.RecurrencePinnedError
	require.ErrorAs(t, err, &rpe)
}

func TestMoveIssueProject_RefusesRevisionConflict(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src, _ := d.CreateProject(ctx, "src")
	tgt, _ := d.CreateProject(ctx, "target")
	iss := seedIssueInProject(t, d, src.ID, "stale", "tester")

	_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: src.ID, ToProjectID: tgt.ID,
		IfMatchRev: 99, Actor: "tester",
	})
	var rce *db.RevisionConflictError
	require.ErrorAs(t, err, &rce)
	assert.Equal(t, int64(1), rce.CurrentRevision)
}

func TestMoveIssueProject_RefusesSameProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, _ := d.CreateProject(ctx, "p")
	iss := seedIssueInProject(t, d, p.ID, "same", "tester")

	_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: p.ID, ToProjectID: p.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same")
}

func TestMoveIssueProject_RefusesSoftDeleted(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src, _ := d.CreateProject(ctx, "src")
	tgt, _ := d.CreateProject(ctx, "tgt")
	iss := seedIssueInProject(t, d, src.ID, "bye", "tester")

	// Soft-delete the issue.
	_, _, _, err := d.SoftDeleteIssue(ctx, iss.ID, "tester")
	require.NoError(t, err)

	// Move should fail: soft-deleted issue is not "in" the project.
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: src.ID, ToProjectID: tgt.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in project")
}

func TestMoveIssueProject_RehomesImportMappings(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src, _ := d.CreateProject(ctx, "src")
	tgt, _ := d.CreateProject(ctx, "tgt")
	iss := seedIssueInProject(t, d, src.ID, "mappable", "tester")

	// Seed an import_mapping for the issue in the source project.
	issID := iss.ID
	_, err := d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:     "gh",
		ExternalID: "ext-42",
		ObjectType: "issue",
		ProjectID:  src.ID,
		IssueID:    &issID,
	})
	require.NoError(t, err)

	// Move the issue.
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: src.ID, ToProjectID: tgt.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	require.NoError(t, err)

	// The mapping must now point at the target project.
	got, err := d.ImportMappingBySource(ctx, tgt.ID, "gh", "issue", "ext-42")
	require.NoError(t, err)
	assert.Equal(t, tgt.ID, got.ProjectID)
	assert.Equal(t, &iss.ID, got.IssueID)

	// The old mapping in src must be gone.
	_, err = d.ImportMappingBySource(ctx, src.ID, "gh", "issue", "ext-42")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestMoveIssueProject_RehomesImportMappings_SkipsCollisions(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src, _ := d.CreateProject(ctx, "src")
	tgt, _ := d.CreateProject(ctx, "tgt")
	iss := seedIssueInProject(t, d, src.ID, "collision", "tester")
	tgtIss := seedIssueInProject(t, d, tgt.ID, "existing", "tester")

	issID := iss.ID

	// Mapping in src for this issue.
	_, err := d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:     "gh",
		ExternalID: "ext-99",
		ObjectType: "issue",
		ProjectID:  src.ID,
		IssueID:    &issID,
	})
	require.NoError(t, err)

	// A pre-existing mapping in tgt with the same (source, external_id, object_type).
	tgtIssID := tgtIss.ID
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:     "gh",
		ExternalID: "ext-99",
		ObjectType: "issue",
		ProjectID:  tgt.ID,
		IssueID:    &tgtIssID,
	})
	require.NoError(t, err)

	// Move should succeed — the colliding src row is dropped, tgt mapping is kept.
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: src.ID, ToProjectID: tgt.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	require.NoError(t, err)

	// Target mapping is untouched (still points at tgtIss).
	got, err := d.ImportMappingBySource(ctx, tgt.ID, "gh", "issue", "ext-99")
	require.NoError(t, err)
	assert.Equal(t, &tgtIssID, got.IssueID)

	// Source mapping is gone (was the colliding row we dropped).
	_, err = d.ImportMappingBySource(ctx, src.ID, "gh", "issue", "ext-99")
	assert.ErrorIs(t, err, db.ErrNotFound)
}
