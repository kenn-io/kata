package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCloseDone_MaterializesNextRecurrence(t *testing.T) {
	d, ctx, p, rec := setupRecurrence(t, db.CreateRecurrenceIn{
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "Pay rent"},
	})
	firstID, _ := seedRecurrenceInstance(t, d, p.ID, rec.ID, "2026-05-11", "Pay rent")

	_, _, _, err := d.CloseIssue(ctx, firstID, "done", "tester", "paid", nil)
	require.NoError(t, err)

	// Next-week instance must exist.
	var n int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM issues
		 WHERE recurrence_id = ? AND occurrence_key = ?`,
		rec.ID, "2026-05-18",
	).Scan(&n))
	assert.Equal(t, 1, n, "next-week instance should be auto-created")

	assertEventCount(t, d, "recurrence.materialized", 1)

	// issue.created event for the new instance should also exist.
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM events WHERE type='issue.created'
		  AND issue_id = (SELECT id FROM issues WHERE recurrence_id = ? AND occurrence_key = ?)`,
		rec.ID, "2026-05-18",
	).Scan(&n))
	assert.Equal(t, 1, n, "issue.created event should be emitted for new instance")

	// recurrence next_occurrence_key should advance to the week after 2026-05-18.
	var nextKey *string
	require.NoError(t, d.QueryRow(`
		SELECT next_occurrence_key FROM recurrences WHERE id = ?`, rec.ID,
	).Scan(&nextKey))
	require.NotNil(t, nextKey)
	assert.Equal(t, "2026-05-25", *nextKey,
		"next_occurrence_key should advance two steps past the closed instance")
}

func TestCloseWontfix_DoesNotMaterialize(t *testing.T) {
	d, ctx, p, rec := setupRecurrence(t, db.CreateRecurrenceIn{
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "x"},
	})
	firstID, _ := seedRecurrenceInstance(t, d, p.ID, rec.ID, "2026-05-11", "x")

	_, _, _, err := d.CloseIssue(ctx, firstID, "wontfix", "tester", "skip", nil)
	require.NoError(t, err)

	assertEventCount(t, d, "recurrence.materialized", 0)

	// Confirm no new recurrence instance was created.
	var n int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM issues WHERE recurrence_id = ? AND status='open'`, rec.ID,
	).Scan(&n))
	assert.Equal(t, 0, n, "no open instances should exist after wontfix close")
}

func TestCloseDone_NonRecurrenceIssueIsNoOp(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)
	iss := seedIssueInProject(t, d, p.ID, "regular", "tester")

	_, _, _, err = d.CloseIssue(ctx, iss.ID, "done", "tester", "msg", nil)
	require.NoError(t, err)

	// No recurrence-related events.
	var n int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM events
		 WHERE type IN (
		   'recurrence.materialized',
		   'recurrence.materialization_skipped'
		 )
	`).Scan(&n))
	assert.Equal(t, 0, n, "non-recurrence issue close must not emit recurrence events")
}

func TestCloseDone_LabelsSeededFromTemplate(t *testing.T) {
	d, ctx, p, rec := setupRecurrence(t, db.CreateRecurrenceIn{
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{
			Title:  "Weekly sync",
			Labels: []string{"recurring", "auto"},
		},
	})
	firstID, _ := seedRecurrenceInstance(t, d, p.ID, rec.ID, "2026-05-11", "Weekly sync")

	_, _, _, err := d.CloseIssue(ctx, firstID, "done", "tester", "", nil)
	require.NoError(t, err)

	// Retrieve the new instance's ID.
	var newID int64
	require.NoError(t, d.QueryRow(`
		SELECT id FROM issues WHERE recurrence_id = ? AND occurrence_key = ?`,
		rec.ID, "2026-05-18",
	).Scan(&newID))

	// Both template labels must be present.
	var labelCount int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM issue_labels WHERE issue_id = ?`, newID,
	).Scan(&labelCount))
	assert.Equal(t, 2, labelCount, "new instance should inherit both template labels")
}

func TestCloseDone_MetadataContainsScheduledOn(t *testing.T) {
	d, ctx, p, rec := setupRecurrence(t, db.CreateRecurrenceIn{
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "t"},
	})
	firstID, _ := seedRecurrenceInstance(t, d, p.ID, rec.ID, "2026-05-11", "t")

	_, _, _, err := d.CloseIssue(ctx, firstID, "done", "tester", "", nil)
	require.NoError(t, err)

	var metadata string
	require.NoError(t, d.QueryRow(`
		SELECT metadata FROM issues WHERE recurrence_id = ? AND occurrence_key = ?`,
		rec.ID, "2026-05-18",
	).Scan(&metadata))
	assert.Contains(t, metadata, `"scheduled_on"`)
	assert.Contains(t, metadata, "2026-05-18")
}

func TestMaterializeNext_UniqueConflict_SkipsAndAdvancesCursor(t *testing.T) {
	d, ctx, p, rec := setupRecurrence(t, db.CreateRecurrenceIn{
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "x"},
	})
	// Pre-seed the next instance for 2026-05-18 directly to force a UNIQUE conflict.
	_, existingUID := seedRecurrenceInstance(t, d, p.ID, rec.ID, "2026-05-18", "x")

	out, err := d.MaterializeNext(ctx, rec.ID, "2026-05-11", "tester")
	require.NoError(t, err)
	assert.True(t, out.Skipped, "should report skipped on UNIQUE conflict")
	assert.Equal(t, "2026-05-18", out.OccurrenceKey)
	assert.Equal(t, existingUID, out.NewIssueUID, "out.NewIssueUID should reflect the existing row")

	// Cursor advanced PAST the duplicate.
	var nextKey *string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT next_occurrence_key FROM recurrences WHERE id = ?`, rec.ID,
	).Scan(&nextKey))
	require.NotNil(t, nextKey, "next_occurrence_key must be set after a conflict skip")
	assert.Equal(t, "2026-05-25", *nextKey,
		"cursor must advance past the duplicate (2026-05-18) to 2026-05-25")

	// Skipped event emitted exactly once with the expected payload shape.
	var n int
	var payload string
	require.NoError(t, d.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(payload), '') FROM events
		 WHERE type='recurrence.materialization_skipped'`,
	).Scan(&n, &payload))
	assert.Equal(t, 1, n)
	assert.Contains(t, payload, `"recurrence_uid":"`+rec.UID+`"`)
	assert.Contains(t, payload, `"occurrence_key":"2026-05-18"`)
	assert.Contains(t, payload, `"existing_issue_uid":"`+existingUID+`"`)
	assert.Contains(t, payload, `"reason":"already_exists"`)
}

func TestMaterializeNext_AfterConflict_NoRegressionOnReplay(t *testing.T) {
	d, ctx, p, rec := setupRecurrence(t, db.CreateRecurrenceIn{
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "x"},
	})
	seedRecurrenceInstance(t, d, p.ID, rec.ID, "2026-05-18", "x")

	// First call: hits the conflict, advances cursor to 2026-05-25.
	out1, err := d.MaterializeNext(ctx, rec.ID, "2026-05-11", "tester")
	require.NoError(t, err)
	assert.True(t, out1.Skipped)

	// Second call walks from afterKey=2026-05-18, finds 2026-05-25 — cleanly materializes.
	out2, err := d.MaterializeNext(ctx, rec.ID, "2026-05-18", "tester")
	require.NoError(t, err)
	assert.False(t, out2.Skipped, "second call must materialize cleanly, not re-conflict")
	assert.Equal(t, "2026-05-25", out2.OccurrenceKey)
	assert.NotZero(t, out2.NewIssueID)
}
