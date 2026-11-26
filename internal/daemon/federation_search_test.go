package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
	"go.kenn.io/kata/internal/vector"
	kataclient "go.kenn.io/kata/pkg/client"
)

func newFederationSearchHub(t *testing.T) (*testenv.Env, *vector.Index, db.Project, db.Issue, string) {
	t.Helper()
	ctx := t.Context()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &input))
		data := make([]map[string]any, len(input.Input))
		for i := range data {
			data[i] = map[string]any{"embedding": []float32{1, 0}}
		}
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
	}))
	t.Cleanup(provider.Close)
	emb, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example", Dims: 2})
	require.NoError(t, err)
	idx, err := vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, idx.Close()) })
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{Origin: "https://tasks.example", InstanceID: "hub"})
	require.NoError(t, err)
	hub := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Embedder, cfg.VectorIndex, cfg.WebSessions = emb, idx, manager })
	project, issue := createClaimHubIssue(t, hub)
	_, err = idx.RefreshMirror(ctx, hub.DB, nil)
	require.NoError(t, err)
	key := emb.Generation().Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, emb.Generation()))
	_, err = idx.Fill(ctx, key, leaseRecoveryEmbedder{}.EncodeFunc(), 64, nil, nil)
	require.NoError(t, err)
	require.NoError(t, idx.CutOver(ctx, key))
	return hub, idx, project, issue, key
}

func connectSearchMCP(t *testing.T, cfg daemon.ServerConfig) *sdkmcp.ClientSession {
	t.Helper()
	server := startTestServer(t, cfg)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	mcp, err := mcpserver.New(mcpserver.Options{Client: client, Actor: "tester", Version: "test", Scope: mcpserver.NewAllScope(), StartupSections: []string{"issue_discovery"}})
	require.NoError(t, err)
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := mcp.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestDirectSpokeSearchUnpulledReferenceCannotResolveCollision(t *testing.T) {
	ctx := t.Context()
	hub, _, project, issue, _ := newFederationSearchHub(t)
	spoke := testenv.New(t)
	replica, err := spoke.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
	require.NoError(t, err)
	local, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.ID, UID: "1" + issue.UID[1:], Title: "unrelated local issue", Author: "tester"})
	require.NoError(t, err)
	require.Equal(t, issue.ShortID, local.ShortID)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	grant := createClaimEnrollment(t, hub, project.ID, spoke.DB.InstanceUID(), "pull")
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token}))
	resp, raw := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=concept&mode=semantic", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var result struct {
		Results []struct {
			Issue db.Issue `json:"issue"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result.Results, 1)
	require.Zero(t, result.Results[0].Issue.ID)
	require.Equal(t, issue.UID, result.Results[0].Issue.ShortID)
	session := connectSearchMCP(t, daemon.ServerConfig{DB: spoke.DB})
	shown, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{"ref": replica.Name + "#" + result.Results[0].Issue.ShortID}})
	require.NoError(t, err)
	require.True(t, shown.IsError, "%+v", shown.Content)
}

func TestDirectSpokeSearchPostgresWithoutPgvector(t *testing.T) {
	ctx := t.Context()
	hub, _, project, issue, _ := newFederationSearchHub(t)
	dsn, cleanup := testenv.NewPlainPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	replica, err := store.CreateProjectWithUID(ctx, project.Name, project.UID)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.ID, UID: issue.UID, Title: issue.Title, Author: "tester"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	grant := createClaimEnrollment(t, hub, project.ID, store.InstanceUID(), "pull")
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token}))
	server := startTestServer(t, daemon.ServerConfig{DB: store, EmbeddingUnavailableReason: "pgvector unavailable"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/search?q=concept&mode=semantic", server.URL, replica.ID), nil)
	require.NoError(t, err)
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), issue.UID)
}

func newDirectSpokeSearchFixture(t *testing.T) (*testenv.Env, db.Project, db.Issue, db.Issue) {
	t.Helper()
	ctx := t.Context()
	hub, idx, project, issue, key := newFederationSearchHub(t)
	unpulled, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "unpulled issue", Author: "tester"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, hub.DB, nil)
	require.NoError(t, err)
	_, err = idx.Fill(ctx, key, leaseRecoveryEmbedder{}.EncodeFunc(), 64, nil, nil)
	require.NoError(t, err)
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{Origin: "https://spoke.example", InstanceID: "spoke"})
	require.NoError(t, err)
	spoke := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.EmbeddingUnavailableReason = "local vector index unavailable"
		cfg.WebSessions = manager
	})
	replica, err := spoke.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "tester", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	events, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, IssueUID: issue.UID, Types: []string{"issue.created"}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	e := events[0]
	_, err = spoke.DB.InsertRemoteEvent(ctx, replica.ID, db.RemoteEvent{EventUID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: e.ProjectUID, ProjectName: e.ProjectName, IssueUID: e.IssueUID, RelatedIssueUID: e.RelatedIssueUID, Type: e.Type, Actor: e.Actor, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, ContentHash: e.ContentHash, Payload: jsontext.Value(e.Payload), CreatedAt: e.CreatedAt})
	require.NoError(t, err)
	require.NoError(t, spoke.DB.MaterializeFederatedProject(ctx, replica.ID))
	grant := createClaimEnrollment(t, hub, project.ID, spoke.DB.InstanceUID(), "pull")
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token}))
	return spoke, replica, issue, unpulled
}

func TestDirectSpokeSearchUsesHubResults(t *testing.T) {
	spoke, replica, issue, unpulled := newDirectSpokeSearchFixture(t)
	resp, raw := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=concept&mode=semantic", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var body struct {
		Results []struct {
			Issue  db.Issue `json:"issue"`
			WebURL string   `json:"web_url"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Len(t, body.Results, 2)
	seen := make(map[string]bool)
	for _, hit := range body.Results {
		seen[hit.Issue.UID] = true
		switch hit.Issue.UID {
		case unpulled.UID:
			require.Equal(t, unpulled.UID, hit.Issue.ShortID)
			require.Equal(t, "https://tasks.example/kata?issue="+unpulled.UID+"#direct=1", hit.WebURL)
		case issue.UID:
			require.Equal(t, "https://spoke.example/kata?issue="+issue.UID+"#direct=1", hit.WebURL)
		}
	}
	require.True(t, seen[issue.UID])
	require.True(t, seen[unpulled.UID])
	resp, raw = envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=concept&mode=semantic&status=closed", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Empty(t, body.Results)
}

func TestDirectSpokeSearchHubWithoutEmbeddings(t *testing.T) {
	ctx := t.Context()
	hub, spoke, project, replica, _, token := createClaimForwardingPair(t, "pull")
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "unpulled zebra", Author: "tester"})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: token}))

	for _, mode := range []string{"", "&mode=auto"} {
		resp, raw := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=zebra%s", replica.ID, mode))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var body struct {
			Mode           string `json:"mode"`
			Degraded       bool   `json:"degraded"`
			DegradedReason string `json:"degraded_reason"`
			Results        []struct {
				Issue     db.Issue `json:"issue"`
				MatchedIn []string `json:"matched_in"`
			} `json:"results"`
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		require.Equal(t, "lexical", body.Mode)
		require.True(t, body.Degraded)
		require.Contains(t, body.DegradedReason, "hub search: semantic search is unavailable; results are keyword-only")
		require.Len(t, body.Results, 1)
		require.Equal(t, issue.UID, body.Results[0].Issue.UID)
		require.Equal(t, []string{"title"}, body.Results[0].MatchedIn)
	}
}

func TestFederationSearchIndexUnavailable(t *testing.T) {
	t.Run("index unavailable", func(t *testing.T) {
		emb, err := embedding.New(embedding.Config{BaseURL: "https://example.test", Model: "example", Dims: 2})
		require.NoError(t, err)
		hub := testenv.New(t, func(cfg *daemon.ServerConfig) {
			cfg.Embedder, cfg.EmbeddingUnavailableReason = emb, "vector index unavailable"
		})
		project, _ := createClaimHubIssue(t, hub)
		spoke := testenv.New(t)
		grant := createClaimEnrollment(t, hub, project.ID, spoke.DB.InstanceUID(), "pull")
		path := fmt.Sprintf("/api/v1/projects/%d/federation/search?q=claim&auto=true", project.ID)
		resp, raw := envDoRaw(t, hub, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + grant.Token})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		require.Contains(t, string(raw), `"mode":"lexical"`)
		require.Contains(t, string(raw), "vector index unavailable")
	})
	t.Run("provider rejects credentials", func(t *testing.T) {
		ctx := t.Context()
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
		t.Cleanup(provider.Close)
		emb, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
		require.NoError(t, err)
		idx, err := vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, idx.Close()) })
		hub := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Embedder, cfg.VectorIndex = emb, idx })
		project, _ := createClaimHubIssue(t, hub)
		_, err = idx.RefreshMirror(ctx, hub.DB, nil)
		require.NoError(t, err)
		key := emb.Generation().Fingerprint()
		require.NoError(t, idx.EnsureBuilding(ctx, key, emb.Generation()))
		_, err = idx.Fill(ctx, key, leaseRecoveryEmbedder{}.EncodeFunc(), 64, nil, nil)
		require.NoError(t, err)
		require.NoError(t, idx.CutOver(ctx, key))
		spoke := testenv.New(t)
		grant := createClaimEnrollment(t, hub, project.ID, spoke.DB.InstanceUID(), "pull")
		path := fmt.Sprintf("/api/v1/projects/%d/federation/search?q=claim", project.ID)
		headers := map[string]string{"Authorization": "Bearer " + grant.Token}
		resp, raw := envDoRaw(t, hub, http.MethodGet, path+"&auto=true", nil, headers)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		require.Contains(t, string(raw), `"mode":"lexical"`)
		require.Contains(t, string(raw), "rejected the API key")
		require.Contains(t, string(raw), "claim target")
		resp, raw = envDoRaw(t, hub, http.MethodGet, path, nil, headers)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	})
}

func TestDirectSpokeSearchLocalWords(t *testing.T) {
	ctx := t.Context()
	spoke, replica, _, _ := newDirectSpokeSearchFixture(t)
	local, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.ID, Title: "pending zebra", Author: "tester"})
	require.NoError(t, err)
	resp, raw := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=zebra", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var body struct {
		Results []struct {
			Issue db.Issue `json:"issue"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	found := false
	for _, hit := range body.Results {
		found = found || hit.Issue.UID == local.UID
	}
	require.True(t, found)
}

func TestDirectSpokeSearchMCPMissingEmbeddings(t *testing.T) {
	ctx := t.Context()
	spoke, _, _, unpulled := newDirectSpokeSearchFixture(t)
	_, err := spoke.DB.CreateProject(ctx, "local-only")
	require.NoError(t, err)
	session := connectSearchMCP(t, daemon.ServerConfig{DB: spoke.DB})
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "concept", "mode": "semantic"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	output := result.StructuredContent.(map[string]any)
	require.Equal(t, true, output["degraded"])
	require.Contains(t, output["degraded_reason"], `local-only: mode "semantic" is unavailable: embeddings are not configured`)
	hits := output["results"].([]any)
	require.Len(t, hits, 2)
	foundRemote := false
	for _, hit := range hits {
		resultIssue := hit.(map[string]any)["issue"].(map[string]any)
		if resultIssue["uid"] == unpulled.UID {
			foundRemote = true
			require.Equal(t, "https://tasks.example/kata?issue="+unpulled.UID+"#direct=1", resultIssue["web_url"])
		}
	}
	require.True(t, foundRemote)
}

func TestDirectSpokeSearchMCPLocalAndHubSemantic(t *testing.T) {
	ctx := t.Context()
	spoke, _, issue, unpulled := newDirectSpokeSearchFixture(t)
	localProject, err := spoke.DB.CreateProject(ctx, "local-only")
	require.NoError(t, err)
	localIssue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localProject.ID, Title: "local concept", Author: "tester"})
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &input))
		require.Equal(t, []string{"concept"}, input.Input)
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []any{map[string]any{"embedding": []float32{1, 0}}}}))
	}))
	t.Cleanup(provider.Close)
	localEmbedder, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example", Dims: 2})
	require.NoError(t, err)
	localIndex, err := vector.Open(ctx, filepath.Join(t.TempDir(), "local-vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, localIndex.Close()) })
	_, err = localIndex.RefreshMirror(ctx, spoke.DB, nil)
	require.NoError(t, err)
	localKey := localEmbedder.Generation().Fingerprint()
	require.NoError(t, localIndex.EnsureBuilding(ctx, localKey, localEmbedder.Generation()))
	var inputs []string
	_, err = localIndex.Fill(ctx, localKey, func(_ context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}, 64, nil, nil)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Contains(t, inputs[0], "local concept")
	require.NoError(t, localIndex.CutOver(ctx, localKey))
	configuredSession := connectSearchMCP(t, daemon.ServerConfig{DB: spoke.DB, Embedder: localEmbedder, VectorIndex: localIndex})
	result, err := configuredSession.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "concept", "mode": "semantic"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	hits := result.StructuredContent.(map[string]any)["results"].([]any)
	require.Len(t, hits, 3)
	foundUIDs := make(map[string]bool)
	for _, hit := range hits {
		foundUIDs[hit.(map[string]any)["issue"].(map[string]any)["uid"].(string)] = true
	}
	require.True(t, foundUIDs[issue.UID])
	require.True(t, foundUIDs[unpulled.UID])
	require.True(t, foundUIDs[localIssue.UID])
}

func TestDirectSpokeSearchReportsUnavailableHub(t *testing.T) {
	hub, spoke, hubProject, replica, _, token := createClaimForwardingPair(t, "pull")
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Actor: "tester", Capabilities: "pull",
	}))
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("auto") == "true" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"hits":[],"bounded":false}`))
	}))
	t.Cleanup(slow.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, slow.URL)
	for _, mode := range []string{"semantic", "hybrid"} {
		resp, body := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim&mode=%s", replica.ID, mode))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	}
	resp, body := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), `"mode":"lexical"`)
	require.Contains(t, string(body), `"degraded":true`)
	require.Contains(t, string(body), "claim target")
	setClaimBindingHubURL(t, spoke, replica.ID, hub.URL)
	resp, body = envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim&mode=semantic", replica.ID))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	require.Contains(t, string(body), `"code":"hub_search_failed"`)
	require.NotContains(t, string(body), "hub unreachable")
	require.Contains(t, string(body), "embeddings are not configured")
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "Rate exceeded.", http.StatusTooManyRequests)
	}))
	t.Cleanup(busy.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, busy.URL)
	resp, body = envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim&mode=semantic", replica.ID))
	assertAPIError(t, resp.StatusCode, body, http.StatusTooManyRequests, "server_busy")
	resp, body = envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), `"degraded":true`)
	require.Contains(t, string(body), "claim target")
	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(stalled.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, stalled.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	path := fmt.Sprintf("%s/api/v1/projects/%d/search?q=claim&mode=semantic", spoke.URL, replica.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	require.NoError(t, err)
	response, err := spoke.HTTP.Do(req)
	require.NoError(t, err)
	body, err = io.ReadAll(response.Body)
	require.NoError(t, errors.Join(err, response.Body.Close()))
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, string(body))
	require.Contains(t, string(body), `"code":"hub_unreachable"`)
	older := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(older.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, older.URL)
	path = fmt.Sprintf("/api/v1/projects/%d/search?q=claim", replica.ID)
	for _, mode := range []string{"semantic", "hybrid"} {
		resp, body = envGetRaw(t, spoke, path+"&mode="+mode)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	}
	resp, body = envGetRaw(t, spoke, path)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), `"mode":"lexical"`)
	require.Contains(t, string(body), `"degraded":true`)
	require.Contains(t, string(body), "claim target")
}

func TestDirectSpokeSearchWaitsForHubFallback(t *testing.T) {
	_, spoke, project, replica, issue, token := createClaimForwardingPair(t, "pull")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3200 * time.Millisecond):
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"hits": []any{map[string]any{"issue": issue, "score": 1, "matched_in": []string{"title"}}}, "mode": "lexical", "degraded_reason": "keyword-only"}))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, server.URL)
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: server.URL, HubProjectID: project.ID, Token: token}))
	resp, raw := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=concept", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), issue.UID)
	require.Contains(t, string(raw), "keyword-only")
}

func TestDirectSpokeSearchRejectsUnknownHubMode(t *testing.T) {
	hub, spoke, project, replica, _, token := createClaimForwardingPair(t, "pull")
	remote, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "unpulled claim", Author: "tester"})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"hits": []any{map[string]any{"issue": remote, "score": 1}}, "mode": "future"}))
	}))
	t.Cleanup(server.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, server.URL)
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: server.URL, HubProjectID: project.ID, Token: token}))
	path := fmt.Sprintf("/api/v1/projects/%d/search?q=claim", replica.ID)
	resp, raw := envGetRaw(t, spoke, path)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"degraded":true`)
	require.Contains(t, string(raw), "unsupported hub search mode")
	require.NotContains(t, string(raw), remote.UID)
	resp, raw = envGetRaw(t, spoke, path+"&mode=semantic")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"code":"hub_search_failed"`)
}

func TestDirectSpokeSearchOlderHubLabelsSemantic(t *testing.T) {
	_, spoke, project, replica, issue, token := createClaimForwardingPair(t, "pull")
	older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "true", r.URL.Query().Get("auto"))
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"hits": []any{map[string]any{"issue": issue, "score": 1}}, "bounded": false}))
	}))
	t.Cleanup(older.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, older.URL)
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: older.URL, HubProjectID: project.ID, Token: token}))
	resp, raw := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=concept", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var body struct {
		Results []struct {
			Issue     db.Issue `json:"issue"`
			MatchedIn []string `json:"matched_in"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Len(t, body.Results, 1)
	require.Equal(t, issue.UID, body.Results[0].Issue.UID)
	require.Equal(t, []string{"semantic"}, body.Results[0].MatchedIn)
}

func TestFederationSearchRequiresPullEnrollment(t *testing.T) {
	hub, spoke, project, _, _, _ := createClaimForwardingPair(t, "pull")
	path := fmt.Sprintf("/api/v1/projects/%d/federation/search?q=claim", project.ID)
	push := createClaimEnrollment(t, hub, project.ID, spoke.DB.InstanceUID(), "push")
	resp, body := envDoRaw(t, hub, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + push.Token})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	parent, _, err := hub.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: "tester", AdminActor: "admin", PlaintextToken: "relay-parent-search-token"})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, hub.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: hub.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	relay, err := hub.DB.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: spoke.DB.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion, Token: "relay-search-token"})
	require.NoError(t, err)
	resp, body = envDoRaw(t, hub, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + relay.Token})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	require.Contains(t, string(body), "relay_protocol_required")
}

func TestDirectSpokeSearchProjectAccessPrecedesHub(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := testfix.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := newProjectAccessFixture(t, store)
	var calls atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(hub.Close)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 1, HubProjectUID: f.private.UID, Actor: "member", Enabled: true})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(f.private.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 1, Token: "pull-test-token"}))
	path := fmt.Sprintf("/api/v1/projects/%d/search?q=claim&mode=semantic", f.private.ID)
	status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	require.Zero(t, calls.Load())
	status, _, body = f.request(t, http.MethodGet, path, "member", nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, status, string(body))
	require.EqualValues(t, 1, calls.Load())
}
