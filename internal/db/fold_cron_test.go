package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

const foldCronUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
const foldProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAB"
const foldOriginUID = "01ARZ3NDEKTSV4RRFFQ69G5FAC"

func cronFoldEvent(t testing.TB, kind string, index int, deleted bool) db.FoldEvent {
	t.Helper()
	operation := "updated"
	if index == 1 {
		operation = "created"
	}
	payload := map[string]any{
		"uid": foldCronUID, "project_uid": foldProjectUID,
		"name": fmt.Sprintf("Document %d", index), "author": "worker",
		"created_at": "2026-10-03T12:00:00Z", "updated_at": "2026-10-03T12:00:01Z",
	}
	if deleted {
		operation = "deleted"
		payload["deleted_at"] = "2026-10-03T12:00:01Z"
	}
	if kind == "job" {
		payload["definition"] = map[string]any{"version": 1, "kind": "job", "enabled": false, "trigger": map[string]any{"kind": "interval", "interval_seconds": int64(index)}, "action": map[string]any{"kind": "execute", "prompt": fmt.Sprintf("Prompt %d", index)}, "issue": map[string]any{"kind": "per-run", "title": "Review"}, "overlap": "forbid", "catchup": "skip"}
	} else {
		payload["definition"] = cron.WorkflowDefinition{Version: 1, About: fmt.Sprintf("About %d", index), Steps: []cron.WorkflowStep{{Key: "review", Kind: "command", Command: fmt.Sprintf("echo %d", index)}}}
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return db.FoldEvent{UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", index), ProjectUID: foldProjectUID, OriginInstanceUID: foldOriginUID, Actor: "worker", Type: "cron." + kind + "." + operation, HLCPhysicalMS: int64(index), CreatedAt: "2026-10-03T12:00:01Z", Payload: raw}
}

// Decode the public fold value to test the whole-document contract, independent
// of internal fold bookkeeping and backend-local IDs/revisions.
func foldedDefinitions(t testing.TB, events []db.FoldEvent) (map[string]db.CronJob, map[string]db.CronWorkflow) {
	t.Helper()
	raw, err := json.Marshal(db.FoldEvents(events))
	require.NoError(t, err)
	var out struct {
		CronJobs      map[string]db.CronJob
		CronWorkflows map[string]db.CronWorkflow
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out.CronJobs, out.CronWorkflows
}

func TestCronFoldWholeDocumentsAndSnapshotClocks(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			older := cronFoldEvent(t, kind, 1, false)
			deleted := cronFoldEvent(t, kind, 2, true)
			restored := cronFoldEvent(t, kind, 3, false)
			restored.Type = "cron." + kind + ".restored"
			snapshot := older
			snapshot.Type = "cron." + kind + ".snapshot"
			snapshot.HLCPhysicalMS = 100
			snapshot.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
			var payload map[string]any
			require.NoError(t, json.Unmarshal(older.Payload, &payload))
			payload["definition_event_uid"] = older.UID
			payload["definition_hlc"] = cron.HLC{Version: 1, PhysicalMS: older.HLCPhysicalMS, OriginInstanceUID: older.OriginInstanceUID}
			snapshot.Payload, _ = json.Marshal(payload)
			for _, tc := range []struct {
				name    string
				events  []db.FoldEvent
				want    db.FoldEvent
				deleted bool
			}{
				{"delete beats delayed edit", []db.FoldEvent{deleted, older, older}, deleted, true},
				{"restore beats delete", []db.FoldEvent{restored, deleted, older, deleted}, restored, false},
				{"baseline does not edit winner", []db.FoldEvent{restored, snapshot, deleted}, restored, false},
				{"snapshot preserves original", []db.FoldEvent{snapshot}, older, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					jobs, workflows := foldedDefinitions(t, tc.events)
					var actual db.CronDefinition
					if kind == "job" {
						actual = jobs[foldCronUID].CronDefinition
					} else {
						actual = workflows[foldCronUID].CronDefinition
					}
					require.Equal(t, tc.want.UID, actual.DefinitionEventUID)
					require.Equal(t, tc.want.HLCPhysicalMS, actual.DefinitionHLC.PhysicalMS)
					require.Equal(t, tc.deleted, actual.DeletedAt != nil)
				})
			}
		})
	}
}

func TestCronFoldKeepsAdoptionAuthorAcrossNewerEdits(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			created := cronFoldEvent(t, kind, 1, false)
			snapshot := created
			snapshot.Type = "cron." + kind + ".snapshot"
			snapshot.HLCPhysicalMS = 3
			snapshot.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
			var payload map[string]any
			require.NoError(t, json.Unmarshal(created.Payload, &payload))
			payload["author"] = "operator"
			payload["definition_event_uid"] = created.UID
			payload["definition_hlc"] = cron.HLC{Version: 1, PhysicalMS: created.HLCPhysicalMS, OriginInstanceUID: created.OriginInstanceUID}
			snapshot.Payload, _ = json.Marshal(payload)

			updated := cronFoldEvent(t, kind, 2, false)
			updatedPayload := map[string]any{}
			require.NoError(t, json.Unmarshal(updated.Payload, &updatedPayload))
			updatedPayload["author"] = "worker"
			updated.Payload, _ = json.Marshal(updatedPayload)
			for _, events := range [][]db.FoldEvent{
				{created, updated, snapshot},
				{snapshot, created, updated},
				{updated, snapshot, created},
			} {
				jobs, workflows := foldedDefinitions(t, events)
				if kind == "job" {
					require.Equal(t, "operator", jobs[foldCronUID].Author)
					require.Equal(t, updated.UID, jobs[foldCronUID].DefinitionEventUID)
					require.Equal(t, "Document 2", jobs[foldCronUID].Name)
				} else {
					require.Equal(t, "operator", workflows[foldCronUID].Author)
					require.Equal(t, updated.UID, workflows[foldCronUID].DefinitionEventUID)
					require.Equal(t, "Document 2", workflows[foldCronUID].Name)
				}
			}
		})
	}
}

func TestCronSnapshotEnvelopeValidation(t *testing.T) {
	base := cronFoldEvent(t, "job", 1, false)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(base.Payload, &payload))
	payload["definition_event_uid"] = base.UID
	payload["definition_hlc"] = cron.HLC{Version: 1, PhysicalMS: base.HLCPhysicalMS, OriginInstanceUID: base.OriginInstanceUID}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	event := db.RemoteEvent{EventUID: base.UID, OriginInstanceUID: base.OriginInstanceUID, ProjectUID: base.ProjectUID, Actor: base.Actor, Type: "cron.job.snapshot", HLCPhysicalMS: -1, Payload: raw}
	require.ErrorIs(t, db.ValidateCronFederationEvent(event), db.ErrFederationIngestValidation, "a valid original clock cannot excuse an invalid snapshot envelope")
}

// The final document in a monotonic writer trace is the hand-derived oracle.
// Arrival order and duplicate count must not change any of its fields. Both
// sibling definitions use this law; arbitrary bytes vary the permutation,
// duplicate count and tombstone trace. Snapshot provenance is covered above.
func FuzzCronFoldConvergence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{255, 255, 0, 0, 1})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		count := max(1, min(len(data), 64))
		for _, kind := range []string{"job", "workflow"} {
			events := make([]db.FoldEvent, 0, count*2)
			var winner db.FoldEvent
			for i := range count {
				deleted := i < len(data) && data[i]&1 != 0
				winner = cronFoldEvent(t, kind, i+1, deleted)
				dimension := 0
				if len(data) > 0 {
					dimension = int(data[0]) % 4
				}
				switch dimension {
				case 1:
					winner.HLCPhysicalMS = 1
					winner.HLCCounter = int64(i + 1)
				case 2:
					winner.HLCPhysicalMS = 1
					winner.OriginInstanceUID = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", i+1)
				case 3:
					winner.HLCPhysicalMS = 1
				}

				events = append(events, winner)
				if i < len(data) && data[i]&2 != 0 {
					events = append(events, winner)
				}
			}
			for i := len(events) - 1; i > 0; i-- {
				j := 0
				if len(data) > 0 {
					j = int(data[i%len(data)]) % (i + 1)
				}
				events[i], events[j] = events[j], events[i]
			}
			jobs, workflows := foldedDefinitions(t, events)
			var actual db.CronDefinition
			var document jsontext.Value
			if kind == "job" {
				actual = jobs[foldCronUID].CronDefinition
				document, _ = json.Marshal(jobs[foldCronUID].Definition)
			} else {
				actual = workflows[foldCronUID].CronDefinition
				document, _ = json.Marshal(workflows[foldCronUID].Definition)
			}
			var expected struct {
				Name       string         `json:"name"`
				Definition jsontext.Value `json:"definition"`
				DeletedAt  *time.Time     `json:"deleted_at"`
			}
			require.NoError(t, json.Unmarshal(winner.Payload, &expected))
			require.Equal(t, winner.UID, actual.DefinitionEventUID)
			require.Equal(t, expected.Name, actual.Name)
			require.Equal(t, expected.DeletedAt, actual.DeletedAt)
			require.JSONEq(t, string(expected.Definition), string(document))
		}
	})
}
