package daemon_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
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

// A full restore can roll an inbox back, for example by bringing back a
// request that was later cleared, with no event after the restored cursor. The
// first pass after a restore therefore asks the receiver to reload, even
// though its visible projects are unchanged.
func TestMCPEventsRestoreFromBackupSendsAGap(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newEventsFixture(t, store)
		keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
		var recorder postRecorder
		svc, err := mcpevents.NewService(mcpevents.ServiceConfig{
			Store: store, Authority: f.authority, Keys: mcpevents.NewFileKeyProvider(keyPath),
			Webhook: mcpEventsWebhookRecording(t, &recorder),
		})
		require.NoError(t, err)
		_, err = svc.Subscribe(ctx, f.outsider, mcpevents.ForwarderScope{All: true}, mcpevents.SubscribeRequest{
			Name: mcpevents.EventName, Arguments: mcpevents.Arguments{For: "example-user"},
			Delivery: mcpevents.Delivery{Mode: "webhook", URL: mcpEventsURL, Secret: mcpEventsSecret},
		})
		require.NoError(t, err)
		require.NoError(t, svc.Drain(ctx))
		require.Empty(t, recorder.all())

		var backup bytes.Buffer
		require.NoError(t, jsonl.Export(ctx, store, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
		restored, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, restored.Close()) })
		require.NoError(t, jsonl.Import(ctx, &backup, restored))

		svc, err = mcpevents.NewService(mcpevents.ServiceConfig{
			Store: restored, Keys: mcpevents.NewFileKeyProvider(keyPath),
			Authority: daemon.NewMCPEventsAuthority(daemon.ServerConfig{DB: restored, Auth: config.AuthConfig{Token: "configured", RequireTokenIdentity: true}}),
			Webhook:   mcpEventsWebhookRecording(t, &recorder),
		})
		require.NoError(t, err)
		require.True(t, svc.Availability().Enabled, svc.Availability().Reason)
		require.NoError(t, svc.Drain(ctx))
		posts := recorder.all()
		require.Len(t, posts, 1, "the restored subscription reloads once")
		assert.Equal(t, "gap", posts[0].Type)
	})
}
