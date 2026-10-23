package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
	kitvec "go.kenn.io/kit/vector"
	"hegel.dev/go/hegel"
)

const (
	routeFingerprintA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	routeFingerprintB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	routeFingerprintC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type artifactRouteHub struct {
	store    db.Storage
	exporter *artifactRouteStore
	url      string
	http     *http.Client
}

type artifactRouteStore struct {
	db.Storage
	exportOverride func(context.Context, db.ExportFilter, ...string) iter.Seq2[db.ImportRecord, error]
}

func (s *artifactRouteStore) ExportEmbeddingArtifacts(ctx context.Context, filter db.ExportFilter, issueUIDs ...string) iter.Seq2[db.ImportRecord, error] {
	if s.exportOverride != nil {
		return s.exportOverride(ctx, filter, issueUIDs...)
	}
	return s.Storage.(db.EmbeddingArtifactExporter).ExportEmbeddingArtifacts(ctx, filter, issueUIDs...)
}

// artifactRouteBackends serves a real daemon over each storage backend so the
// route is exercised through HTTP on SQLite and PostgreSQL.
func artifactRouteBackends(t *testing.T, check func(*testing.T, *artifactRouteHub)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			if backend == "sqlite" {
				native, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "hub.db"))
				require.NoError(t, err)
				store = native
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, t.Context())
				t.Cleanup(cleanup)
				native, err := pgstore.Open(t.Context(), dsn)
				require.NoError(t, err)
				store = native
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			exporter := &artifactRouteStore{Storage: store}
			server := daemon.NewServer(daemon.ServerConfig{DB: exporter, StartedAt: time.Now().UTC()})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			check(t, &artifactRouteHub{store: store, exporter: exporter, url: httpServer.URL, http: httpServer.Client()})
		})
	}
}

func (h *artifactRouteHub) hubProject(t testing.TB, name string) db.Project {
	t.Helper()
	project, err := h.store.CreateProject(t.Context(), name)
	require.NoError(t, err)
	_, err = h.store.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	return project
}

func (h *artifactRouteHub) enroll(t testing.TB, project db.Project, capabilities string) string {
	t.Helper()
	token, err := uid.New()
	require.NoError(t, err)
	created, err := h.store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token: "route-" + token, SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: capabilities, Actor: "tester",
	})
	require.NoError(t, err)
	return created.Token
}

func (h *artifactRouteHub) issue(t testing.TB, project db.Project, title string) db.Issue {
	t.Helper()
	issue, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: title, Body: "body of " + title, Author: "tester"})
	require.NoError(t, err)
	return issue
}

// retain stores one artifact for issueUID. A UID the hub does not hold stays
// staged; an existing issue makes it durable.
func (h *artifactRouteHub) retain(t testing.TB, project db.Project, issueUID, input, fingerprint string, vectors [][]float32, wantDurable bool) embedding.EmbeddingArtifact {
	t.Helper()
	return h.retainSplit(t, project, issueUID, input, fingerprint, vectors, 2000, wantDurable)
}

func (h *artifactRouteHub) retainSplit(t testing.TB, project db.Project, issueUID, input, fingerprint string, vectors [][]float32, splitMaxRunes int, wantDurable bool) embedding.EmbeddingArtifact {
	t.Helper()
	identity := embedding.ArtifactIdentity{
		ProjectUID: project.UID, IssueUID: issueUID, ProducerInstanceUID: h.store.InstanceUID(),
		Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1",
		Dimensions: len(vectors[0]), InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2",
		RecipeVersion: 2, SplitMaxRunes: splitMaxRunes, SplitOverlap: 0, RecipeFingerprint: fingerprint,
	}
	artifact, err := embedding.NewArtifact(identity, input, vectors)
	require.NoError(t, err)
	durable, err := h.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
	require.NoError(t, err)
	require.Equal(t, wantDurable, durable)
	return artifact
}

func (h *artifactRouteHub) get(t testing.TB, projectID int64, token string, fingerprint string, issueUIDs ...string) (int, []byte) {
	t.Helper()
	query := url.Values{}
	for _, issueUID := range issueUIDs {
		query.Add("issue_uid", issueUID)
	}
	query.Set("recipe_fingerprint", fingerprint)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/federation/embedding-artifacts?%s", h.url, projectID, query.Encode()), nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Do(req) //nolint:gosec // test request to loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func (h *artifactRouteHub) artifacts(t testing.TB, projectID int64, token, fingerprint string, issueUIDs ...string) []embedding.EmbeddingArtifact {
	t.Helper()
	status, raw := h.get(t, projectID, token, fingerprint, issueUIDs...)
	require.Equal(t, http.StatusOK, status, "%.400s", raw)
	var body struct {
		Artifacts []embedding.EmbeddingArtifact `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	return body.Artifacts
}

// retainStale keeps a durable artifact for an input the issue no longer has.
func (h *artifactRouteHub) retainStale(t testing.TB, project db.Project, issueUID, fingerprint string) embedding.EmbeddingArtifact {
	t.Helper()
	input := "stale input for " + issueUID
	identity := embedding.ArtifactIdentity{
		ProjectUID: project.UID, IssueUID: issueUID, ProducerInstanceUID: h.store.InstanceUID(),
		Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1",
		Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2",
		RecipeVersion: 2, SplitMaxRunes: 2000, RecipeFingerprint: fingerprint,
	}
	artifact, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := h.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
	require.NoError(t, err)
	require.True(t, durable)
	return artifact
}

func digests(artifacts []embedding.EmbeddingArtifact) []string {
	out := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		out = append(out, artifact.Digest)
	}
	return out
}

func routeInput(issue db.Issue) string { return embedding.EmbedText(issue.Title, issue.Body) }

func TestEmbeddingArtifactsRouteServesDurableRecipeMatches(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		project := hub.hubProject(t, "hub-project")
		token := hub.enroll(t, project, "pull")
		wanted := hub.issue(t, project, "wanted")
		otherRecipe := hub.issue(t, project, "other recipe")
		unrequested := hub.issue(t, project, "unrequested")
		deleted := hub.issue(t, project, "deleted")
		vectors := [][]float32{{1, 0}}
		want := hub.retain(t, project, wanted.UID, routeInput(wanted), routeFingerprintA, vectors, true)
		hub.retain(t, project, otherRecipe.UID, routeInput(otherRecipe), routeFingerprintB, vectors, true)
		hub.retain(t, project, unrequested.UID, routeInput(unrequested), routeFingerprintA, vectors, true)
		hub.retain(t, project, deleted.UID, routeInput(deleted), routeFingerprintA, vectors, true)
		stagedUID, err := uid.New()
		require.NoError(t, err)
		hub.retain(t, project, stagedUID, "never held by the hub", routeFingerprintA, vectors, false)
		_, _, _, err = hub.store.SoftDeleteIssue(t.Context(), deleted.ID, "tester")
		require.NoError(t, err)

		got := hub.artifacts(t, project.ID, token, routeFingerprintA, wanted.UID, otherRecipe.UID, deleted.UID, stagedUID)
		require.Equal(t, []embedding.EmbeddingArtifact{want}, got)
		require.Empty(t, hub.artifacts(t, project.ID, token, routeFingerprintC, wanted.UID, otherRecipe.UID))
	})
}

// Several durable artifacts can exist for one issue and recipe. Only the one
// for the hub's current content is served.
func TestEmbeddingArtifactsRouteServesOnlyCurrentInputHash(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		project := hub.hubProject(t, "hub-project")
		token := hub.enroll(t, project, "pull")
		issue := hub.issue(t, project, "edited")
		current := hub.retain(t, project, issue.UID, routeInput(issue), routeFingerprintA, [][]float32{{1, 0}}, true)
		stale := hub.retainStale(t, project, issue.UID, routeFingerprintA)
		require.NotEqual(t, current.InputHash, stale.InputHash)
		staleOnly := hub.issue(t, project, "only stale")
		hub.retainStale(t, project, staleOnly.UID, routeFingerprintA)

		got := hub.artifacts(t, project.ID, token, routeFingerprintA, issue.UID, staleOnly.UID)
		require.Equal(t, []embedding.EmbeddingArtifact{current}, got)
	})
}

func TestEmbeddingArtifactsRouteRejectsRelayCredential(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		ctx := t.Context()
		project := hub.hubProject(t, "hub-project")
		issue := hub.issue(t, project, "relay issue")
		parent, _, err := hub.store.CreateAPIToken(ctx, db.CreateAPITokenParams{
			Actor: "member", AdminActor: "admin", PlaintextToken: "relay-parent-test-token", // #nosec G101 -- synthetic parent token used only by this test.
		})
		require.NoError(t, err)
		public, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		require.NoError(t, hub.store.PinRootAuthority(ctx, db.RootKeyPin{
			ProjectUID: project.UID, AuthorityUID: hub.store.InstanceUID(),
			KeyID: db.RootPublicKeyID(public), PublicKey: public,
		}))
		//nolint:gosec // Synthetic relay credential used only by this test.
		relay, err := hub.store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
			ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: federationTestSpokeUID,
			ProtocolVersion: db.RelayProtocolVersion, Token: "negotiated-relay-test-token", ServeDownstream: true,
		})
		require.NoError(t, err)

		status, raw := hub.get(t, project.ID, relay.Token, routeFingerprintA, issue.UID)
		require.Equal(t, http.StatusForbidden, status, "%s", raw)
		require.Contains(t, string(raw), "relay_protocol_required")
	})
}

func TestEmbeddingArtifactsRouteRequiresPull(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		project := hub.hubProject(t, "hub-project")
		issue := hub.issue(t, project, "pull gated")
		hub.retain(t, project, issue.UID, routeInput(issue), routeFingerprintA, [][]float32{{1, 0}}, true)
		pushOnly := hub.enroll(t, project, "push")

		status, raw := hub.get(t, project.ID, pushOnly, routeFingerprintA, issue.UID)
		require.Equal(t, http.StatusForbidden, status, "%s", raw)
		status, raw = hub.get(t, project.ID, "", routeFingerprintA, issue.UID)
		require.Equal(t, http.StatusUnauthorized, status, "%s", raw)
	})
}

func TestEmbeddingArtifactsRouteValidatesIssueCount(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		project := hub.hubProject(t, "hub-project")
		token := hub.enroll(t, project, "pull")
		issueUIDs := make([]string, 33)
		for i := range issueUIDs {
			next, err := uid.New()
			require.NoError(t, err)
			issueUIDs[i] = next
		}
		status, raw := hub.get(t, project.ID, token, routeFingerprintA)
		require.Equal(t, http.StatusBadRequest, status, "%s", raw)
		require.Contains(t, string(raw), "validation")
		status, raw = hub.get(t, project.ID, token, routeFingerprintA, issueUIDs...)
		require.Equal(t, http.StatusBadRequest, status, "%s", raw)
		require.Contains(t, string(raw), "validation")
		status, raw = hub.get(t, project.ID, token, routeFingerprintA, issueUIDs[:32]...)
		require.Equal(t, http.StatusOK, status, "%s", raw)
		status, raw = hub.get(t, project.ID, token, "", issueUIDs[:1]...)
		require.Equal(t, http.StatusBadRequest, status, "%s", raw)
	})
}

// A batch is cut by digest ascending at the byte cap, never emptied by one
// large artifact.
func TestEmbeddingArtifactsRouteTruncatesAtBatchBytes(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		project := hub.hubProject(t, "hub-project")
		token := hub.enroll(t, project, "pull")
		// About 100 maximal-dimension chunks are about 12.5 MiB of vectors each,
		// so five JSON artifacts exceed the 64 MiB batch cap. Each artifact
		// is for its issue's current content, as the route requires.
		const dims = embedding.MaxArtifactDimensions
		const split = 10
		var all []embedding.EmbeddingArtifact
		var issueUIDs []string
		for i := range 5 {
			issue, _, err := hub.store.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: project.ID, Title: fmt.Sprintf("large %d", i), Body: strings.Repeat(fmt.Sprintf("%d", i), 1000), Author: "tester",
			})
			require.NoError(t, err)
			input := routeInput(issue)
			vectors := make([][]float32, len(kitvec.Split(input, kitvec.SplitOptions{MaxRunes: split})))
			for j := range vectors {
				vectors[j] = make([]float32, dims)
			}
			all = append(all, hub.retainSplit(t, project, issue.UID, input, routeFingerprintA, vectors, split, true))
			issueUIDs = append(issueUIDs, issue.UID)
		}
		tailIssue := hub.issue(t, project, "small tail")
		tail := hub.retain(t, project, tailIssue.UID, routeInput(tailIssue), routeFingerprintA, [][]float32{{1, 0}}, true)
		all = append(all, tail)
		issueUIDs = append(issueUIDs, tailIssue.UID)
		want := routeExpectedBatch(t, all)
		require.Less(t, len(want), len(all), "fixture must exceed the batch cap")
		require.NotEmpty(t, want)
		slices.SortFunc(all, func(a, b embedding.EmbeddingArtifact) int { return strings.Compare(a.Digest, b.Digest) })
		readCount := 0
		hub.exporter.exportOverride = func(_ context.Context, _ db.ExportFilter, _ ...string) iter.Seq2[db.ImportRecord, error] {
			return func(yield func(db.ImportRecord, error) bool) {
				for _, artifact := range all {
					readCount++
					if !yield(&db.EmbeddingArtifactExport{EmbeddingArtifact: artifact}, nil) {
						return
					}
				}
			}
		}

		got := hub.artifacts(t, project.ID, token, routeFingerprintA, issueUIDs...)
		require.Equal(t, digests(want), digests(got))
		require.Equal(t, len(want)+1, readCount, "the exporter should stop after yielding the first artifact that exceeds the response cap")
	})
}

// routeExpectedBatch is the documented order and cut: digest ascending, stop
// before the running JSON size exceeds the cap, always keep the first.
func routeExpectedBatch(t testing.TB, artifacts []embedding.EmbeddingArtifact) []embedding.EmbeddingArtifact {
	t.Helper()
	sorted := slices.Clone(artifacts)
	slices.SortFunc(sorted, func(a, b embedding.EmbeddingArtifact) int { return strings.Compare(a.Digest, b.Digest) })
	out := make([]embedding.EmbeddingArtifact, 0, len(sorted))
	total := 0
	for _, artifact := range sorted {
		raw, err := json.Marshal(artifact)
		require.NoError(t, err)
		if len(out) > 0 && total+len(raw) > embedding.MaxArtifactBatchBytes {
			break
		}
		total += len(raw)
		out = append(out, artifact)
	}
	return out
}

// Archiving a hub project revokes its enrollments, so the shared federation
// auth step rejects the request before the handler's own project_not_found
// guard (identical to the legacy event poll).
func TestEmbeddingArtifactsRouteArchivedProject(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		project := hub.hubProject(t, "hub-project")
		token := hub.enroll(t, project, "pull")
		issue := hub.issue(t, project, "soon archived")
		hub.retain(t, project, issue.UID, routeInput(issue), routeFingerprintA, [][]float32{{1, 0}}, true)
		_, _, _, err := hub.store.CloseIssue(t.Context(), issue.ID, "done", "tester", "", nil)
		require.NoError(t, err)
		_, _, err = hub.store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester"})
		require.NoError(t, err)

		status, raw := hub.get(t, project.ID, token, routeFingerprintA, issue.UID)
		require.NotContains(t, string(raw), "artifacts")
		require.Equal(t, http.StatusForbidden, status, "%s", raw)
		require.Contains(t, string(raw), "auth_invalid")
	})
}

func TestEmbeddingArtifactsRouteNoOtherProject(t *testing.T) {
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		authorized := hub.hubProject(t, "hub-project")
		other := hub.hubProject(t, "other-project")
		token := hub.enroll(t, authorized, "pull")
		foreign := hub.issue(t, other, "foreign")
		hub.retain(t, other, foreign.UID, routeInput(foreign), routeFingerprintA, [][]float32{{1, 0}}, true)

		require.Empty(t, hub.artifacts(t, authorized.ID, token, routeFingerprintA, foreign.UID))
		status, _ := hub.get(t, other.ID, token, routeFingerprintA, foreign.UID)
		require.Equal(t, http.StatusForbidden, status)
	})
}

// The response is the durable, current-content, fingerprint-matching artifacts
// of the requested issues of the authorized project, digest ascending, and
// nothing else. Two-dimension vectors keep every batch far below the byte cap,
// so no truncation applies; the fixed truncation test covers the cut.
func TestEmbeddingArtifactsRouteResponseIsAuthorizedSubset(t *testing.T) {
	fingerprints := []string{routeFingerprintA, routeFingerprintB, routeFingerprintC}
	artifactRouteBackends(t, func(t *testing.T, hub *artifactRouteHub) {
		hegel.Test(t, func(ht *hegel.T) {
			authorized := hub.hubProject(ht, "hub-"+routeUnique(ht))
			other := hub.hubProject(ht, "other-"+routeUnique(ht))
			token := hub.enroll(ht, authorized, "pull")

			type candidate struct {
				uid     string
				owner   int64
				deleted bool
				current map[string]embedding.EmbeddingArtifact
			}
			var candidates []*candidate
			for _, project := range []db.Project{authorized, other} {
				for i := range hegel.Draw(ht, hegel.Integers(1, 12)) {
					issue := hub.issue(ht, project, fmt.Sprintf("issue %d %s", i, routeUnique(ht)))
					c := &candidate{uid: issue.UID, owner: project.ID, current: map[string]embedding.EmbeddingArtifact{}}
					for _, fingerprint := range fingerprints {
						// 0 none, 1 current, 2 stale only, 3 current and stale.
						kind := hegel.Draw(ht, hegel.Integers(0, 3))
						if kind == 1 || kind == 3 {
							c.current[fingerprint] = hub.retain(ht, project, issue.UID, routeInput(issue), fingerprint, [][]float32{{1, 0}}, true)
						}
						if kind >= 2 {
							hub.retainStale(ht, project, issue.UID, fingerprint)
						}
					}
					if hegel.Draw(ht, hegel.Booleans()) {
						_, _, _, err := hub.store.SoftDeleteIssue(ht.Context(), issue.ID, "tester")
						require.NoError(ht, err)
						c.deleted = true
					}
					candidates = append(candidates, c)
				}
			}
			for range hegel.Draw(ht, hegel.Integers(0, 8)) {
				phantom, err := uid.New()
				require.NoError(ht, err)
				hub.retain(ht, authorized, phantom, "never held by the hub", hegel.Draw(ht, hegel.SampledFrom(fingerprints)), [][]float32{{1, 0}}, false)
				candidates = append(candidates, &candidate{uid: phantom, owner: authorized.ID, current: map[string]embedding.EmbeddingArtifact{}})
			}

			fingerprint := hegel.Draw(ht, hegel.SampledFrom(fingerprints))
			always := hegel.Draw(ht, hegel.Integers(0, len(candidates)-1))
			var requested []string
			var expected []embedding.EmbeddingArtifact
			for i, c := range candidates {
				if i != always && !hegel.Draw(ht, hegel.Booleans()) {
					continue
				}
				requested = append(requested, c.uid)
				if artifact, ok := c.current[fingerprint]; ok && c.owner == authorized.ID && !c.deleted {
					expected = append(expected, artifact)
				}
			}
			require.LessOrEqual(ht, len(requested), 32)

			got := hub.artifacts(ht, authorized.ID, token, fingerprint, requested...)
			require.True(ht, slices.IsSorted(digests(got)), "response must be digest ascending")
			require.ElementsMatch(ht, expected, got)
		}, hegel.WithTestCases(30))
	})
}

func routeUnique(t testing.TB) string {
	t.Helper()
	next, err := uid.New()
	require.NoError(t, err)
	return strings.ToLower(next)
}
