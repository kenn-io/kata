package daemon_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
)

func TestMCPEventsCatalogChangesReloadOlderInboxContents(t *testing.T) {
	for _, change := range []string{"archive", "access grant"} {
		t.Run(change, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				f := newEventsFixture(t, store)
				var recorder postRecorder
				svc, err := mcpevents.NewService(mcpevents.ServiceConfig{
					Store: store, Authority: f.authority,
					Keys:    mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "mcp-events.key")),
					Webhook: mcpEventsWebhookRecording(t, &recorder),
				})
				require.NoError(t, err)
				subscribed, err := svc.Subscribe(ctx, f.outsider, mcpevents.ForwarderScope{All: true}, mcpevents.SubscribeRequest{
					Name: mcpevents.EventName, Arguments: mcpevents.Arguments{For: "example-user"},
					Delivery: mcpevents.Delivery{Mode: "webhook", URL: mcpEventsURL, Secret: mcpEventsSecret},
				})
				require.NoError(t, err)
				require.NoError(t, svc.Drain(ctx))
				require.Empty(t, recorder.all(), "new subscriptions start after existing pending requests")
				before, err := store.GetMCPEventSubscription(ctx, subscribed.ID)
				require.NoError(t, err)
				if change == "archive" {
					_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.public.ID, Actor: db.BootstrapActor, Force: true})
				} else {
					_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.hidden.UID, Visibility: "all"}, db.BootstrapActor)
				}
				require.NoError(t, err)
				svc.Recheck()
				require.NoError(t, svc.Drain(ctx))
				posts := recorder.all()
				require.NotEmpty(t, posts, "catalog changes must reload inbox state even when its requests precede the settled cursor")
				assert.Equal(t, "gap", posts[0].Type)
				after, err := store.GetMCPEventSubscription(ctx, subscribed.ID)
				require.NoError(t, err)
				assert.Greater(t, after.ContinuityGeneration, before.ContinuityGeneration)
				assert.Equal(t, db.MCPEventStateActive, after.State)
				count := len(posts)
				svc.Recheck()
				require.NoError(t, svc.Drain(ctx))
				assert.Len(t, recorder.all(), count, "unchanged visibility does not repeat the gap")
			})
		})
	}
}
