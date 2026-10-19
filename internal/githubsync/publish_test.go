package githubsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func openPublishGitHub(t *testing.T, server *httptest.Server) issuesync.PublishSession {
	t.Helper()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := EncodeConfig(statusConfig())
	require.NoError(t, err)
	session, err := NewPublishAdapter(f).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "github", SourceKey: "github:R_example", RemoteID: "R_example", Config: raw})
	require.NoError(t, err)
	return session
}
func publishGitHubRepo() map[string]any {
	return map[string]any{"id": 123, "node_id": "R_example", "full_name": "example-owner/example-repo", "has_issues": true, "archived": false}
}

func TestPublishGitHubTitleBodyAndRecovery(t *testing.T) {
	for _, body := range []string{"", "Markdown **世界**\nsecond line"} {
		t.Run(body, func(t *testing.T) {
			posts, admissions := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertHTTPFetcherHeaders(t, r)
				switch r.URL.Path {
				case "/repos/example-owner/example-repo":
					require.Equal(t, http.MethodGet, r.Method)
					require.NoError(t, json.MarshalWrite(w, publishGitHubRepo()))
				case "/repos/example-owner/example-repo/issues":
					require.Equal(t, http.MethodPost, r.Method)
					posts++
					require.Equal(t, 1, admissions)
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					require.Equal(t, map[string]any{"title": "Native 🐟", "body": body}, payload)
					w.WriteHeader(http.StatusCreated)
					require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
				case "/repos/example-owner/example-repo/issues/7":
					require.Equal(t, http.MethodGet, r.Method)
					require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			session := openPublishGitHub(t, server)
			payload, err := session.PrepareCreate(t.Context(), db.Issue{Title: "Native 🐟", Body: body, Status: "open"})
			require.NoError(t, err)
			remote, err := session.Create(t.Context(), payload, func(context.Context) error { admissions++; return nil })
			require.NoError(t, err)
			require.Equal(t, "issue-id:456", remote.ExternalID)
			requirePublicationLegacyExternalIDs(t, remote, []string{"issue:I_example"})
			require.Equal(t, "7", remote.Locator)
			require.Equal(t, "open", remote.Status)
			require.NotNil(t, remote.SourceUpdatedAt)
			restored, err := session.ReadCreated(t.Context(), remote)
			require.NoError(t, err)
			require.Equal(t, remote.ExternalID, restored.ExternalID)
			requirePublicationLegacyExternalIDs(t, restored, []string{"issue:I_example"})
			adopted, err := session.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: "7"})
			require.NoError(t, err)
			require.Equal(t, remote.ExternalID, adopted.ExternalID)
			requirePublicationLegacyExternalIDs(t, adopted, []string{"issue:I_example"})
			require.Equal(t, 1, posts)
		})
	}
}

func requirePublicationLegacyExternalIDs(t *testing.T, remote db.IssuePublicationRemote, want []string) {
	t.Helper()
	raw, err := json.Marshal(remote)
	require.NoError(t, err)
	var got struct {
		LegacyExternalIDs []string `json:"legacy_external_ids"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, want, got.LegacyExternalIDs)
}

func TestPublishGitHubSourceAndIdentityFences(t *testing.T) {
	for _, scenario := range []string{"repo_changed", "disabled", "archived", "pull_request", "wrong_repository", "wrong_id", "url_input", "admission", "invalid_status"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example-owner/example-repo" {
					repo := publishGitHubRepo()
					switch scenario {
					case "repo_changed":
						repo["node_id"] = "R_other"
					case "disabled":
						repo["has_issues"] = false
					case "archived":
						repo["archived"] = true
					}
					require.NoError(t, json.MarshalWrite(w, repo))
					return
				}
				if r.Method == http.MethodPost {
					posts++
				}
				wire := statusIssueWire("open")
				switch scenario {
				case "pull_request":
					wire["pull_request"] = map[string]any{}
				case "wrong_repository":
					wire["url"] = "https://api.github.com/repos/other/repo/issues/7"
				case "wrong_id":
					wire["id"] = 789
				case "invalid_status":
					delete(wire, "updated_at")
				}
				require.NoError(t, json.MarshalWrite(w, wire))
			}))
			defer server.Close()
			session := openPublishGitHub(t, server)
			payload, err := session.PrepareCreate(t.Context(), db.Issue{Title: "Native", Status: "open"})
			if scenario == "repo_changed" || scenario == "disabled" || scenario == "archived" {
				require.Error(t, err)
				require.Zero(t, posts)
				return
			}
			require.NoError(t, err)
			if scenario == "admission" {
				_, err = session.Create(t.Context(), payload, func(context.Context) error { return errors.New("denied") })
				require.Error(t, err)
				require.Zero(t, posts)
				return
			}
			identity := db.IssuePublicationRemote{ExternalID: "issue-id:456", Locator: "7"}
			if scenario == "url_input" {
				identity = db.IssuePublicationRemote{ExternalID: server.URL + "/untrusted"}
			}
			_, err = session.ReadCreated(t.Context(), identity)
			require.Error(t, err)
			require.Zero(t, posts)
		})
	}
}

func TestPublishGitHubCreateNeverRetries(t *testing.T) {
	for _, scenario := range []string{"drop", "server_error", "malformed", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			targetHits := 0
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { targetHits++ }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					require.NoError(t, json.MarshalWrite(w, publishGitHubRepo()))
					return
				}
				posts++
				switch scenario {
				case "drop":
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				case "server_error":
					w.WriteHeader(503)
				case "malformed":
					w.WriteHeader(201)
					_, err := w.Write([]byte(`{"id":`))
					require.NoError(t, err)
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				}
			}))
			defer server.Close()
			session := openPublishGitHub(t, server)
			payload, err := session.PrepareCreate(t.Context(), db.Issue{Title: "Native", Status: "open"})
			require.NoError(t, err)
			_, err = session.Create(t.Context(), payload, func(context.Context) error { return nil })
			require.Error(t, err)
			require.Equal(t, 1, posts)
			require.Zero(t, targetHits)
		})
	}
}

func TestPublishGitHubValidationBeforeCreate(t *testing.T) {
	for _, scenario := range []string{"empty_title", "long_title", "long_body", "closed", "denied", "extra_fields"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
				}
				if scenario == "denied" {
					w.WriteHeader(403)
					return
				}
				require.NoError(t, json.MarshalWrite(w, publishGitHubRepo()))
			}))
			defer server.Close()
			session := openPublishGitHub(t, server)
			issue := db.Issue{Title: "Native", Status: "open"}
			switch scenario {
			case "empty_title":
				issue.Title = "  "
			case "long_title":
				issue.Title = strings.Repeat("界", 257)
			case "long_body":
				issue.Body = strings.Repeat("界", 65537)
			case "closed":
				issue.Status = "closed"
			}
			_, err := session.PrepareCreate(t.Context(), issue)
			if scenario == "extra_fields" {
				require.NoError(t, err)
				payload := []byte(`{"title":"Native","body":"","assignees":["someone"]}`)
				_, err = session.Create(t.Context(), payload, func(context.Context) error { return nil })
			}
			require.Error(t, err)
			require.Zero(t, posts)
		})
	}
}

func TestPublishGitHubRetainsIdentityWhenObservationIsIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.NoError(t, json.MarshalWrite(w, publishGitHubRepo()))
			return
		}
		wire := statusIssueWire("open")
		delete(wire, "updated_at")
		require.NoError(t, json.MarshalWrite(w, wire))
	}))
	defer server.Close()
	session := openPublishGitHub(t, server)
	payload, err := session.PrepareCreate(t.Context(), db.Issue{Title: "Native", Status: "open"})
	require.NoError(t, err)
	remote, err := session.Create(t.Context(), payload, func(context.Context) error { return nil })
	require.Error(t, err)
	require.Equal(t, "issue-id:456", remote.ExternalID)
	require.Equal(t, "7", remote.Locator)
}
