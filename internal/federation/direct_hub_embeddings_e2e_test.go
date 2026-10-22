package federation_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/vector"
)

// countingProvider is a fake embeddings endpoint that counts document inputs
// and query inputs separately. A text maps to the axis named by the first
// keyword it contains, so a query finds the document sharing its keyword.
type countingProvider struct {
	client    *embedding.Client
	documents atomic.Int64
	queries   atomic.Int64
}

func newCountingProvider(t *testing.T) *countingProvider {
	t.Helper()
	p := &countingProvider{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		data := make([]map[string]any, len(request.Input))
		for i, text := range request.Input {
			if strings.HasPrefix(text, "query: ") {
				p.queries.Add(1)
			} else {
				require.True(t, strings.HasPrefix(text, "document: "), "unexpected input %q", text)
				p.documents.Add(1)
			}
			vector := []float32{0.01, 0.01, 0.01}
			for axis, keyword := range []string{"alpha", "beta", "gamma"} {
				if strings.Contains(text, keyword) {
					vector[axis] = 1
					break
				}
			}
			data[i] = map[string]any{"index": i, "embedding": vector}
		}
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)
	client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 3, DocumentPrefix: "document: ", QueryPrefix: "query: "})
	require.NoError(t, err)
	p.client = client
	return p
}

// directEmbeddingNode is one daemon with its own vector index and provider.
type directEmbeddingNode struct {
	*relayMatrixNode
	provider *countingProvider
	index    *vector.Index
	allowed  func(context.Context, string, embedding.ArtifactIdentity, activity.Admission, bool) (bool, error)
	fetch    func(context.Context, embedding.ArtifactIdentity, []vector.ArtifactWant) map[string]error
}

func newDirectEmbeddingNode(t *testing.T, backend, account string) *directEmbeddingNode {
	t.Helper()
	base := newRelayMatrixNode(t, backend, account, true)
	var ix *vector.Index
	var err error
	if pg, ok := base.store.(*pgstore.Store); ok {
		ix, err = vector.OpenPostgres(t.Context(), pg.DB)
	} else {
		ix, err = vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"))
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	return &directEmbeddingNode{relayMatrixNode: base, provider: newCountingProvider(t), index: ix}
}

// reconcile runs one fresh reconciler to its first successful pass.
func (n *directEmbeddingNode) reconcile(t *testing.T) *daemon.Reconciler {
	t.Helper()
	worker := daemon.NewReconciler(n.store, n.index, n.provider.client, daemon.ReconcilerConfig{SweepEvery: time.Hour, FederationProducerAllowed: n.allowed, FetchFederatedArtifacts: n.fetch})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	//nolint:kennlint // The reconciler is a real background worker; bounded polling observes its first completed pass.
	require.Eventually(t, func() bool { return worker.Health().LastSuccessAt != nil }, 30*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("reconciler did not stop")
	}
	return worker
}

func TestDirectSpokeReusesHubEmbeddings(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			hub := newDirectEmbeddingNode(t, backend, "hub-member")
			spoke := newDirectEmbeddingNode(t, backend, "spoke-member")

			project, err := hub.store.CreateProject(ctx, "hub-project")
			require.NoError(t, err)
			hub.project = project
			_, err = hub.store.EnableProjectFederation(ctx, project.ID, "worker")
			require.NoError(t, err)
			var issues []db.Issue
			for _, title := range []string{"alpha rollout notes", "beta migration plan", "gamma incident review"} {
				issue, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "assistant"})
				require.NoError(t, err)
				issues = append(issues, issue)
			}

			// 1. The hub embeds each document once and records itself as producer.
			hub.reconcile(t)
			require.EqualValues(t, 3, hub.provider.documents.Load())

			// Direct enrollment, then the spoke joins and pulls the hub's events,
			// including the producer metadata recorded by the reconcile above.
			grant, err := hub.store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.store.InstanceUID(), ProjectID: &project.ID, Capabilities: "pull,push", Actor: "worker"})
			require.NoError(t, err)
			meta, err := hub.store.ProjectByID(ctx, project.ID)
			require.NoError(t, err)
			horizon := hubReplayHorizon(t, hub)
			credential := config.FederationCredential{HubURL: hub.http.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "worker", Capabilities: "pull,push"}
			replica, err := daemon.EnsureFederationReplica(ctx, spoke.store, spoke.credentials, nil, daemon.EnsureFederationReplicaParams{
				HubURL: hub.http.URL, HubProjectID: project.ID, HubProjectUID: meta.UID, ProjectName: "spoke-project",
				ReplayHorizonEventID: horizon, Credential: credential, PushEnabled: true,
			})
			require.NoError(t, err)
			spoke.project = replica.Project
			spoke.credential = credential
			require.Nil(t, replica.Binding.RelayConfig, "the spoke is a direct spoke")

			fetcher := federation.NewDirectArtifactFetcher(spoke.store, spoke.credentials, clientpkg.Opts{}, time.Nanosecond)
			spoke.fetch = fetcher.Fetch
			spoke.allowed = func(ctx context.Context, projectUID string, recipe embedding.ArtifactIdentity, _ activity.Admission, syncArtifacts bool) (bool, error) {
				if !syncArtifacts {
					return federation.EmbeddingProducerAuthorized(ctx, spoke.store, spoke.credentials, projectUID, recipe)
				}
				return federation.EmbeddingProducerConnected(ctx, spoke.store, spoke.credentials, projectUID, recipe)
			}

			// 2. The spoke reuses the hub's artifacts without embedding documents.
			syncRelayMatrixNode(t, spoke.relayMatrixNode)
			spoke.reconcile(t)
			require.Zero(t, spoke.provider.documents.Load(), "the spoke must not embed documents the hub already embedded")
			key, ok, err := spoke.index.ActiveGeneration(ctx)
			require.NoError(t, err)
			require.True(t, ok)
			embedded, _, backlog, err := spoke.index.Coverage(ctx, key)
			require.NoError(t, err)
			require.EqualValues(t, 3, embedded)
			require.Zero(t, backlog)

			// 3. A semantic query against the spoke's index finds the target.
			queryVectors, err := spoke.provider.client.EmbedQuery(ctx, []string{"beta"})
			require.NoError(t, err)
			require.EqualValues(t, 1, spoke.provider.queries.Load())
			hits, err := spoke.index.Query(ctx, key, queryVectors[0], 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, issues[1].UID, hits[0].Doc)

			// 4. A metadata-only edit re-embeds nothing on either side.
			_, _, err = hub.store.AddLabelAndEvent(ctx, issues[0].ID, db.LabelEventParams{EventType: "issue.labeled", Label: "area:db", Actor: "assistant"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, spoke.relayMatrixNode)
			hub.reconcile(t)
			spoke.reconcile(t)
			require.EqualValues(t, 3, hub.provider.documents.Load())
			require.Zero(t, spoke.provider.documents.Load())

			// 5. Replaying the same sync changes nothing.
			syncRelayMatrixNode(t, spoke.relayMatrixNode)
			hub.reconcile(t)
			spoke.reconcile(t)
			require.EqualValues(t, 3, hub.provider.documents.Load())
			require.Zero(t, spoke.provider.documents.Load())
		})
	}
}

func hubReplayHorizon(t *testing.T, hub *directEmbeddingNode) int64 {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hub.http.URL+"/api/v1/projects/"+strconv.FormatInt(hub.project.ID, 10)+"/federation", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+hub.userToken)
	response, err := hub.http.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var body api.ProjectFederationBody
	require.NoError(t, json.UnmarshalRead(response.Body, &body))
	return body.ReplayHorizonEventID
}
