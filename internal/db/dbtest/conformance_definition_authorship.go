package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A pushing spoke cannot invent, rewrite or take over the author of a cron
// definition. Authorship comes from the create event or an approved adoption
// snapshot, and the hub refuses creates and snapshots for definitions it holds.
func checkPushedDefinitionAuthorship(t *testing.T, hub db.Storage) error {
	ctx := t.Context()
	project, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, project.ID, "hub-user")
	require.NoError(t, err)
	_, workflow := nativeFederationDefinitionsAs(t, hub, project, "hub-user")
	events, err := hub.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	var created db.Event
	for _, event := range events {
		if event.Type == "cron.workflow.created" {
			created = event
		}
	}
	require.NotEmpty(t, created.UID)
	spokeUID, err := uid.New()
	require.NoError(t, err)
	sourceEventID := int64(0)
	push := func(kind string, edit func(*db.CronDefinitionEvent), physicalMS int64) error {
		var payload db.CronDefinitionEvent
		require.NoError(t, json.Unmarshal([]byte(created.Payload), &payload))
		edit(&payload)
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		event := portableNativeEvent(created)
		event.EventUID, err = uid.New()
		require.NoError(t, err)
		event.Type = kind
		event.OriginInstanceUID = spokeUID
		event.Actor = "worker"
		event.HLCPhysicalMS = physicalMS
		event.Payload = jsontext.Value(raw)
		resignNativeEvent(t, &event)
		sourceEventID++
		_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spokeUID, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: sourceEventID, Event: event}}})
		return err
	}
	earlier := created.HLCPhysicalMS - 60_000

	t.Run("edit cannot introduce a definition under another author", func(t *testing.T) {
		fresh, err := uid.New()
		require.NoError(t, err)
		err = push("cron.workflow.updated", func(p *db.CronDefinitionEvent) { p.UID = fresh; p.Author = "victim" }, created.HLCPhysicalMS+1)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		_, err = hub.CronWorkflow(ctx, project.ID, fresh)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("backdated edit cannot rewrite the author", func(t *testing.T) {
		require.NoError(t, push("cron.workflow.updated", func(p *db.CronDefinitionEvent) { p.Author = "forged-author" }, earlier))
		stored, err := hub.CronWorkflow(ctx, project.ID, workflow.UID)
		require.NoError(t, err)
		require.Equal(t, "hub-user", stored.Author)
	})
	t.Run("fresh snapshot cannot take over a known definition", func(t *testing.T) {
		err := push("cron.workflow.snapshot", func(p *db.CronDefinitionEvent) {
			p.Author = "worker"
			p.DefinitionEventUID = created.UID
			p.DefinitionHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: earlier, OriginInstanceUID: spokeUID}
		}, created.HLCPhysicalMS+2)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		require.ErrorContains(t, err, "targets existing workflow")
	})
	t.Run("backdated create cannot take over a known definition", func(t *testing.T) {
		err := push("cron.workflow.created", func(p *db.CronDefinitionEvent) { p.Author = "worker" }, earlier)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		require.ErrorContains(t, err, "targets existing workflow")
	})
	stored, err := hub.CronWorkflow(ctx, project.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, "hub-user", stored.Author)
	return nil
}
