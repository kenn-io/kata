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
	var originalValue CronWorkflowExport
	originalValue.CronDefinition = CronDefinition{ID: 1, ProjectID: 1, UID: workflowUID, Name: "old", Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now, DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: 1, OriginInstanceUID: origin}}
	original := &originalValue
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
			require.Equal(t, event.UID, row.DefinitionEventUID)
			require.Equal(t, "new", row.Name)
			require.Equal(t, tc.deleted, row.DeletedAt != nil)
			if tc.name == "later valid" {
				require.Equal(t, 1, row.Definition.Version)
			} else {
				require.JSONEq(t, tc.document, string(row.RawDefinition))
			}
		})
	}
}

func TestOwnerRestoreRepairsSameRevisionHeldOriginal(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID, workflowUID, origin, eventUID := newUID(t), newUID(t), newUID(t), newUID(t)
	project := &ProjectExport{ID: 1, UID: projectUID}
	var rowValue CronWorkflowExport
	rowValue.CronDefinition = CronDefinition{ID: 1, ProjectID: 1, UID: workflowUID, Name: "Review", Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now, DefinitionEventUID: eventUID, DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: 2, OriginInstanceUID: origin}}
	row := &rowValue
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

// Equal evidence for one revision must not depend on import order: the
// unsupported original wins over its normalized snapshot, keeps the canonical
// author, and still yields to genuinely newer projections.
func TestOwnerRestoreTiedSnapshotFavorsUnsupportedOriginal(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID, workflowUID, origin, eventUID := newUID(t), newUID(t), newUID(t), newUID(t)
	held := `{"version":1,"steps":null}`
	clock := CronDefinitionHLC{Version: 1, PhysicalMS: 2, OriginInstanceUID: origin}
	newBody := func(author, document string) CronDefinitionEvent {
		return CronDefinitionEvent{UID: workflowUID, ProjectUID: projectUID, Name: "Review", Author: author, CreatedAt: now, UpdatedAt: now, Definition: jsontext.Value(document)}
	}
	marshal := func(body CronDefinitionEvent) []byte {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		return payload
	}
	original := &EventExport{UID: eventUID, ProjectID: 1, OriginInstanceUID: origin, Type: "cron.workflow.updated", HLCPhysicalMS: 2, Payload: marshal(newBody("editor", held))}
	snapshotBody := newBody("worker", `{"version":1,"steps":[]}`)
	snapshotBody.DefinitionEventUID, snapshotBody.DefinitionHLC = eventUID, &clock
	snapshot := &EventExport{UID: newUID(t), ProjectID: 1, OriginInstanceUID: origin, Type: "cron.workflow.snapshot", HLCPhysicalMS: 5, Payload: marshal(snapshotBody)}
	project := &ProjectExport{ID: 1, UID: projectUID}
	for _, tc := range []struct {
		name   string
		order  []*EventExport
		rowMS  int64
		newest bool
	}{
		{"original first", []*EventExport{original, snapshot}, 2, false},
		{"snapshot first", []*EventExport{snapshot, original}, 2, false},
		{"newer projection", []*EventExport{snapshot, original}, 9, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rowValue CronWorkflowExport
			rowValue.CronDefinition = CronDefinition{ID: 1, ProjectID: 1, UID: workflowUID, Name: "Review", Author: "worker", Revision: 3, CreatedAt: now, UpdatedAt: now, DefinitionEventUID: eventUID, DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: tc.rowMS, OriginInstanceUID: origin}}
			rowValue.Definition.Version = 1
			row := &rowValue
			result := ReconcileOwnerCronWorkflowReplay([]ImportRecord{project, row, tc.order[0], tc.order[1]})
			require.Same(t, tc.order[0], result[2], "retained events must remain intact")
			require.Same(t, tc.order[1], result[3], "retained events must remain intact")
			if tc.newest {
				require.Same(t, row, result[1])
				return
			}
			got := result[1].(*CronWorkflowExport)
			require.JSONEq(t, held, string(got.RawDefinition))
			require.Equal(t, eventUID, got.DefinitionEventUID)
			require.Equal(t, "worker", got.Author)
			require.Equal(t, int64(3), got.Revision)
		})
	}
}
