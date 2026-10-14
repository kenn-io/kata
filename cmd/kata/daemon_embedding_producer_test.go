package main

import (
	"context"
	"crypto/ed25519"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R7/runtime: daemon startup publishes its root producer assignment through the
// same broadcaster as ordinary committed project metadata changes.
func TestStartEmbeddingReconcilerPublishesProducerSelection(t *testing.T) {
	home := setupKataEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "root-producer-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Root worker content", Author: "member"})
	require.NoError(t, err)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	t.Cleanup(provider.Close)
	ec := config.EmbeddingsConfig{BaseURL: provider.URL, Model: "example-model", Dims: 2, APIKey: "producer-test-secret"}
	client, _, err := preflightEmbeddingStartup(ec, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	t.Cleanup(sub.Unsub)
	workers := newDaemonWorkerGroup()
	_, idx, health, _, err := startEmbeddingReconciler(ctx, workers, nil, ec, client, filepath.Join(home, "vectors.db"), store, broadcaster, log.New(io.Discard, "", 0))
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); require.True(t, workers.Wait(context.Background())); require.NoError(t, idx.Close()) })
	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool { return health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, calls.Load())
	current, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	producer, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
	require.NoError(t, err)
	require.NotNil(t, producer)
	require.Equal(t, store.InstanceUID(), producer.ProducerInstanceUID)
	select {
	case msg := <-sub.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, "project.metadata_updated", msg.Event.Type)
		require.Equal(t, "system", msg.Event.Actor)
		retained, err := store.EventsByUIDs(ctx, project.ID, []string{msg.Event.UID})
		require.NoError(t, err)
		require.Len(t, retained, 1)
	case <-time.After(time.Second):
		t.Fatal("root producer assignment missing from live publisher")
	}
}
