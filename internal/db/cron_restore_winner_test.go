package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An owner restore must not revive an older runnable projection when a later
// retained held edit or tombstone was inserted before materialization.
func TestOwnerRestoreRetainedWorkflowWinner(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID, workflowUID, origin := newUID(t), newUID(t), newUID(t)
	project := &ProjectExport{ID: 1, UID: projectUID}
	original := &CronWorkflowExport{CronDefinition: CronDefinition{ID: 1, ProjectID: 1, UID: workflowUID, Name: "old", Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now, DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: 1, OriginInstanceUID: origin}}}
	for _, tc := range []struct {
		name, document   string
		deleted, omitted bool
	}{
		{"held edit", `{"version":1,"steps":[{"kind":"argv"}]}`, false, false},
		{"held tombstone", `{"version":1,"steps":[{"kind":"argv"}]}`, true, false},
		{"unrepresentable null", `null`, false, true},
		{"unrepresentable version", `{"version":0,"steps":[]}`, false, true},
		{"later valid", `{"version":1,"steps":[]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := CronDefinitionEvent{UID: workflowUID, ProjectUID: projectUID, Name: "new", Author: "worker", CreatedAt: now, UpdatedAt: now, Definition: jsontext.Value(tc.document)}
			kind := "cron.workflow.updated"
			if tc.deleted {
				body.DeletedAt = &now
				kind = "cron.workflow.deleted"
			}
			payload, err := json.Marshal(body)
			require.NoError(t, err)
			event := &EventExport{UID: newUID(t), ProjectID: 1, OriginInstanceUID: origin, Type: kind, HLCPhysicalMS: 2, Payload: payload}
			records := []ImportRecord{project, original, event}
			result := ReconcileOwnerCronWorkflowReplay(records)
			require.Same(t, original, records[1], "original backup must remain unchanged")
			require.Same(t, event, result[len(result)-1], "retained event must remain intact")
			if tc.omitted {
				require.Len(t, result, 2)
				return
			}
			row := result[1].(*CronWorkflowExport)
			if tc.name == "later valid" {
				require.Same(t, original, row)
				return
			}
			require.Equal(t, event.UID, row.DefinitionEventUID)
			require.Equal(t, "new", row.Name)
			require.Equal(t, tc.deleted, row.DeletedAt != nil)
			require.JSONEq(t, tc.document, string(row.RawDefinition))
		})
	}
}

func TestOwnerRestoreRepairsSameRevisionHeldOriginal(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID, workflowUID, origin, eventUID := newUID(t), newUID(t), newUID(t), newUID(t)
	project := &ProjectExport{ID: 1, UID: projectUID}
	row := &CronWorkflowExport{CronDefinition: CronDefinition{ID: 1, ProjectID: 1, UID: workflowUID, Name: "Review", Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now, DefinitionEventUID: eventUID, DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: 2, OriginInstanceUID: origin}}}
	body := CronDefinitionEvent{UID: workflowUID, ProjectUID: projectUID, Name: row.Name, Author: row.Author, CreatedAt: now, UpdatedAt: now, Definition: jsontext.Value(`{"version":1,"steps":[null]}`)}
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	event := &EventExport{UID: eventUID, ProjectID: 1, OriginInstanceUID: origin, Type: "cron.workflow.created", HLCPhysicalMS: 2, Payload: payload}
	result := ReconcileOwnerCronWorkflowReplay([]ImportRecord{project, row, event})
	repaired := result[1].(*CronWorkflowExport)
	require.JSONEq(t, string(body.Definition), string(repaired.RawDefinition))
	again := ReconcileOwnerCronWorkflowReplay(result)
	require.Same(t, repaired, again[1], "already retained original must not be rewritten")
}
