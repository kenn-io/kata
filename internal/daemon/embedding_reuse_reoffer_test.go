package daemon_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
)

// A producer that reuses retained bytes for content that became current again
// still offers them to peers. A fast peer that acknowledged and later pruned
// the artifact while a slow peer kept the original offer open would otherwise
// never receive the revert's vectors.
func TestReconcileArtifactsReoffersReusedArtifactToPrunedPeer(t *testing.T) {
	for name, reuse := range map[string]func(context.Context, *testing.T, *vector.Index, string, db.Storage, embedding.ArtifactIdentity){
		"reconcile": func(ctx context.Context, t *testing.T, ix *vector.Index, key string, store db.Storage, recipe embedding.ArtifactIdentity) {
			reused, err := ix.ReconcileArtifacts(ctx, key, store, recipe)
			require.NoError(t, err)
			require.Equal(t, 1, reused, "the revert reuses the retained bytes instead of embedding again")
		},
		// Documents beyond the reconciler's first page are reused while the
		// fill scans pending work; a one-document scan batch drives that path.
		"fill": func(ctx context.Context, t *testing.T, ix *vector.Index, key string, store db.Storage, recipe embedding.ArtifactIdentity) {
			encode := func(_ context.Context, texts []string) ([][]float32, error) {
				vectors := make([][]float32, len(texts))
				for i, text := range texts {
					if strings.Contains(text, "Content A") {
						return nil, errors.New("the reverted content must be reused, not embedded")
					}
					vectors[i] = []float32{0, 1}
				}
				return vectors, nil
			}
			_, _, err := ix.FillWithArtifactsDetailed(ctx, key, store, recipe, encode, 1, nil, func(bool) {})
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) { runReusedArtifactReoffer(t, reuse) })
	}
}

func runReusedArtifactReoffer(t *testing.T, reuse func(context.Context, *testing.T, *vector.Index, string, db.Storage, embedding.ArtifactIdentity)) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		public, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		enroll := func(name, spoke string) db.FederationEnrollment {
			t.Helper()
			parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: name + "-parent-test-token", Actor: "member", AdminActor: "admin"})
			require.NoError(t, err)
			//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
			grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: spoke, ProtocolVersion: db.RelayProtocolVersion, Token: name + "-relay-test-token"})
			require.NoError(t, err)
			return grant.Enrollment
		}
		fast := enroll("fast", "00000000000000000000000006")
		slow := enroll("slow", "00000000000000000000000007")
		ackAll := func(enrollment db.FederationEnrollment) {
			t.Helper()
			pending, err := store.PendingRelayDeliveries(ctx, enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
			require.NoError(t, err)
			require.NotEmpty(t, pending)
			last := pending[len(pending)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, enrollment.RelayBindingUID, last.Epoch, db.RelayStreamArtifact, last.Sequence, last.Digest))
		}

		client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		identity, err := client.ArtifactIdentity(f.private.UID, f.issue.UID, store.InstanceUID())
		require.NoError(t, err)
		artifacts := store.(db.EmbeddingArtifactStorage)
		retainCurrent := func(body string) embedding.EmbeddingArtifact {
			t.Helper()
			edited, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: f.issue.ID, Body: &body, Actor: "member"})
			require.NoError(t, err)
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(edited.Title, edited.Body), [][]float32{{1, 0}})
			require.NoError(t, err)
			durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
			require.NoError(t, err)
			require.True(t, durable)
			return artifact
		}
		original := retainCurrent("Content A")
		ackAll(fast)
		retainCurrent("Content B")
		ackAll(fast)
		_, err = artifacts.StoredEmbeddingArtifact(ctx, f.private.UID, original.Digest)
		require.NoError(t, err, "the slow peer's open offer keeps the original bytes retained")
		revert := "Content A"
		_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: f.issue.ID, Body: &revert, Actor: "member"})
		require.NoError(t, err)

		var ix *vector.Index
		if pg, ok := store.(*pgstore.Store); ok {
			ix, err = vector.OpenPostgres(ctx, pg.DB)
			require.NoError(t, err)
			release, err := ix.AcquireReconcilerLease(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, release()) })
		} else {
			ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
			require.NoError(t, err)
		}
		t.Cleanup(func() { require.NoError(t, ix.Close()) })
		key := client.Generation().Fingerprint()
		require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))
		_, err = ix.RefreshMirror(ctx, store)
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", store.InstanceUID())
		require.NoError(t, err)
		reuse(ctx, t, ix, key, store, recipe)

		pending, err := store.PendingRelayDeliveries(ctx, fast.RelayBindingUID, db.RelayStreamArtifact, 32)
		require.NoError(t, err)
		require.Len(t, pending, 1, "the fast peer that pruned the original is offered it again")
		require.Equal(t, original.Digest, pending[0].SourceHash)
		require.NotEqual(t, original.Digest, pending[0].SourceUID)
		slowPending, err := store.PendingRelayDeliveries(ctx, slow.RelayBindingUID, db.RelayStreamArtifact, 32)
		require.NoError(t, err)
		require.NotEmpty(t, slowPending, "the slow peer keeps its open offers")
	})
}

// failingRetentionStore refuses the first re-offer retention so the reuse
// must leave the document pending and succeed on the next pass.
type failingRetentionStore struct {
	db.Storage
	failures *int
}

func (s failingRetentionStore) RetainEmbeddingArtifact(ctx context.Context, artifact embedding.EmbeddingArtifact) (bool, error) {
	if *s.failures > 0 {
		*s.failures--
		return false, errors.New("retention store unavailable")
	}
	return s.Storage.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
}

func (s failingRetentionStore) StoredEmbeddingArtifact(ctx context.Context, projectUID, digest string) (embedding.EmbeddingArtifact, error) {
	return s.Storage.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, projectUID, digest)
}

func (s failingRetentionStore) EmbeddingArtifactManifests(ctx context.Context, projectUID string, limit int) ([]embedding.ArtifactManifest, error) {
	return s.Storage.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, projectUID, limit)
}

func (s failingRetentionStore) ExportEmbeddingArtifacts(ctx context.Context, filter db.ExportFilter, issueUIDs ...string) iter.Seq2[db.ImportRecord, error] {
	return s.Storage.(db.EmbeddingArtifactExporter).ExportEmbeddingArtifacts(ctx, filter, issueUIDs...)
}

// A re-offer that fails before the index stamps the document is retried on
// the next pass; the document is never indexed without its offer.
func TestReconcileArtifactsRetriesReofferAfterRetentionFailure(t *testing.T) {
	runReusedArtifactReoffer(t, func(ctx context.Context, t *testing.T, ix *vector.Index, key string, store db.Storage, recipe embedding.ArtifactIdentity) {
		failures := 1
		failing := failingRetentionStore{Storage: store, failures: &failures}
		_, err := ix.ReconcileArtifacts(ctx, key, failing, recipe)
		require.ErrorContains(t, err, "retention store unavailable")
		reused, err := ix.ReconcileArtifacts(ctx, key, failing, recipe)
		require.NoError(t, err)
		require.Equal(t, 1, reused, "the document stayed pending until its re-offer was retained")
	})
}
