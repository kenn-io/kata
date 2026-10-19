package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
)

// An explicit publish must expose the existing issue's mapping without creating
// another remote object, including on a binding disabled after publication.
func TestIssuePublishMappedReplayAllProviders(t *testing.T) {
	for _, provider := range []string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"} {
		t.Run(provider, func(t *testing.T) {
			h := newGitHubSyncHandlerHarness(t)
			i, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: h.project.ID, Title: "Native issue", Author: "worker"})
			require.NoError(t, err)
			b, err := h.store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: h.project.ID, Provider: provider, SourceKey: provider + ":source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300})
			require.NoError(t, err)
			_, err = h.store.UpsertImportMapping(t.Context(), db.ImportMappingParams{ProjectID: h.project.ID, Source: b.SourceKey, ObjectType: "issue", ExternalID: "native:42", IssueID: &i.ID})
			require.NoError(t, err)
			_, err = h.store.DisableIssueSyncBinding(t.Context(), h.project.ID)
			require.NoError(t, err)
			resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, provider, "publish"), map[string]any{"issue_ref": i.ShortID, "actor": "worker"})
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			var out map[string]any
			require.NoError(t, json.Unmarshal(body, &out))
			require.Equal(t, "already_published", out["result"])
			require.Equal(t, i.UID, out["issue_uid"])
			require.Equal(t, "native:42", out["external_id"])
		})
	}
}
func TestIssuePublishRejectsInvalidRecoveryRequest(t *testing.T) {
	for _, body := range []map[string]any{
		{"issue_ref": "abc4", "actor": "worker", "adopt": "remote", "resolve_not_created": true, "attempt": "attempt", "message": "checked"},
		{"issue_ref": "abc4", "actor": "worker", "resolve_not_created": true},
		{"issue_ref": "abc4", "actor": "worker", "attempt": "attempt"},
		{"issue_ref": "abc4", "actor": "worker", "message": "checked"},
	} {
		h := newGitHubSyncHandlerHarness(t)
		resp, raw := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), body)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	}
}

func TestIssuePublishStatusAndLifecycleExposeOnlyRecoveryIdentity(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	i, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: h.project.ID, Title: "Private title", Body: "Private body", Author: "worker"})
	require.NoError(t, err)
	b, err := h.store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: h.project.ID, Provider: "github", SourceKey: "github:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300})
	require.NoError(t, err)
	s := h.store.(db.IssuePublicationStore)
	now := time.Now().UTC()
	claim, err := s.ClaimIssuePublication(t.Context(), h.project.ID, i.ID, "github", now, now.Add(-time.Hour), false)
	require.NoError(t, err)
	p, err := s.BeginIssuePublication(t.Context(), db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: "01K00000000000000000000001", Actor: "worker", Revision: i.Revision, Payload: []byte(`{"title":"Private title","body":"Private body"}`)}})
	require.NoError(t, err)
	require.NoError(t, s.ReleaseIssuePublication(t.Context(), claim.Guard, false))
	resp, err := http.Get(h.server.URL + issueSyncEndpoint(h.project.ID, "github", "status"))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.NotContains(t, string(raw), "Private")
	require.NotContains(t, string(raw), "payload")
	var out struct {
		Status struct {
			Pending map[string]any `json:"pending_publication"`
		}
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Status.Pending, 4)
	require.Equal(t, p.AttemptUID, out.Status.Pending["attempt_uid"])
	response, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), map[string]any{"issue_ref": i.ShortID, "actor": "worker"})
	require.Equal(t, 409, response.StatusCode, string(body))
	require.Contains(t, string(body), p.AttemptUID)
	require.Contains(t, string(body), h.project.Name+"#"+i.ShortID, "recovery instructions must retain the issue's project when invoked from another workspace")
	require.NotContains(t, string(body), "Private")
	deleted := postWithHeader(t, h.server, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/delete", h.project.ID, i.ShortID), map[string]string{"X-Kata-Confirm": "DELETE " + h.project.Name + "#" + i.ShortID}, map[string]any{"actor": "worker"})
	require.Equal(t, http.StatusConflict, deleted.status, string(deleted.body))
	require.Contains(t, string(deleted.body), "issue_publication_pending")
	require.Contains(t, string(deleted.body), h.project.Name+"#"+i.ShortID, "recovery guidance must retain the pending issue's project scope")
	require.Contains(t, string(deleted.body), p.AttemptUID)
	require.NotContains(t, string(deleted.body), "Private")
	_, err = h.store.DisableIssueSyncBinding(t.Context(), b.ProjectID)
	require.NoError(t, err)
	sub := h.broadcaster.Subscribe(daemon.SubFilter{ProjectID: h.project.ID})
	defer sub.Unsub()
	response, body = postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), map[string]any{"issue_ref": i.ShortID, "actor": "worker", "resolve_not_created": true, "attempt": p.AttemptUID, "message": "Inspected source and found no matching item"})
	require.Equal(t, 200, response.StatusCode, string(body))
	require.Contains(t, string(body), "resolved_not_created")
	select {
	case <-sub.Ch:
	case <-time.After(time.Second):
		t.Fatal("resolution audit event was not broadcast")
	}
	require.NotEmpty(t, h.hooks.snapshot())
}

func TestIssuePublishRequiresAttributedPrincipal(t *testing.T) {
	h := newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{trustedActorHeader: "X-Example-Actor"})
	resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), map[string]any{"issue_ref": "abc4", "actor": "forged"})
	assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "actor_header_required")
}
func TestIssuePublishRejectsProjectMismatch(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	p, err := h.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	i, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Other issue", Author: "worker"})
	require.NoError(t, err)
	resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), map[string]any{"issue_ref": i.UID, "actor": "worker"})
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
}

type publishTestTransportResolver struct{}

func (publishTestTransportResolver) Transport(_ context.Context, _ githubsync.Binding, base http.RoundTripper) (http.RoundTripper, error) {
	return base, nil
}
func TestIssuePublishCreateAndDisabledAdoptionThroughRealAdapter(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprint(adopt), func(t *testing.T) {
			posts := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example-owner/example-repo" {
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": 123, "node_id": "R_example", "full_name": "example-owner/example-repo", "has_issues": true, "archived": false}))
					return
				}
				if r.Method == http.MethodPost {
					posts++
					var payload map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					require.Equal(t, map[string]any{"title": "Native issue", "body": "Exact body"}, payload)
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": 456, "node_id": "I_example", "number": 7, "url": "https://api.github.com/repos/example-owner/example-repo/issues/7", "html_url": "https://github.com/example-owner/example-repo/issues/7", "title": "Native issue", "body": "Exact body", "state": "open", "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-02T00:00:00Z"}))
			}))
			defer remote.Close()
			h := newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{omitFetcher: true, fetcherFactory: func(config.GitHubSyncConfig) githubsync.Fetcher {
				return githubsync.NewHTTPFetcher(githubsync.HTTPFetcherConfig{Client: remote.Client(), RESTBaseURLOverride: remote.URL, CredentialResolver: publishTestTransportResolver{}})
			}})
			raw, err := githubsync.EncodeConfig(githubsync.Config{Host: "github.com", Owner: "example-owner", Repo: "example-repo", RepoID: 123})
			require.NoError(t, err)
			_, err = h.store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: h.project.ID, Provider: "github", SourceKey: "github:R_example", RemoteID: "R_example", DisplayName: "Example source", Config: raw, IntervalSeconds: 300})
			require.NoError(t, err)
			i, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: h.project.ID, Title: "Native issue", Body: "Exact body", Author: "worker"})
			require.NoError(t, err)
			body := map[string]any{"issue_ref": i.ShortID, "actor": "worker"}
			if adopt {
				_, err = h.store.DisableIssueSyncBinding(t.Context(), h.project.ID)
				require.NoError(t, err)
				body["adopt"] = "7"
			}
			resp, result := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), body)
			require.Equal(t, 200, resp.StatusCode, string(result))
			require.Contains(t, string(result), "issue-id:456")
			if adopt {
				require.Zero(t, posts)
			} else {
				require.Equal(t, 1, posts)
			}
			resp, result = postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), body)
			require.Equal(t, 200, resp.StatusCode, string(result))
			require.Contains(t, string(result), "already_published")
		})
	}
}

func TestIssuePublishRejectsSpokeAndArchivedProject(t *testing.T) {
	for _, kind := range []string{"spoke", "archived"} {
		t.Run(kind, func(t *testing.T) {
			h := newGitHubSyncHandlerHarness(t)
			i, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: h.project.ID, Title: "Native issue", Author: "worker"})
			require.NoError(t, err)
			if kind == "spoke" {
				_, err = h.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: h.project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373", HubProjectID: 42, HubProjectUID: h.project.UID, ReplayHorizonEventID: 1, Enabled: true})
			} else {
				_, _, err = h.store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: h.project.ID, Actor: "worker", Force: true})
			}
			require.NoError(t, err)
			resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "publish"), map[string]any{"issue_ref": i.ShortID, "actor": "worker"})
			if kind == "spoke" {
				assertAPIError(t, resp.StatusCode, body, 409, "issue_sync_federation_conflict")
			} else {
				assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "project_not_found")
			}
		})
	}
}

func TestIssuePublishReadOnlyDeniesMutation(t *testing.T) {
	d := openTestDB(t)
	server := daemon.NewServer(daemon.ServerConfig{DB: d.db, InsecureReadonly: true})
	defer func() { _ = server.Close() }()
	req := httptest.NewRequest(http.MethodPost, "http://daemon.example/api/v1/projects/1/issue-sync/github/publish", strings.NewReader(`{"issue_ref":"abc4","actor":"worker"}`))
	req.RemoteAddr = "198.51.100.10:12345"
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.True(t, out.Code == 401 || out.Code == 403, out.Body.String())
}
