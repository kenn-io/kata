package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// hubSemantic is an embedding client plus sidecar index wired into a testenv
// hub, filled on demand with the same calls the reconciler makes.
type hubSemantic struct {
	emb *embedding.Client
	idx *vector.Index
}

// newHubSemantic starts a stub /embeddings server that maps every input to
// the unit vector [1, 0] and opens a fresh sidecar.
func newHubSemantic(t *testing.T) *hubSemantic {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	emb, err := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "hub-model", Dims: 2})
	require.NoError(t, err)
	idx, err := vector.Open(context.Background(), filepath.Join(t.TempDir(), "hub.vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	return &hubSemantic{emb: emb, idx: idx}
}

func (h *hubSemantic) option() testenv.Option {
	return func(cfg *daemon.ServerConfig) {
		cfg.Embedder = h.emb
		cfg.VectorIndex = h.idx
	}
}

// fill mirrors store and embeds every pending issue under the hub's
// generation, exactly as one reconciler turn does.
func (h *hubSemantic) fill(t *testing.T, store *sqlitestore.Store) {
	t.Helper()
	ctx := context.Background()
	_, err := h.idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	gen := h.emb.Generation()
	key := gen.Fingerprint()
	require.NoError(t, h.idx.EnsureBuilding(ctx, key, gen))
	_, err = h.idx.Fill(ctx, key, h.emb.EncodeFunc(), 64, nil, nil)
	require.NoError(t, err)
	require.NoError(t, h.idx.CutOver(ctx, key))
}

func vectorLookupPath(projectID int64) string {
	return projectPath(projectID) + "/federation/vectors:lookup"
}

func pullToken(t *testing.T, env *testenv.Env, project db.Project, token string) string {
	t.Helper()
	created, err := env.DB.CreateFederationEnrollment(context.Background(), db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)
	return created.Token
}

func lookupVectors(t *testing.T, env *testenv.Env, projectID int64, token string, body api.FederationVectorLookupRequestBody) api.FederationVectorLookupBody {
	t.Helper()
	resp, raw := envDoRaw(t, env, http.MethodPost, vectorLookupPath(projectID), body, bearer(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "lookup response: %s", raw)
	var out api.FederationVectorLookupBody
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestFederationVectorLookupServesMatchingHashes(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "login loop", Body: "auth callback", Author: "tester"})
	require.NoError(t, err)
	hub.fill(t, env.DB)
	token := pullToken(t, env, project, "vector-lookup-token")
	fingerprint := hub.emb.Generation().Fingerprint()
	hash := vector.ContentSHA256(embedding.EmbedText("login loop", "auth callback"))

	out := lookupVectors(t, env, project.ID, token, api.FederationVectorLookupRequestBody{
		Fingerprint: fingerprint,
		Docs: []api.FederationVectorLookupDoc{
			{IssueUID: issue.UID, ContentSHA256: hash},
		},
	})
	require.NotNil(t, out.Generation)
	assert.Equal(t, fingerprint, out.Generation.Fingerprint)
	assert.Equal(t, "hub-model", out.Generation.Model)
	assert.Equal(t, 2, out.Generation.Dims)
	assert.Equal(t, "2000", out.Generation.Params["chunk_max_runes"])
	assert.Equal(t, "active", out.Generation.State)
	require.Len(t, out.Records, 1)
	rec := out.Records[0]
	assert.Equal(t, api.FederationVectorStatusOK, rec.Status)
	assert.Equal(t, hash, rec.ContentSHA256)
	require.Len(t, rec.Chunks, 1)
	got, err := vector.DecodeVector(rec.Chunks[0].Vector, 2)
	require.NoError(t, err)
	assert.Equal(t, []float32{1, 0}, []float32(got))
}

// An edit makes the hub's stored vector describe old text: the new hash is
// not_ready until the hub re-embeds, then ok. This is the hub half of the
// spec's "local edit" acceptance criterion.
func TestFederationVectorLookupFollowsHubReembedAfterEdit(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "before", Body: "body", Author: "tester"})
	require.NoError(t, err)
	hub.fill(t, env.DB)
	token := pullToken(t, env, project, "vector-edit-token")
	title := "after"
	_, _, _, err = env.DB.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "tester"})
	require.NoError(t, err)
	newHash := vector.ContentSHA256(embedding.EmbedText("after", "body"))
	req := api.FederationVectorLookupRequestBody{
		Fingerprint: hub.emb.Generation().Fingerprint(),
		Docs:        []api.FederationVectorLookupDoc{{IssueUID: issue.UID, ContentSHA256: newHash}},
	}

	before := lookupVectors(t, env, project.ID, token, req)
	require.Len(t, before.Records, 1)
	assert.Equal(t, api.FederationVectorStatusNotReady, before.Records[0].Status)
	assert.Empty(t, before.Records[0].Chunks)

	hub.fill(t, env.DB)
	after := lookupVectors(t, env, project.ID, token, req)
	require.Len(t, after.Records, 1)
	assert.Equal(t, api.FederationVectorStatusOK, after.Records[0].Status)
	assert.Equal(t, newHash, after.Records[0].ContentSHA256)
}

func TestFederationVectorLookupNeverServesAnotherProject(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	other := createFederatedHubProject(t, env, "other")
	foreign, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "secret", Body: "x", Author: "tester"})
	require.NoError(t, err)
	hub.fill(t, env.DB)
	token := pullToken(t, env, project, "vector-scope-token")

	out := lookupVectors(t, env, project.ID, token, api.FederationVectorLookupRequestBody{
		Fingerprint: hub.emb.Generation().Fingerprint(),
		Docs: []api.FederationVectorLookupDoc{{
			IssueUID: foreign.UID, ContentSHA256: vector.ContentSHA256(embedding.EmbedText("secret", "x")),
		}},
	})
	require.Len(t, out.Records, 1)
	assert.Equal(t, api.FederationVectorStatusNotReady, out.Records[0].Status)
	assert.Empty(t, out.Records[0].Chunks)
}

func TestFederationVectorLookupGenerationMismatchReturnsNoRecords(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "t", Body: "b", Author: "tester"})
	require.NoError(t, err)
	hub.fill(t, env.DB)
	token := pullToken(t, env, project, "vector-mismatch-token")

	out := lookupVectors(t, env, project.ID, token, api.FederationVectorLookupRequestBody{
		Fingerprint: "0123456789abcdef",
		Docs: []api.FederationVectorLookupDoc{{
			IssueUID: issue.UID, ContentSHA256: vector.ContentSHA256(embedding.EmbedText("t", "b")),
		}},
	})
	require.NotNil(t, out.Generation)
	assert.Equal(t, hub.emb.Generation().Fingerprint(), out.Generation.Fingerprint)
	assert.Empty(t, out.Records, "vectors from another space must never be sent")
}

func TestFederationVectorLookupWithoutEmbeddingsAdvertisesNoGeneration(t *testing.T) {
	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "hub")
	token := pullToken(t, env, project, "vector-none-token")

	out := lookupVectors(t, env, project.ID, token, api.FederationVectorLookupRequestBody{Fingerprint: "0123456789abcdef"})
	assert.Nil(t, out.Generation)
	assert.Empty(t, out.Records)

	resp, raw := envDoRaw(t, env, http.MethodGet, projectPath(project.ID)+"/federation/metadata", nil, bearer(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "metadata: %s", raw)
	assert.NotContains(t, string(raw), "vector_generation")
}

func TestFederationMetadataAdvertisesVectorGeneration(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	project := createFederatedHubProject(t, env, "hub")
	token := pullToken(t, env, project, "vector-meta-token")

	resp, raw := envDoRaw(t, env, http.MethodGet, projectPath(project.ID)+"/federation/metadata", nil, bearer(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "metadata: %s", raw)
	var out api.ProjectFederationBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.VectorGeneration)
	assert.Equal(t, hub.emb.Generation().Fingerprint(), out.VectorGeneration.Fingerprint)
	assert.Equal(t, "pending", out.VectorGeneration.State, "no reconcile has registered the generation yet")
}

func TestFederationVectorLookupAuthAndValidation(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	pull := pullToken(t, env, project, "vector-pull-token")
	pushOnly, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token: "vector-push-token", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "push", Actor: "tester",
	})
	require.NoError(t, err)
	valid := api.FederationVectorLookupRequestBody{Fingerprint: "0123456789abcdef"}

	resp, raw := envDoRaw(t, env, http.MethodPost, vectorLookupPath(project.ID), valid, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no bearer: %s", raw)
	resp, raw = envDoRaw(t, env, http.MethodPost, vectorLookupPath(project.ID), valid, bearer(pushOnly.Token))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "push-only bearer: %s", raw)

	tooMany := api.FederationVectorLookupRequestBody{Fingerprint: "0123456789abcdef"}
	for i := range 65 {
		tooMany.Docs = append(tooMany.Docs, api.FederationVectorLookupDoc{
			IssueUID: fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAB%05d", i), ContentSHA256: strings.Repeat("a", 64),
		})
	}
	for name, body := range map[string]api.FederationVectorLookupRequestBody{
		"too many docs": tooMany,
		"bad uid": {Fingerprint: "0123456789abcdef", Docs: []api.FederationVectorLookupDoc{
			{IssueUID: "not-a-uid", ContentSHA256: strings.Repeat("a", 64)}}},
		"bad hash": {Fingerprint: "0123456789abcdef", Docs: []api.FederationVectorLookupDoc{
			{IssueUID: "01HZNQ7VFPK1XGD8R5MABCD4EY", ContentSHA256: strings.Repeat("A", 64)}}},
		"duplicate uid": {Fingerprint: "0123456789abcdef", Docs: []api.FederationVectorLookupDoc{
			{IssueUID: "01HZNQ7VFPK1XGD8R5MABCD4EY", ContentSHA256: strings.Repeat("a", 64)},
			{IssueUID: "01HZNQ7VFPK1XGD8R5MABCD4EY", ContentSHA256: strings.Repeat("b", 64)}}},
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := envDoRaw(t, env, http.MethodPost, vectorLookupPath(project.ID), body, bearer(pull))
			assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
		})
	}
}

// Review focus: right after an upgrade the hub's configured generation is
// still building while its old one stays active. Spokes must be served from
// the building generation instead of seeing a mismatch and embedding locally.
func TestFederationVectorLookupServesBuildingGeneration(t *testing.T) {
	hub := newHubSemantic(t)
	env := testenv.New(t, hub.option())
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "t", Body: "b", Author: "tester"})
	require.NoError(t, err)
	// An older generation is active; the configured one is only building.
	old := kitvec.Generation{Model: "old-model", Dimensions: 2}
	require.NoError(t, hub.idx.EnsureBuilding(ctx, old.Fingerprint(), old))
	require.NoError(t, hub.idx.CutOver(ctx, old.Fingerprint()))
	_, err = hub.idx.RefreshMirror(ctx, env.DB)
	require.NoError(t, err)
	gen := hub.emb.Generation()
	require.NoError(t, hub.idx.EnsureBuilding(ctx, gen.Fingerprint(), gen))
	_, err = hub.idx.Fill(ctx, gen.Fingerprint(), hub.emb.EncodeFunc(), 64, nil, nil)
	require.NoError(t, err)
	token := pullToken(t, env, project, "vector-building-token")

	out := lookupVectors(t, env, project.ID, token, api.FederationVectorLookupRequestBody{
		Fingerprint: gen.Fingerprint(),
		Docs: []api.FederationVectorLookupDoc{{
			IssueUID: issue.UID, ContentSHA256: vector.ContentSHA256(embedding.EmbedText("t", "b")),
		}},
	})
	require.NotNil(t, out.Generation)
	assert.Equal(t, "building", out.Generation.State)
	require.Len(t, out.Records, 1)
	assert.Equal(t, api.FederationVectorStatusOK, out.Records[0].Status)
}
