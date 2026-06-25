package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestPurgeProject_DeletesArchivedProjectAndFreesName(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "doomed", Author: "tester",
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: iss.ID, Author: "tester", Body: "bye",
	})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester", Force: true})
	require.NoError(t, err)

	pl, err := d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	assert.Equal(t, p.ID, pl.ProjectID)
	assert.Equal(t, "spoke-project", pl.ProjectName)
	assert.Equal(t, int64(1), pl.IssueCount)
	assert.Equal(t, int64(1), pl.CommentCount)
	assert.Len(t, pl.UID, 26)
	assert.NotEmpty(t, pl.OriginInstanceUID)

	// Project row gone; name is free for reuse.
	_, err = d.ProjectByID(ctx, p.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	fresh, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	assert.NotEqual(t, p.ID, fresh.ID)

	// No dangling FKs.
	var violations int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Equal(t, 0, violations)
}

func TestPurgeProject_RefusesActiveProject(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.ErrorIs(t, err, db.ErrProjectNotArchived)
}

func TestPurgeProject_NotFound(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	_, err := d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: 9999, Actor: "tester"})
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestPurgeProject_RefusesFederated(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	// Insert a hub federation_bindings row directly. hub_project_uid is
	// NOT NULL with a 26-char CHECK, so it must be supplied even for a hub
	// row whose hub_url/hub_project_id default to empty.
	_, err = d.ExecContext(ctx,
		`INSERT INTO federation_bindings(project_id, role, hub_project_uid, enabled, push_enabled)
		 VALUES(?, 'hub', '0000000000000000000000000A', 1, 0)`, p.ID)
	require.NoError(t, err)

	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	var fed *db.ProjectFederatedError
	require.ErrorAs(t, err, &fed)
	assert.Equal(t, db.FederationRoleHub, fed.Role)
}

func TestPurgeProject_RefusesFederatedSpoke(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	// A spoke binding must also block purge, with a role-aware error. Federation
	// must be torn down (kata federation leave) before the project can be purged.
	// Spoke rows require a non-empty hub_url and a positive hub_project_id (schema
	// CHECKs), unlike hub rows where those default to empty/zero.
	_, err = d.ExecContext(ctx,
		`INSERT INTO federation_bindings(
		   project_id, role, hub_url, hub_project_id, hub_project_uid, enabled, push_enabled)
		 VALUES(?, 'spoke', 'https://hub.example', 1, '0000000000000000000000000A', 1, 0)`, p.ID)
	require.NoError(t, err)

	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	var fed *db.ProjectFederatedError
	require.ErrorAs(t, err, &fed)
	assert.Equal(t, db.FederationRoleSpoke, fed.Role)
}

func TestPurgeProject_DetachesMovedInIssueEvents(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	origin, err := d.CreateProject(ctx, "origin-project")
	require.NoError(t, err)
	dest, err := d.CreateProject(ctx, "dest-project")
	require.NoError(t, err)
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: origin.ID, Title: "mover", Author: "tester"})
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: iss.ID, FromProjectID: origin.ID, ToProjectID: dest.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: dest.ID, Actor: "tester", Force: true})
	require.NoError(t, err)

	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: dest.ID, Actor: "tester"})
	require.NoError(t, err)

	// origin's pre-move events survive (rows kept) but are detached.
	var orphaned int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE project_id = ? AND issue_id IS NULL`, origin.ID).Scan(&orphaned))
	assert.Positive(t, orphaned, "moved-in issue's origin events should be detached, not deleted")
	var violations int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Equal(t, 0, violations)
}

// TestPurgeProject_ReservesResetCursorForBothStreams checks that the project
// purge cursor is visible to the SSE reset backstop on both the global stream
// (projectID 0) and the purged project's own stream (projectID == p.ID), so a
// resuming subscriber discovers the reset regardless of its subscription scope.
func TestPurgeProject_ReservesResetCursorForBothStreams(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "x", Author: "tester"})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester", Force: true})
	require.NoError(t, err)

	pl, err := d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	require.NotNil(t, pl.PurgeResetAfterEventID)

	// Global stream sees the cursor.
	global, err := d.PurgeResetCheck(ctx, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, *pl.PurgeResetAfterEventID, global)
	// Purged project's own stream sees it too (defensive SSE check uses subscriber projectID).
	scoped, err := d.PurgeResetCheck(ctx, 0, p.ID)
	require.NoError(t, err)
	assert.Equal(t, *pl.PurgeResetAfterEventID, scoped)
}

// TestPurgeProject_PreservesIssuePurgeLog guards the deliberate spec decision
// that purging a project does NOT delete purge_log (issue tombstones): those
// rows have no FK to projects so prior-purge audit history must survive a
// project purge. A future stray `DELETE FROM purge_log` in the cascade would
// flip this assertion.
func TestPurgeProject_PreservesIssuePurgeLog(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "purge me first", Author: "tester",
	})
	require.NoError(t, err)

	// Purge the issue: writes a purge_log tombstone keyed on project_id.
	_, err = d.PurgeIssue(ctx, iss.ID, "tester", nil)
	require.NoError(t, err)

	var before int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM purge_log WHERE project_id = ?`, p.ID).Scan(&before))
	require.GreaterOrEqual(t, before, 1, "PurgeIssue must write a purge_log row")

	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	// The issue tombstone survives the project purge.
	var after int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM purge_log WHERE project_id = ?`, p.ID).Scan(&after))
	assert.GreaterOrEqual(t, after, 1, "issue purge_log tombstones must outlive a project purge")

	var violations int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Equal(t, 0, violations)
}
