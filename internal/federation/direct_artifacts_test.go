package federation_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/internal/vector"
)

// retainSpyStore exposes the artifact storage of the wrapped store and records
// every artifact the fetcher asks it to retain.
type retainSpyStore struct {
	db.Storage
	mu       sync.Mutex
	retained []string
	reads    map[string]int
}

func (s *retainSpyStore) count(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reads == nil {
		s.reads = map[string]int{}
	}
	s.reads[name]++
}

func (s *retainSpyStore) ListFederationBindings(ctx context.Context) ([]db.FederationBinding, error) {
	s.count("ListFederationBindings")
	return s.Storage.ListFederationBindings(ctx)
}

func (s *retainSpyStore) ProjectByUID(ctx context.Context, projectUID string) (db.Project, error) {
	s.count("ProjectByUID")
	return s.Storage.ProjectByUID(ctx, projectUID)
}

func (s *retainSpyStore) FederationBindingByProject(ctx context.Context, id int64) (db.FederationBinding, error) {
	s.count("FederationBindingByProject")
	return s.Storage.FederationBindingByProject(ctx, id)
}

func (s *retainSpyStore) RetainEmbeddingArtifact(ctx context.Context, artifact embedding.EmbeddingArtifact) (bool, error) {
	s.mu.Lock()
	s.retained = append(s.retained, artifact.Digest)
	s.mu.Unlock()
	return s.Storage.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
}

func (s *retainSpyStore) StoredEmbeddingArtifact(ctx context.Context, projectUID, digest string) (embedding.EmbeddingArtifact, error) {
	return s.Storage.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, projectUID, digest)
}

func (s *retainSpyStore) EmbeddingArtifactManifests(ctx context.Context, projectUID string, limit int) ([]embedding.ArtifactManifest, error) {
	return s.Storage.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, projectUID, limit)
}

// hubRequest is one artifact request the fake hub received.
type hubRequest struct {
	hubProjectID int64
	issueUIDs    []string
	recipe       string
	authorized   bool
}

// fakeArtifactHub serves the artifact route from a per-request callback.
type fakeArtifactHub struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []hubRequest
	respond  func(hubRequest) (int, any)
}

func newFakeArtifactHub(t *testing.T) *fakeArtifactHub {
	t.Helper()
	hub := &fakeArtifactHub{respond: func(hubRequest) (int, any) {
		return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{}}
	}}
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/api/v1/projects/"
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/federation/embedding-artifacts"), 10, 64)
		if err != nil || !strings.HasSuffix(r.URL.Path, "/federation/embedding-artifacts") {
			http.NotFound(w, r)
			return
		}
		request := hubRequest{hubProjectID: id, issueUIDs: r.URL.Query()["issue_uid"], recipe: r.URL.Query().Get("recipe_fingerprint"), authorized: r.Header.Get("Authorization") == "Bearer hub-token"}
		hub.mu.Lock()
		hub.requests = append(hub.requests, request)
		respond := hub.respond
		hub.mu.Unlock()
		status, body := respond(request)
		if text, ok := body.(string); ok {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(text))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		require.NoError(t, json.MarshalWrite(w, body))
	}))
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *fakeArtifactHub) seen() []hubRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubRequest(nil), h.requests...)
}

// directFetcherEnv is a spoke store with an embedding recipe and helpers that
// bind projects to the fake hub.
type directFetcherEnv struct {
	t           *testing.T
	store       *retainSpyStore
	credentials *fakeCredentialStore
	hub         *fakeArtifactHub
	client      *embedding.Client
	recipe      embedding.ArtifactIdentity
	remote      string
}

func newDirectFetcherEnv(t *testing.T, backend string) *directFetcherEnv {
	t.Helper()
	store := &retainSpyStore{Storage: openRelayMatrixStore(t, backend, false)}
	c, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "example-model", Dims: 2})
	require.NoError(t, err)
	recipe, err := c.ArtifactIdentity("", "", "")
	require.NoError(t, err)
	remote, err := uid.New()
	require.NoError(t, err)
	return &directFetcherEnv{t: t, store: store, credentials: newFakeCredentialStore(), hub: newFakeArtifactHub(t), client: c, recipe: recipe, remote: remote}
}

type directProject struct {
	project      db.Project
	hubProjectID int64
	issue        db.Issue
	want         vector.ArtifactWant
}

type directBindingKind int

const (
	bindDirect directBindingKind = iota
	bindNone
	bindDisabled
	bindRelay
)

// addProject creates a project with one issue, a producer designation and a
// binding of the requested kind.
func (e *directFetcherEnv) addProject(name string, kind directBindingKind, producer string, recipe embedding.ArtifactIdentity) directProject {
	e.t.Helper()
	ctx := e.t.Context()
	project, err := e.store.CreateProject(ctx, name)
	require.NoError(e.t, err)
	issue, _, err := e.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Direct artifact " + name, Body: "body of " + name, Author: "member"})
	require.NoError(e.t, err)
	if producer != "" {
		raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: producer, Recipe: recipe})
		require.NoError(e.t, err)
		_, err = e.store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "system", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
		require.NoError(e.t, err)
	}
	hubProjectID := project.ID + 100
	if kind != bindNone {
		_, err = e.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: e.hub.server.URL, HubProjectID: hubProjectID, HubProjectUID: project.UID, Actor: "member", Enabled: kind != bindDisabled, PushEnabled: true})
		require.NoError(e.t, err)
		require.NoError(e.t, e.credentials.StoreFederationCredential(ctx, project.UID, config.FederationCredential{HubURL: e.hub.server.URL, HubProjectID: hubProjectID, Token: "hub-token", Actor: "member", AllowInsecure: true}))
	}
	if kind == bindRelay {
		public, _, err := ed25519.GenerateKey(nil)
		require.NoError(e.t, err)
		rootUID, err := uid.New()
		require.NoError(e.t, err)
		require.NoError(e.t, e.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		bindingUID, err := uid.New()
		require.NoError(e.t, err)
		_, err = e.store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, e.store.InstanceUID()}, LocalActor: "member", ResetEpoch: 1})
		require.NoError(e.t, err)
	}
	return directProject{project: project, hubProjectID: hubProjectID, issue: issue, want: vector.ArtifactWant{IssueUID: issue.UID, ProjectUID: project.UID, InputHash: embedding.ArtifactInputHash(embedding.EmbedText(issue.Title, issue.Body))}}
}

// artifactFor builds the hub's artifact for one project's issue over text.
func (e *directFetcherEnv) artifactFor(p directProject, title, body string) embedding.EmbeddingArtifact {
	e.t.Helper()
	identity, err := e.client.ArtifactIdentity(p.project.UID, p.issue.UID, e.remote)
	require.NoError(e.t, err)
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(title, body), [][]float32{{1, 0}})
	require.NoError(e.t, err)
	return artifact
}

func (e *directFetcherEnv) currentArtifact(p directProject) embedding.EmbeddingArtifact {
	return e.artifactFor(p, p.issue.Title, p.issue.Body)
}

func (e *directFetcherEnv) fetcher() *federation.DirectArtifactFetcher {
	return federation.NewDirectArtifactFetcher(e.store, e.credentials, client.Opts{Timeout: 5 * time.Second}, 30*time.Second)
}

func (e *directFetcherEnv) stored(p directProject, artifact embedding.EmbeddingArtifact) error {
	_, err := e.store.StoredEmbeddingArtifact(e.t.Context(), p.project.UID, artifact.Digest)
	return err
}

func forEachDirectBackend(t *testing.T, run func(t *testing.T, backend string)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) { run(t, backend) })
	}
}

func TestDirectFetcherOnlyRemoteMatchingProducers(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		other, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "other-model", Dims: 2})
		require.NoError(t, err)
		otherRecipe, err := other.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		unbound := env.addProject("unbound-project", bindNone, env.remote, env.recipe)
		relay := env.addProject("relay-project", bindRelay, env.remote, env.recipe)
		disabled := env.addProject("disabled-project", bindDisabled, env.remote, env.recipe)
		self := env.addProject("self-project", bindDirect, env.store.InstanceUID(), env.recipe)
		differs := env.addProject("other-recipe-project", bindDirect, env.remote, otherRecipe)
		absent := env.addProject("no-producer-project", bindDirect, "", env.recipe)
		match := env.addProject("matching-project", bindDirect, env.remote, env.recipe)
		wants := []vector.ArtifactWant{unbound.want, relay.want, disabled.want, self.want, differs.want, absent.want, match.want}

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, wants)

		require.Equal(t, map[string]error{match.project.UID: nil}, outcomes)
		requests := env.hub.seen()
		require.Len(t, requests, 1)
		require.Equal(t, match.hubProjectID, requests[0].hubProjectID)
		require.Equal(t, []string{match.issue.UID}, requests[0].issueUIDs)
		require.Equal(t, env.recipe.RecipeFingerprint, requests[0].recipe)
		require.True(t, requests[0].authorized)
	})
}

func TestDirectFetcherRetainsCurrentArtifactAndDropsStaleInputHash(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		fresh := env.addProject("fresh-project", bindDirect, env.remote, env.recipe)
		stale := env.addProject("stale-project", bindDirect, env.remote, env.recipe)
		freshArtifact := env.currentArtifact(fresh)
		staleArtifact := env.artifactFor(stale, stale.issue.Title, "edited on the hub after the want")
		env.hub.respond = func(r hubRequest) (int, any) {
			if r.hubProjectID == fresh.hubProjectID {
				return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{freshArtifact}}
			}
			return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{staleArtifact}}
		}

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, []vector.ArtifactWant{fresh.want, stale.want})

		require.Equal(t, map[string]error{fresh.project.UID: nil, stale.project.UID: nil}, outcomes)
		require.NoError(t, env.stored(fresh, freshArtifact))
		require.ErrorIs(t, env.stored(stale, staleArtifact), db.ErrNotFound)
		require.Equal(t, []string{freshArtifact.Digest}, env.store.retained, "a stale artifact must never reach retention")
	})
}

func TestDirectFetcherDropsArtifactForUnrequestedIssueOrProject(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		asked := env.addProject("asked-project", bindDirect, env.remote, env.recipe)
		neighbor := env.addProject("neighbor-project", bindDirect, env.remote, env.recipe)
		foreignIdentity, err := env.client.ArtifactIdentity(neighbor.project.UID, asked.issue.UID, env.remote)
		require.NoError(t, err)
		foreignProjectArtifact, err := embedding.NewArtifact(foreignIdentity, embedding.EmbedText(asked.issue.Title, asked.issue.Body), [][]float32{{1, 0}})
		require.NoError(t, err)
		env.hub.respond = func(hubRequest) (int, any) {
			return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{foreignProjectArtifact}}
		}

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, []vector.ArtifactWant{asked.want})

		require.Equal(t, map[string]error{asked.project.UID: nil}, outcomes)
		require.Empty(t, env.store.retained, "a requested issue from a foreign project must never reach retention")
	})
}

func TestDirectFetcherThrottlesPerProject(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		p := env.addProject("throttled-project", bindDirect, env.remote, env.recipe)
		fetcher := env.fetcher()
		now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
		fetcher.SetClockForTest(func() time.Time { return now })
		wants := []vector.ArtifactWant{p.want}

		require.Equal(t, map[string]error{p.project.UID: nil}, fetcher.Fetch(t.Context(), env.recipe, wants))
		require.Len(t, env.hub.seen(), 1)

		now = now.Add(29 * time.Second)
		require.Empty(t, fetcher.Fetch(t.Context(), env.recipe, wants), "a throttled project is absent from the outcomes")
		require.Len(t, env.hub.seen(), 1)

		now = now.Add(time.Second)
		require.Equal(t, map[string]error{p.project.UID: nil}, fetcher.Fetch(t.Context(), env.recipe, wants))
		require.Len(t, env.hub.seen(), 2)
	})
}

func TestDirectFetcherThrottlesAFailedAttemptToo(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		p := env.addProject("failing-project", bindDirect, env.remote, env.recipe)
		env.hub.respond = func(hubRequest) (int, any) { return http.StatusInternalServerError, "boom" }
		fetcher := env.fetcher()
		fetcher.SetClockForTest(func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) })

		first := fetcher.Fetch(t.Context(), env.recipe, []vector.ArtifactWant{p.want})
		require.Error(t, first[p.project.UID])
		require.Empty(t, fetcher.Fetch(t.Context(), env.recipe, []vector.ArtifactWant{p.want}))
		require.Len(t, env.hub.seen(), 1)
	})
}

func TestDirectFetcherReportsHubStatus(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		older := env.addProject("older-hub-project", bindDirect, env.remote, env.recipe)
		revoked := env.addProject("revoked-project", bindDirect, env.remote, env.recipe)
		env.hub.respond = func(r hubRequest) (int, any) {
			if r.hubProjectID == older.hubProjectID {
				return http.StatusNotFound, "404 page not found"
			}
			return http.StatusForbidden, `{"status":403,"error":{"code":"auth_invalid","message":"project archived"}}`
		}

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, []vector.ArtifactWant{older.want, revoked.want})

		require.Len(t, outcomes, 2)
		require.ErrorContains(t, outcomes[older.project.UID], "404")
		require.ErrorContains(t, outcomes[revoked.project.UID], "403")
		require.ErrorContains(t, outcomes[revoked.project.UID], "auth_invalid")
		for _, err := range outcomes {
			require.NotContains(t, err.Error(), env.hub.server.URL)
			require.NotContains(t, err.Error(), "hub-token")
		}
	})
}

func TestDirectFetcherNeverSendsForeignOrigin(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		foreign := newFakeArtifactHub(t)
		mismatched := env.addProject("misrouted-project", bindDirect, env.remote, env.recipe)
		wrongActor := env.addProject("wrong-actor-project", bindDirect, env.remote, env.recipe)
		wrongProject := env.addProject("wrong-hub-project", bindDirect, env.remote, env.recipe)
		pending := env.addProject("leave-pending-project", bindDirect, env.remote, env.recipe)
		enrolling := env.addProject("enrolling-project", bindDirect, env.remote, env.recipe)
		missing := env.addProject("no-credential-project", bindDirect, env.remote, env.recipe)
		ctx := t.Context()
		credential := func(p directProject) config.FederationCredential {
			found, ok, err := env.credentials.FederationCredential(ctx, p.project.UID)
			require.NoError(t, err)
			require.True(t, ok)
			return found
		}
		store := func(p directProject, c config.FederationCredential) {
			require.NoError(t, env.credentials.StoreFederationCredential(ctx, p.project.UID, c))
		}
		c := credential(mismatched)
		c.HubURL = foreign.server.URL
		store(mismatched, c)
		c = credential(wrongActor)
		c.Actor = "someone-else"
		store(wrongActor, c)
		c = credential(wrongProject)
		c.HubProjectID++
		store(wrongProject, c)
		c = credential(pending)
		c.LeavePending = true
		store(pending, c)
		c = credential(enrolling)
		c.RelayEnrollmentPending = true
		store(enrolling, c)
		require.NoError(t, env.credentials.DeleteFederationCredential(ctx, missing.project.UID))

		outcomes := env.fetcher().Fetch(ctx, env.recipe, []vector.ArtifactWant{mismatched.want, wrongActor.want, wrongProject.want, pending.want, enrolling.want, missing.want})

		require.Len(t, outcomes, 3)
		require.Error(t, outcomes[mismatched.project.UID])
		require.Error(t, outcomes[wrongActor.project.UID])
		require.Error(t, outcomes[wrongProject.project.UID])
		for _, err := range outcomes {
			require.NotContains(t, err.Error(), foreign.server.URL)
			require.NotContains(t, err.Error(), env.hub.server.URL)
		}
		require.Empty(t, env.hub.seen())
		require.Empty(t, foreign.seen())
	})
}

func TestDirectFetcherNoDirectBindingsNoWork(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		unbound := env.addProject("unbound-project", bindNone, env.remote, env.recipe)
		relay := env.addProject("relay-project", bindRelay, env.remote, env.recipe)
		disabled := env.addProject("disabled-project", bindDisabled, env.remote, env.recipe)
		env.store.reads = nil

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, []vector.ArtifactWant{unbound.want, relay.want, disabled.want})

		require.Empty(t, outcomes)
		require.Equal(t, map[string]int{"ListFederationBindings": 1}, env.store.reads)
		require.Empty(t, env.hub.seen())
	})
}

func TestDirectFetcherCapsBatchAt32(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		p := env.addProject("busy-project", bindDirect, env.remote, env.recipe)
		wants := make([]vector.ArtifactWant, 0, 40)
		for range 40 {
			id, err := uid.New()
			require.NoError(t, err)
			wants = append(wants, vector.ArtifactWant{IssueUID: id, ProjectUID: p.project.UID, InputHash: embedding.ArtifactInputHash(id)})
		}

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, wants)

		require.Equal(t, map[string]error{p.project.UID: nil}, outcomes)
		requests := env.hub.seen()
		require.Len(t, requests, 1)
		require.Len(t, requests[0].issueUIDs, 32)
	})
}

// A hub that stops answering must not hold the reconcile pass for every
// project that shares its origin: after one transport failure the rest of that
// origin is skipped, unattempted and unthrottled.
func TestDirectFetcherSkipsOriginAfterTransportFailure(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		first := env.addProject("first-project", bindDirect, env.remote, env.recipe)
		second := env.addProject("second-project", bindDirect, env.remote, env.recipe)
		release := make(chan struct{})
		env.hub.respond = func(hubRequest) (int, any) {
			<-release
			return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{}}
		}
		t.Cleanup(func() { close(release) }) // runs before the hub server closes
		fetcher := env.fetcher()
		fetcher.SetRequestTimeoutForTest(50 * time.Millisecond)
		fetcher.SetClockForTest(func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) })
		wants := []vector.ArtifactWant{first.want, second.want}

		outcomes := fetcher.Fetch(t.Context(), env.recipe, wants)

		require.Len(t, env.hub.seen(), 1, "the second project on the same origin is not requested")
		require.Len(t, outcomes, 1)
		require.Error(t, outcomes[first.project.UID])
		require.NotContains(t, outcomes, second.project.UID)

		// The skipped project was never attempted, so it is not throttled; the
		// first project was attempted and is.
		env.hub.mu.Lock()
		env.hub.respond = func(hubRequest) (int, any) {
			return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{}}
		}
		env.hub.mu.Unlock()
		outcomes = fetcher.Fetch(t.Context(), env.recipe, wants)
		require.Equal(t, map[string]error{second.project.UID: nil}, outcomes)
		require.Len(t, env.hub.seen(), 2)
	})
}

// A hub that answers with an error status is reachable, so the other projects
// on its origin are still asked.
func TestDirectFetcherKeepsOriginAfterHubStatusError(t *testing.T) {
	forEachDirectBackend(t, func(t *testing.T, backend string) {
		env := newDirectFetcherEnv(t, backend)
		first := env.addProject("first-project", bindDirect, env.remote, env.recipe)
		second := env.addProject("second-project", bindDirect, env.remote, env.recipe)
		env.hub.respond = func(request hubRequest) (int, any) {
			if request.hubProjectID == first.hubProjectID {
				return http.StatusInternalServerError, "boom"
			}
			return http.StatusOK, api.FederationEmbeddingArtifactsBody{Artifacts: []embedding.EmbeddingArtifact{}}
		}

		outcomes := env.fetcher().Fetch(t.Context(), env.recipe, []vector.ArtifactWant{first.want, second.want})

		require.Len(t, env.hub.seen(), 2)
		require.Error(t, outcomes[first.project.UID])
		require.NoError(t, outcomes[second.project.UID])
		require.Contains(t, outcomes, second.project.UID)
	})
}
