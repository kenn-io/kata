package federation_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func TestConfiguredAdoptionPublishesFormerProjectResetBeforeCredentialFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := openReconcileStore(t)
		source, err := store.CreateProject(t.Context(), "source-project")
		require.NoError(t, err)
		target, err := store.CreateProject(t.Context(), "spoke-project")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Departed issue", Author: "worker"})
		require.NoError(t, err)
		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		credentials := newFakeCredentialStore()
		credentials.storeErr = errors.New("credential persistence unavailable")
		credentials.failStoreAt = 1
		broadcaster := daemon.NewEventBroadcaster()
		subscription := broadcaster.Subscribe(daemon.SubFilter{ProjectID: source.ID})
		defer subscription.Unsub()
		publisher := daemon.EventPublisher{Broadcaster: broadcaster}
		reconciler := federation.NewReconciler(federation.ReconcilerConfig{
			Store: store, Credentials: credentials,
			Targets:          []federation.Target{{Catalog: testCatalog(), Mapping: testMapping()}},
			HubFactory:       func(context.Context, config.CatalogDaemonConfig) (federation.Hub, error) { return newFakeHub(), nil },
			ProjectResetSink: publisher.Reset,
			Logger:           ioDiscardLogger(),
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := runReconciler(ctx, t, reconciler)
		defer func() {
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
		}()
		synctest.Wait()
		require.Equal(t, "credential_io", reconciler.Health().LastErrorCategory)
		adopted, err := store.ProjectByID(t.Context(), target.ID)
		require.NoError(t, err)
		require.Equal(t, hubProjectUID, adopted.UID, "adoption committed before credentials failed")
		select {
		case message := <-subscription.Ch:
			require.Equal(t, daemon.StreamKindReset, message.Kind)
			reset, err := store.PurgeResetCheck(t.Context(), 0, source.ID)
			require.NoError(t, err)
			require.Equal(t, reset, message.ResetID)
		default:
			t.Fatal("configured adoption did not publish the committed former-project reset")
		}
	})
}
