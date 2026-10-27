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

// Neither a daemon restart nor a policy change that leaves the subscriber's
// visible projects unchanged asks the receiver to reload its inbox.
func TestMCPEventsUnchangedVisibilitySendsNoGap(t *testing.T) {
	for _, change := range []string{"restart", "unrelated project"} {
		t.Run(change, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				f := newEventsFixture(t, store)
				keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
				var recorder postRecorder
				newService := func() *mcpevents.Service {
					svc, err := mcpevents.NewService(mcpevents.ServiceConfig{
						Store: store, Authority: f.authority, Keys: mcpevents.NewFileKeyProvider(keyPath),
						Webhook: mcpEventsWebhookRecording(t, &recorder),
					})
					require.NoError(t, err)
					return svc
				}
				svc := newService()
				_, err := svc.Subscribe(ctx, f.outsider, mcpevents.ForwarderScope{All: true}, mcpevents.SubscribeRequest{
					Name: mcpevents.EventName, Arguments: mcpevents.Arguments{For: "example-user"},
					Delivery: mcpevents.Delivery{Mode: "webhook", URL: mcpEventsURL, Secret: mcpEventsSecret},
				})
				require.NoError(t, err)
				require.NoError(t, svc.Drain(ctx))
				require.Empty(t, recorder.all())

				if change == "restart" {
					svc = newService()
				} else {
					// The outsider cannot see the new team project.
					project, err := store.CreateProject(ctx, "team-project")
					require.NoError(t, err)
					_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{f.teamUID}}, db.BootstrapActor)
					require.NoError(t, err)
					svc.Recheck()
				}
				require.NoError(t, svc.Drain(ctx))
				assert.Empty(t, recorder.all(), "the receiver's inbox view did not change")
			})
		})
	}
}
