package federation_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

// R4/A9: supported root recurrence operations must leave ordinary issue sync working.
func TestRelayRootRecurrenceKeepsContentFlow(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			personal := newRelayMatrixNode(t, backend, "personal-member")
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)
			rec, event, err := root.store.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{ProjectID: root.project.ID, Rule: "FREQ=DAILY", DTStart: "2026-10-04", Timezone: "UTC", Actor: root.account, Template: db.RecurrenceTemplate{Title: "Daily ordinary work"}})
			require.NoError(t, err)
			require.Equal(t, "recurrence.created", event.Type)
			title := "Updated daily ordinary work"
			_, err = root.store.PatchRecurrence(t.Context(), db.PatchRecurrenceIn{RecurrenceID: rec.ID, IfMatchRev: rec.Revision, Actor: root.account, Update: db.RecurrenceUpdate{TemplateTitle: &title}})
			require.NoError(t, err)
			materialized, err := root.store.MaterializeNext(t.Context(), rec.ID, "2026-10-03", root.account)
			require.NoError(t, err)
			require.NotEmpty(t, materialized.NewIssueUID)
			current, err := root.store.GetRecurrenceByID(t.Context(), rec.ID)
			require.NoError(t, err)
			_, err = root.store.SoftDeleteRecurrence(t.Context(), db.SoftDeleteRecurrenceIn{RecurrenceID: rec.ID, IfMatchRev: current.Revision, Actor: root.account})
			require.NoError(t, err)
			issue, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Content after recurrence", Author: root.account})
			require.NoError(t, err)
			syncRelayMatrixNode(t, personal)
			_, err = personal.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			received, err := personal.store.IssueByUID(t.Context(), materialized.NewIssueUID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, title, received.Title)
			localRecurrences, err := personal.store.ListRecurrencesByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.Empty(t, localRecurrences, "recurrence scheduling remains with its source instance")
		})
	}
}
