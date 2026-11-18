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
	_, err = idx.RefreshMirror(ctx, hub.DB)
	require.NoError(t, err)
	key := emb.Generation().Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, emb.Generation()))
	_, err = idx.Fill(ctx, key, func(_ context.Context, inputs []string) ([][]float32, error) {
		vectors := make([][]float32, len(inputs))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}, 64, nil, nil)
	require.NoError(t, err)
	require.NoError(t, idx.CutOver(ctx, key))
	return hub, idx, project, issue, key
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
	server := startTestServer(t, daemon.ServerConfig{DB: spoke.DB})
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	mcp, err := mcpserver.New(mcpserver.Options{Client: client, Actor: "tester", Version: "test", Scope: mcpserver.NewAllScope(), StartupSections: []string{"issue_discovery"}})
	require.NoError(t, err)
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := mcp.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
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

func TestDirectSpokeSearchUsesHubResults(t *testing.T) {
	ctx := t.Context()
	hub, idx, project, issue, key := newFederationSearchHub(t)
	unpulled, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "unpulled issue", Author: "tester"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, hub.DB)
	require.NoError(t, err)
	_, err = idx.Fill(ctx, key, func(_ context.Context, inputs []string) ([][]float32, error) {
		vectors := make([][]float32, len(inputs))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}, 64, nil, nil)
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
		if hit.Issue.UID == unpulled.UID {
			require.Equal(t, unpulled.UID, hit.Issue.ShortID)
			require.Equal(t, "https://tasks.example/kata?issue="+unpulled.UID+"#direct=1", hit.WebURL)
		} else if hit.Issue.UID == issue.UID {
			require.Equal(t, "https://spoke.example/kata?issue="+issue.UID+"#direct=1", hit.WebURL)
		}
	}
	require.True(t, seen[issue.UID])
	require.True(t, seen[unpulled.UID])
	resp, raw = envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=concept&mode=semantic&status=closed", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Empty(t, body.Results)
	local, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.ID, Title: "pending zebra", Author: "tester"})
	require.NoError(t, err)
	resp, raw = envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=zebra", replica.ID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &body))
	found := false
	for _, hit := range body.Results {
		found = found || hit.Issue.UID == local.UID
	}
	require.True(t, found)
	localProject, err := spoke.DB.CreateProject(ctx, "local-only")
	require.NoError(t, err)
	server := startTestServer(t, daemon.ServerConfig{DB: spoke.DB})
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	mcp, err := mcpserver.New(mcpserver.Options{Client: client, Actor: "tester", Version: "test", Scope: mcpserver.NewAllScope(), StartupSections: []string{"issue_discovery"}})
	require.NoError(t, err)
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := mcp.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
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
	_, err = localIndex.RefreshMirror(ctx, spoke.DB)
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
	configured := startTestServer(t, daemon.ServerConfig{DB: spoke.DB, Embedder: localEmbedder, VectorIndex: localIndex})
	configuredClient, err := kataclient.NewWithHTTPClient(configured.URL, configured.Client())
	require.NoError(t, err)
	configuredMCP, err := mcpserver.New(mcpserver.Options{Client: configuredClient, Actor: "tester", Version: "test", Scope: mcpserver.NewAllScope(), StartupSections: []string{"issue_discovery"}})
	require.NoError(t, err)
	serverTransport, clientTransport = sdkmcp.NewInMemoryTransports()
	configuredServerSession, err := configuredMCP.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = configuredServerSession.Close() })
	configuredSession, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = configuredSession.Close() })
	result, err = configuredSession.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "concept", "mode": "semantic"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	hits = result.StructuredContent.(map[string]any)["results"].([]any)
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
		select {
		case <-time.After(3200 * time.Millisecond):
			_, _ = w.Write([]byte(`{"hits":[],"bounded":false}`))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, slow.URL)
	for _, mode := range []string{"semantic", "hybrid"} {
		resp, body := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim&mode=%s", replica.ID, mode))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	}
	for _, mode := range []string{"", "&mode=auto"} {
		resp, body := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim%s", replica.ID, mode))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Contains(t, string(body), `"mode":"lexical"`)
		require.Contains(t, string(body), `"degraded":true`)
		require.Contains(t, string(body), "claim target")
	}
	setClaimBindingHubURL(t, spoke, replica.ID, hub.URL)
	resp, body := envGetRaw(t, spoke, fmt.Sprintf("/api/v1/projects/%d/search?q=claim&mode=semantic", replica.ID))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	require.Contains(t, string(body), `"code":"hub_search_failed"`)
	require.NotContains(t, string(body), "hub unreachable")
	require.Contains(t, string(body), "embeddings are not configured")
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
	checkSearch := func() {
		path := fmt.Sprintf("/api/v1/projects/%d/search?q=claim", replica.ID)
		for _, mode := range []string{"semantic", "hybrid"} {
			resp, body := envGetRaw(t, spoke, path+"&mode="+mode)
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
		}
		resp, body := envGetRaw(t, spoke, path)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Contains(t, string(body), `"mode":"lexical"`)
		require.Contains(t, string(body), `"degraded":true`)
		require.Contains(t, string(body), "claim target")
	}
	for _, hubURL := range []string{"http://127.0.0.1:1", older.URL} {
		setClaimBindingHubURL(t, spoke, replica.ID, hubURL)
		checkSearch()
	}
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
