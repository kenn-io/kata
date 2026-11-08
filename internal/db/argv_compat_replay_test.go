package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestArgvCompatReplayRefusesKindOnlyDocuments(t *testing.T) {
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
			require.Empty(t, projection.CronWorkflows)
			require.NotEmpty(t, projection.Warnings)
		})
	}
}
