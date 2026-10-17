package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

func TestCronSundaySevenPeerCompatibility(t *testing.T) {
	const document = `{"version":1,"kind":"job","trigger":{"kind":"cron","cron":"0 9 * * 7"},"action":{"kind":"execute","prompt":"Review changes"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	projectUID := newUID(t)
	payload := CronDefinitionEvent{UID: newUID(t), ProjectUID: projectUID, Name: "Sunday review", Definition: jsontext.Value(document), Author: "worker", CreatedAt: now, UpdatedAt: now}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	event := RemoteEvent{EventUID: newUID(t), OriginInstanceUID: newUID(t), ProjectUID: projectUID, Type: "cron.job.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now, Payload: jsontext.Value(raw)}
	t.Run("push", func(t *testing.T) {
		require.NoError(t, ValidateCronFederationEvent(event))
	})
	t.Run("pull", func(t *testing.T) {
		require.NoError(t, ValidateAcceptedCronEvent(event))
	})
	t.Run("restore history", func(t *testing.T) {
		project := &ProjectExport{ID: 1, UID: projectUID}
		historical := &EventExport{ID: 1, UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectID: 1, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, Payload: event.Payload, CreatedAt: now.Format(time.RFC3339Nano)}
		require.NoError(t, validateCronImportEvents([]ImportRecord{project, historical}))
	})
	t.Run("restore live definition", func(t *testing.T) {
		job, err := cron.DecodeJob([]byte(document))
		require.NoError(t, err)
		record := CronJobExport{ID: 1, ProjectID: 1, UID: payload.UID, Name: payload.Name, DefinitionEventUID: event.EventUID, DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: now.UnixMilli(), OriginInstanceUID: event.OriginInstanceUID}, Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now, Definition: job}
		require.NoError(t, ValidateCronRecord(&record))
	})
	t.Run("feature requirements", func(t *testing.T) {
		got, err := PayloadRequiredFeatures(event.Type, string(event.Payload))
		require.NoError(t, err)
		zeroPayload := strings.Replace(string(event.Payload), `0 9 * * 7`, `0 9 * * 0`, 1)
		want, err := PayloadRequiredFeatures(event.Type, zeroPayload)
		require.NoError(t, err)
		require.Equal(t, want, got, "Sunday 7 must not add a peer capability")
	})
}
