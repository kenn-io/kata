package vector_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// A local embedding request can already be in flight when disconnect commits
// its push fence. That result must not add a relay artifact the leave cannot
// drain after the upstream grant is revoked.
func TestEmbeddingArtifactFinishingAfterDisconnectFence(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := idx.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}

			project, err := source.CreateProject(ctx, "artifact-disconnect-project")
			require.NoError(t, err)
			_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: "In-flight embedding", Body: strings.Repeat("body ", 40), Author: "member",
			})
			require.NoError(t, err)
			public, _, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			require.NoError(t, source.PinRootAuthority(ctx, db.RootKeyPin{
				ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public,
			}))
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{
				ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
				HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true,
			})
			require.NoError(t, err)
			relayConfig := db.RelayBindingConfig{
				ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000005",
				UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
				HubPath: []string{rootUID, source.InstanceUID()}, LocalActor: "member", ResetEpoch: 1,
			}
			_, err = source.SetRelayBindingConfig(db.WithRelayStateCapture(ctx), project.ID, relayConfig)
			require.NoError(t, err)
			for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamArtifact, db.RelayStreamReceipt} {
				pending, err := source.PendingRelayDeliveries(ctx, relayConfig.BindingUID, stream, 1024)
				require.NoError(t, err)
				if len(pending) > 0 {
					last := pending[len(pending)-1]
					require.NoError(t, source.AckRelayDeliveries(ctx, relayConfig.BindingUID, last.Epoch, stream, last.Sequence, last.Digest))
				}
			}
			_, err = idx.RefreshMirror(ctx, source)
			require.NoError(t, err)
			client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example/v1", Model: "example-model", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
			require.NoError(t, err)
			key := client.Generation().Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, client.Generation()))

			encodeStarted := make(chan struct{}, 1)
			releaseEncode := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseEncode) }) }
			defer release()
			encode := func(ctx context.Context, texts []string) ([][]float32, error) {
				select {
				case encodeStarted <- struct{}{}:
				default:
				}
				select {
				case <-releaseEncode:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				vectors := make([][]float32, len(texts))
				for i := range vectors {
					vectors[i] = []float32{1, 0}
				}
				return vectors, nil
			}
			done := make(chan error, 1)
			go func() {
				_, err := idx.FillWithArtifacts(ctx, key, source, recipe, encode, 0, nil, nil)
				done <- err
			}()
			select {
			case <-encodeStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("embedding request did not become in flight")
			}

			fencer, ok := source.(db.RelayDisconnectFenceStore)
			require.True(t, ok, "native store must expose the atomic disconnect fence")
			require.NoError(t, fencer.FenceRelayDisconnect(ctx, project.ID))
			release()
			select {
			case fillErr := <-done:
				require.True(t, fillErr == nil || errors.Is(fillErr, kitvec.ErrStale), "in-flight embedding should save locally or be discarded as stale")
			case <-time.After(5 * time.Second):
				t.Fatal("embedding fill did not finish after the encoder was released")
			}

			lifecycle, ok := source.(db.RelayLifecycleStore)
			require.True(t, ok)
			require.NoError(t, lifecycle.ValidateRelayLifecycle(ctx, project.ID), "post-fence embedding must not enqueue undrainable relay artifact work")
		})
	}
}
