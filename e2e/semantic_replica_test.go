//go:build !windows

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// replicaHub is an in-process hub daemon with semantic search configured
// against its own fixture embedder.
type replicaHub struct {
	env      *testenv.Env
	embedder *fixtureEmbedder
	emb      *embedding.Client
	idx      *vector.Index
}

func newReplicaHub(t *testing.T) *replicaHub {
	t.Helper()
	h := &replicaHub{embedder: newFixtureEmbedder(t)}
	var err error
	h.emb, err = embedding.New(embedding.Config{BaseURL: h.embedder.URL(), Model: fixtureModelV1, Dims: 2})
	require.NoError(t, err)
	h.idx, err = vector.Open(context.Background(), filepath.Join(t.TempDir(), "hub.vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.idx.Close() })
	h.env = testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Embedder = h.emb
		cfg.VectorIndex = h.idx
	})
	return h
}

// embed runs the hub's reconcile steps once: mirror, register, fill, activate.
func (h *replicaHub) embed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := h.idx.RefreshMirror(ctx, h.env.DB)
	require.NoError(t, err)
	gen := h.emb.Generation()
	key := gen.Fingerprint()
	require.NoError(t, h.idx.EnsureBuilding(ctx, key, gen))
	_, err = h.idx.Fill(ctx, key, h.emb.EncodeFunc(), 64, nil, nil)
	require.NoError(t, err)
	require.NoError(t, h.idx.CutOver(ctx, key))
}

type uidSearchResult struct {
	Mode    string `json:"mode"`
	Results []struct {
		Issue struct {
			UID string `json:"uid"`
		} `json:"issue"`
	} `json:"results"`
}

func semanticTopUID(t *testing.T, client *http.Client, baseURL string, projectID int64, query string) (string, bool) {
	t.Helper()
	status, body := searchStatus(t, client, baseURL, strconv.FormatInt(projectID, 10), query, "semantic")
	if status != http.StatusOK {
		return "", false
	}
	var res uidSearchResult
	require.NoError(t, json.Unmarshal(body, &res))
	if len(res.Results) == 0 {
		return "", false
	}
	return res.Results[0].Issue.UID, true
}

type replicaSpoke struct {
	dirs      e2eDirs
	embedder  *fixtureEmbedder
	url       string
	http      *http.Client
	db        *sqlitestore.Store
	stderr    *safeBuffer
	projectID int64
}

// startReplicaSpoke boots a spoke daemon subprocess with the default
// [search.embeddings] config (same model as the hub, its own fixture
// endpoint) and joins it to hubProject through hubURL.
func startReplicaSpoke(t *testing.T, hub *replicaHub, hubURL string, hubProject db.Project) *replicaSpoke {
	t.Helper()
	ctx := context.Background()
	s := &replicaSpoke{dirs: newE2EDirs(t), embedder: newFixtureEmbedder(t)}
	writeEmbeddingsConfig(t, s.dirs.home, s.embedder.URL(), fixtureModelV1)
	bin := buildKataBinary(t)
	s.stderr = startDaemon(t, bin, append(s.dirs.env(), "KATA_FEDERATION_PULL_INTERVAL_MS=25"))
	s.url, s.http = connectDaemon(t, s.dirs, s.stderr)
	var err error
	s.db, err = sqlitestore.Open(ctx, s.dirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.db.Close() })

	var meta api.ProjectFederationBody
	decodePOST(t, hub.env.HTTP, hub.env.URL+"/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &meta)
	created, err := hub.env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "replica-pull-token-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		SpokeInstanceUID: s.db.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull",
		Actor:            "agent",
	})
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	decodePOST(t, s.http, s.url+"/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hubURL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"token":                   created.Token,
		"actor":                   "agent",
	}, &replica)
	s.projectID = replica.Project.ID
	return s
}

func (s *replicaSpoke) embeddingsHealth(t *testing.T) api.EmbeddingsHealth {
	t.Helper()
	var body struct {
		Embeddings *api.EmbeddingsHealth `json:"embeddings"`
	}
	require.NoError(t, json.Unmarshal([]byte(getBody(t, s.http, s.url+"/api/v1/health")), &body))
	require.NotNil(t, body.Embeddings, "spoke health must report embeddings")
	return *body.Embeddings
}

func (s *replicaSpoke) waitForSemanticTop(t *testing.T, query, wantUID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if uid, ok := semanticTopUID(t, s.http, s.url, s.projectID, query); ok && uid == wantUID {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("spoke semantic search for %q never ranked %s first\nhealth: %+v\ndaemon stderr: %s",
		query, wantUID, s.embeddingsHealth(t), s.stderr.String())
}

func seedReplicaHubProject(t *testing.T, hub *replicaHub) (db.Project, db.Issue) {
	t.Helper()
	ctx := context.Background()
	project, err := hub.env.DB.CreateProject(ctx, "replica-hub")
	require.NoError(t, err)
	target, _, err := hub.env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "OAuth callback loops back to the login page",
		Body: "After the redirect the session cookie is missing.", Author: "agent",
	})
	require.NoError(t, err)
	_, _, err = hub.env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Dashboard colors are too pale",
		Body: "Increase contrast on the chart palette.", Author: "agent",
	})
	require.NoError(t, err)
	return project, target
}

const replicaParaphrase = "sign-in keeps bouncing me"

// TestE2E_SemanticReplica_ImportsHubVectors is acceptance criterion 1 with
// the default configuration: a spoke with its own embedding config imports
// every federated row from the hub, sends only query text to its provider,
// and ranks the same top hit as the hub.
func TestE2E_SemanticReplica_ImportsHubVectors(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tests are slow")
	}
	hub := newReplicaHub(t)
	project, target := seedReplicaHubProject(t, hub)
	hub.embed(t)

	spoke := startReplicaSpoke(t, hub, hub.env.URL, project)
	waitForFederatedIssue(t, spoke.db, target.UID, spoke.stderr)
	spoke.waitForSemanticTop(t, replicaParaphrase, target.UID)

	hubTop, ok := semanticTopUID(t, hub.env.HTTP, hub.env.URL, project.ID, replicaParaphrase)
	require.True(t, ok, "hub semantic search must answer")
	assert.Equal(t, target.UID, hubTop, "hub and spoke rank the same top hit")

	for _, input := range spoke.embedder.Inputs() {
		assert.Equal(t, embedding.EmbedText(replicaParaphrase, ""), input,
			"the spoke provider may only see query text, got a document input")
	}
	h := spoke.embeddingsHealth(t)
	assert.Equal(t, "replica", h.Source)
	assert.Equal(t, daemon.ReplicaStatusOK, h.SourceStatus)
	assert.Equal(t, int64(2), h.Replicated)
	assert.Zero(t, h.Backlog)
}

// TestE2E_SemanticReplica_OlderHubFallsBackToLocalEmbedding is acceptance
// criterion 4: a hub without vectors:lookup (simulated by a proxy answering
// 404 for that route) leaves the spoke embedding locally, as before.
func TestE2E_SemanticReplica_OlderHubFallsBackToLocalEmbedding(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tests are slow")
	}
	hub := newReplicaHub(t)
	project, target := seedReplicaHubProject(t, hub)
	hub.embed(t)
	upstream, err := url.Parse(hub.env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	olderHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/federation/vectors:lookup") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(olderHub.Close)

	spoke := startReplicaSpoke(t, hub, olderHub.URL, project)
	waitForFederatedIssue(t, spoke.db, target.UID, spoke.stderr)
	spoke.waitForSemanticTop(t, replicaParaphrase, target.UID)

	h := spoke.embeddingsHealth(t)
	assert.Equal(t, daemon.ReplicaStatusUnsupported, h.SourceStatus)
	assert.Zero(t, h.Replicated)
	docInputs := 0
	for _, input := range spoke.embedder.Inputs() {
		if input != embedding.EmbedText(replicaParaphrase, "") {
			docInputs++
		}
	}
	assert.Equal(t, 2, docInputs, "an older hub must leave the spoke embedding both rows locally")
}
