package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestArgvCompatReplayKeepsKindOnlyWinners(t *testing.T) {
	for _, operation := range []string{"created", "updated", "deleted", "restored", "snapshot"} {
		t.Run(operation, func(t *testing.T) {
			event := cronFoldEvent(t, "workflow", 2, operation == "deleted")
			event.Type = "cron.workflow." + operation
			var payload map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			payload["definition"] = jsontext.Value(`{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`)
			if operation == "snapshot" {
				payload["definition_event_uid"], _ = json.Marshal(event.UID)
				payload["definition_hlc"], _ = json.Marshal(db.CronDefinitionHLC{Version: 1, PhysicalMS: event.HLCPhysicalMS, OriginInstanceUID: event.OriginInstanceUID})
			}
			var err error
			event.Payload, err = json.Marshal(payload)
			require.NoError(t, err)
			remote := db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, Payload: event.Payload}
			require.Error(t, db.ValidateAcceptedCronEvent(remote), "historical acceptance must reject reserved decoding")
			require.Error(t, db.ValidateCronFederationEvent(remote))
			projection := db.FoldEvents([]db.FoldEvent{event})
			winner, ok := projection.CronWorkflows[foldCronUID]
			require.True(t, ok)
			require.Equal(t, event.UID, winner.DefinitionEventUID)
			require.Equal(t, operation == "deleted", winner.DeletedAt != nil)
			require.Equal(t, "argv", winner.Definition.Steps[0].Kind)
		})
	}
}

func TestArgvCompatReplayDoesNotReviveOlderWorkflow(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		older := cronFoldEvent(t, "workflow", 1, false)
		newer := cronFoldEvent(t, "workflow", 2, deleted)
		var payload map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(newer.Payload, &payload))
		payload["definition"] = jsontext.Value(`{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`)
		var err error
		newer.Payload, err = json.Marshal(payload)
		require.NoError(t, err)
		for _, events := range [][]db.FoldEvent{{older, newer}, {newer, older}} {
			winner := db.FoldEvents(events).CronWorkflows[foldCronUID]
			require.Equal(t, newer.UID, winner.DefinitionEventUID)
			require.Equal(t, deleted, winner.DeletedAt != nil)
		}
	}
}
