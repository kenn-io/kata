package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkLegacyAttentionRecurrence(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "legacy-attention")
	require.NoError(t, err)
	input := db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "scheduler", Rule: "FREQ=DAILY", DTStart: "2026-11-23", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Review"}}
	rec, _, err := store.CreateRecurrence(ctx, input)
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Existing", Author: "scheduler"})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	legacy := `{"work.attention":"working","kind":"weekly"}`
	_, err = q.ExecContext(ctx, `UPDATE recurrences SET template_metadata=$1 WHERE id=$2`, legacy, rec.ID)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, legacy, issue.ID)
	require.NoError(t, err)
	title := "Updated"
	patched, err := store.PatchRecurrence(ctx, db.PatchRecurrenceIn{RecurrenceID: rec.ID, IfMatchRev: rec.Revision, Actor: "scheduler", Update: db.RecurrenceUpdate{TemplateTitle: &title}})
	require.NoError(t, err)
	bad := jsontext.Value(legacy)
	_, err = store.PatchRecurrence(ctx, db.PatchRecurrenceIn{RecurrenceID: rec.ID, IfMatchRev: patched.NewRevision, Actor: "scheduler", Update: db.RecurrenceUpdate{TemplateMetadata: &bad}})
	require.ErrorIs(t, err, db.ErrInvalidRecurrence)
	occurrence, err := store.MaterializeNext(ctx, rec.ID, "2026-11-23", "scheduler")
	require.NoError(t, err)
	created, err := store.IssueByID(ctx, occurrence.NewIssueID)
	require.NoError(t, err)
	require.JSONEq(t, `{"kind":"weekly","scheduled_on":"2026-11-24","timezone":"UTC"}`, string(created.Metadata))
	_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: issue.ID, Recurrence: input})
	require.NoError(t, err)
	attached, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"work.attention":"working","kind":"weekly","scheduled_on":"2026-11-23","timezone":"UTC"}`, string(attached.Metadata))
	_, err = q.ExecContext(ctx, `UPDATE recurrences SET template_metadata=$1 WHERE id=$2`, `{"work.attention":"working","someday":"yes"}`, rec.ID)
	require.NoError(t, err)
	current, err := store.GetRecurrenceByID(ctx, rec.ID)
	require.NoError(t, err)
	title = "Invalid"
	_, err = store.PatchRecurrence(ctx, db.PatchRecurrenceIn{RecurrenceID: rec.ID, IfMatchRev: current.Revision, Actor: "scheduler", Update: db.RecurrenceUpdate{TemplateTitle: &title}})
	require.ErrorIs(t, err, db.ErrInvalidRecurrence)
	return nil
}
