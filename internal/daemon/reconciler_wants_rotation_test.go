package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

// directSpokeProject creates a project whose hub is the selected producer under
// the client's recipe and an enabled direct spoke binding.
func directSpokeProject(t *testing.T, store db.Storage, client *embedding.Client, name string) (db.Project, db.ProjectEmbeddingProducer) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, name)
	require.NoError(t, err)
	recipe, err := client.ArtifactIdentity("", "", "")
	require.NoError(t, err)
	producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: "00000000000000000000000002", Recipe: recipe}
	raw, err := json.Marshal(producer)
	require.NoError(t, err)
	_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "system", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	project, err = store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	return project, producer
}

func reconcilerIndex(t *testing.T, store db.Storage) *vector.Index {
	t.Helper()
	ctx := t.Context()
	if pg, ok := store.(*pgstore.Store); ok {
		ix, err := vector.OpenPostgres(ctx, pg.DB)
		require.NoError(t, err)
		release, err := ix.AcquireReconcilerLease(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, release()) })
		t.Cleanup(func() { require.NoError(t, ix.Close()) })
		return ix
	}
	ix, err := vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	return ix
}

// countingEmbedder returns a client whose endpoint counts encoder requests.
func countingEmbedder(t *testing.T) (*embedding.Client, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected encoder call", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 2})
	require.NoError(t, err)
	return client, &requests
}

// A project whose oldest pending documents the hub cannot serve must still
// reach its newer documents: the wants page rotates, and what the hub returns
// is imported without waiting on the first pending page.
func TestReconcileFetchesPastDocumentsTheHubCannotServe(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		client, requests := countingEmbedder(t)
		project, producer := directSpokeProject(t, store, client, "spoke-project")
		var newest db.Issue
		for i := range 301 {
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("document %03d", i), Author: "member"})
			require.NoError(t, err)
			newest = issue
		}
		ix := reconcilerIndex(t, store)
		key := client.Generation().Fingerprint()

		retained := false
		hook := func(_ context.Context, _ embedding.ArtifactIdentity, wants []vector.ArtifactWant) map[string]error {
			for _, want := range wants {
				if want.IssueUID != newest.UID {
					continue
				}
				identity, err := client.ArtifactIdentity(project.UID, newest.UID, producer.ProducerInstanceUID)
				require.NoError(t, err)
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(newest.Title, newest.Body), [][]float32{{1, 0}})
				require.NoError(t, err)
				_, err = store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				retained = true
			}
			return map[string]error{project.UID: nil}
		}
		const maxPasses = 12 // ceil(300/32) + 2
		passes, err := daemon.ReconcileRepeatedForTest(ctx, store, ix, client, daemon.ReconcilerConfig{BatchSize: 64, FetchFederatedArtifacts: hook}, maxPasses, func(int) bool {
			backlog, err := ix.Backlog(ctx, key)
			require.NoError(t, err)
			return backlog == 300
		})
		require.NoError(t, err)
		require.True(t, retained, "the newest document was requested within %d passes", maxPasses)
		backlog, err := ix.Backlog(ctx, key)
		require.NoError(t, err)
		require.EqualValues(t, 300, backlog, "only the newest document was imported (pass %d)", passes)
		require.Zero(t, requests.Load(), "the hub's bytes are imported without encoding")
	})
}

// Only an enabled direct spoke binding can use a hub's artifacts, so a pass
// without one never scans pending wants or calls the fetch hook.
func TestReconcileSkipsArtifactWantsWithoutDirectSpokeBinding(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Input []string `json:"input"`
			}
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			data := make([]map[string]any, len(request.Input))
			for i := range request.Input {
				data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
			}
			require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
		}))
		t.Cleanup(server.Close)
		client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 2})
		require.NoError(t, err)
		ix := reconcilerIndex(t, store)
		hookCalls := 0
		cfg := daemon.ReconcilerConfig{BatchSize: 64, FetchFederatedArtifacts: func(context.Context, embedding.ArtifactIdentity, []vector.ArtifactWant) map[string]error {
			hookCalls++
			return nil
		}}
		seed := func(name string, binding *db.FederationBinding) db.Project {
			project, err := store.CreateProject(ctx, name)
			require.NoError(t, err)
			_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "pending " + name, Author: "member"})
			require.NoError(t, err)
			if binding != nil {
				binding.ProjectID, binding.HubProjectUID = project.ID, project.UID
				_, err = store.UpsertFederationBinding(ctx, *binding)
				require.NoError(t, err)
			}
			return project
		}
		spoke := func(enabled bool) *db.FederationBinding {
			return &db.FederationBinding{Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, Actor: "member", Enabled: enabled, PushEnabled: true}
		}
		pass := func() int {
			calls, err := daemon.ReconcileOnceCountingWantsForTest(ctx, store, ix, client, cfg)
			require.NoError(t, err)
			return calls
		}

		seed("private-project", nil)
		seed("paused-project", spoke(false))
		seed("hub-project", &db.FederationBinding{Role: db.FederationRoleHub, HubURL: "https://hub.example", HubProjectID: 43, Actor: "member", Enabled: true})
		require.Zero(t, pass(), "no enabled direct spoke binding")
		require.Zero(t, hookCalls)

		seed("spoke-project", spoke(true))
		require.Equal(t, 1, pass())
	})
}

type memoryFederationCredentials struct {
	mu    sync.Mutex
	items map[string]config.FederationCredential
}

func (s *memoryFederationCredentials) FederationCredential(_ context.Context, projectUID string) (config.FederationCredential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.items[projectUID]
	return credential, ok, nil
}

func (s *memoryFederationCredentials) StoreFederationCredential(_ context.Context, projectUID string, credential config.FederationCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]config.FederationCredential{}
	}
	s.items[projectUID] = credential
	return nil
}

func (s *memoryFederationCredentials) DeleteFederationCredential(_ context.Context, projectUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, projectUID)
	return nil
}

// A throttled second page must stay selected until the real fetcher attempts
// it; otherwise a short page wraps the project back to its first 32 issues.
func TestReconcilerKeepsPageAfterDirectFetcherThrottle(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		client, encodeRequests := countingEmbedder(t)
		recipe, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		producerUID := "00000000000000000000000002"
		project, err := store.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: producerUID, Recipe: recipe}
		producerRaw, err := json.Marshal(producer)
		require.NoError(t, err)
		_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
			ProjectID: project.ID, Actor: "system",
			Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: jsontext.Value(producerRaw)},
		})
		require.NoError(t, err)

		var issues []db.Issue
		for i := range 33 {
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: fmt.Sprintf("document %02d", i), Author: "member",
			})
			require.NoError(t, err)
			issues = append(issues, issue)
		}
		slices.SortFunc(issues, func(a, b db.Issue) int { return strings.Compare(a.UID, b.UID) })
		firstPage := make([]string, 32)
		for i, issue := range issues[:32] {
			firstPage[i] = issue.UID
		}
		available := issues[32]
		identity, err := client.ArtifactIdentity(project.UID, available.UID, producerUID)
		require.NoError(t, err)
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(available.Title, available.Body), [][]float32{{1, 0}})
		require.NoError(t, err)

		var requests [][]string
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested := append([]string(nil), r.URL.Query()["issue_uid"]...)
			requests = append(requests, requested)
			body := api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{}}
			if slices.Contains(requested, available.UID) {
				body.Artifacts = []embedding.EmbeddingArtifact{artifact}
			}
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.MarshalWrite(w, body))
		}))
		t.Cleanup(hub.Close)
		binding := db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL,
			HubProjectID: 42, HubProjectUID: project.UID, Actor: "member",
			Enabled: true, PushEnabled: true, AllowInsecure: true,
		}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		credentials := &memoryFederationCredentials{}
		require.NoError(t, credentials.StoreFederationCredential(ctx, project.UID, config.FederationCredential{
			HubURL: hub.URL, HubProjectID: binding.HubProjectID, Token: "hub-token", Actor: binding.Actor, AllowInsecure: true,
		}))
		fetcher := federation.NewDirectArtifactFetcher(store, credentials, clientpkg.Opts{}, 5*time.Second)

		ix := reconcilerIndex(t, store)
		key := client.Generation().Fingerprint()
		cfg := daemon.ReconcilerConfig{BatchSize: 64, FetchFederatedArtifacts: fetcher.Fetch}
		passes, err := daemon.ReconcileRepeatedForTest(ctx, store, ix, client, cfg, 3, func(pass int) bool {
			switch pass {
			case 1:
				require.Len(t, requests, 1)
				require.Equal(t, firstPage, requests[0])
			case 2:
				require.Len(t, requests, 1, "the fetcher throttle skips the second-page request")
				<-time.After(5*time.Second + 10*time.Millisecond)
			case 3:
				require.Len(t, requests, 2)
				require.Equal(t, []string{available.UID}, requests[1])
				return true
			}
			return false
		})
		require.NoError(t, err)
		require.Equal(t, 3, passes)
		backlog, err := ix.Backlog(ctx, key)
		require.NoError(t, err)
		require.EqualValues(t, 32, backlog, "the artifact on the second page is imported without local generation")
		require.Zero(t, encodeRequests.Load())
	})
}
