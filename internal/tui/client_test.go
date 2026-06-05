package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testfix"
)

// newTestClient stands up an httptest server with handler, registers
// cleanup, and returns a daemon Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, srv.Client())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(t *testing.T, body any) *http.Response {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(bs))),
	}
}

// respondJSON writes body as a JSON response with the right Content-Type.
func respondJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestTUIFederationClientsKeepAuthRolesSeparate(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	ctx := context.Background()
	var spokeInstanceAuth, spokeStatusAuth, spokeJoinAuth string
	var hubListAuth, hubEnsureAuth, hubEnableAuth, hubEnrollmentAuth, hubMetadataAuth string
	var joinBody CreateFederationReplicaInput
	hubProjectID := int64(42)

	spokeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/instance":
			spokeInstanceAuth = r.Header.Get("Authorization")
			respondJSON(t, w, InstanceInfo{
				InstanceUID:   "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Version:       "dev",
				SchemaVersion: 1,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/federation/status":
			spokeStatusAuth = r.Header.Get("Authorization")
			respondJSON(t, w, api.FederationStatusBody{Statuses: []api.FederationProjectStatus{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/replicas":
			spokeJoinAuth = r.Header.Get("Authorization")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&joinBody))
			respondJSON(t, w, api.CreateFederationReplicaBody{})
		default:
			t.Fatalf("unexpected spoke request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(spokeSrv.Close)

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			hubListAuth = r.Header.Get("Authorization")
			respondJSON(t, w, map[string]any{"projects": []map[string]any{{"id": hubProjectID, "name": "spoke-project"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects":
			hubEnsureAuth = r.Header.Get("Authorization")
			respondJSON(t, w, map[string]any{"project": map[string]any{"id": hubProjectID, "name": "spoke-project"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/42/federation/enable":
			hubEnableAuth = r.Header.Get("Authorization")
			respondJSON(t, w, api.ProjectFederationBody{
				ProjectID:              hubProjectID,
				ProjectUID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
				ProjectName:            "spoke-project",
				ReplayHorizonEventID:   9,
				BaselineThroughEventID: 11,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/enrollments":
			hubEnrollmentAuth = r.Header.Get("Authorization")
			respondJSON(t, w, api.FederationEnrollmentOut{
				ID:               7,
				SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				ProjectID:        &hubProjectID,
				Capabilities:     "claim,pull,push",
				Actor:            "wesm",
				Token:            "enrollment-token",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/42/federation/metadata":
			hubMetadataAuth = r.Header.Get("Authorization")
			respondJSON(t, w, api.ProjectFederationBody{
				ProjectID:              hubProjectID,
				ProjectUID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
				ProjectName:            "spoke-project",
				ReplayHorizonEventID:   9,
				BaselineThroughEventID: 11,
			})
		default:
			t.Fatalf("unexpected hub request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(hubSrv.Close)

	spokeHTTP, err := clientpkg.NewHTTPClientForTarget(ctx, spokeSrv.URL,
		clientpkg.TargetAuth{Token: "spoke-token"}, clientpkg.Opts{})
	require.NoError(t, err)
	spoke := NewClient(spokeSrv.URL, spokeHTTP)
	hubAdmin, _, err := newHubAdminClient(ctx, daemonTarget{
		Name:  "hub",
		URL:   hubSrv.URL,
		Token: "hub-admin-token",
	})
	require.NoError(t, err)

	_, err = spoke.GetInstance(ctx)
	require.NoError(t, err)
	_, err = spoke.FederationStatus(ctx)
	require.NoError(t, err)
	_, err = hubAdmin.ListProjects(ctx)
	require.NoError(t, err)
	_, err = hubAdmin.EnsureProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = hubAdmin.EnableFederation(ctx, hubProjectID, "requested")
	require.NoError(t, err)
	enrollment, err := hubAdmin.CreateFederationEnrollment(ctx, CreateFederationEnrollmentInput{
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		ProjectID:        &hubProjectID,
		Capabilities:     "claim,pull,push",
		Actor:            "requested",
	})
	require.NoError(t, err)
	hubEnrollment, err := newHubEnrollmentClient(ctx, hubSrv.URL, enrollment.Token, false)
	require.NoError(t, err)
	metadata, err := hubEnrollment.ProjectFederation(ctx, hubProjectID)
	require.NoError(t, err)
	_, err = spoke.CreateFederationReplica(ctx, CreateFederationReplicaInput{
		HubURL:                 hubSrv.URL,
		HubProjectID:           hubProjectID,
		HubProjectUID:          metadata.ProjectUID,
		ProjectName:            metadata.ProjectName,
		ReplayHorizonEventID:   metadata.ReplayHorizonEventID,
		BaselineThroughEventID: metadata.BaselineThroughEventID,
		Token:                  enrollment.Token,
		Capabilities:           "claim,pull,push",
		Actor:                  enrollment.Actor,
		PushEnabled:            true,
		AdoptExisting:          true,
	})
	require.NoError(t, err)

	assert.Equal(t, "Bearer spoke-token", spokeInstanceAuth)
	assert.Equal(t, "Bearer spoke-token", spokeStatusAuth)
	assert.Equal(t, "Bearer spoke-token", spokeJoinAuth)
	assert.Equal(t, "Bearer hub-admin-token", hubListAuth)
	assert.Equal(t, "Bearer hub-admin-token", hubEnsureAuth)
	assert.Equal(t, "Bearer hub-admin-token", hubEnableAuth)
	assert.Equal(t, "Bearer hub-admin-token", hubEnrollmentAuth)
	assert.Equal(t, "Bearer enrollment-token", hubMetadataAuth)
	assert.Equal(t, "enrollment-token", joinBody.Token)
	assert.Equal(t, "claim,pull,push", joinBody.Capabilities)
	assert.True(t, joinBody.PushEnabled)
	assert.True(t, joinBody.AdoptExisting)
	for _, got := range []string{
		spokeInstanceAuth, spokeStatusAuth, spokeJoinAuth,
		hubListAuth, hubEnsureAuth, hubEnableAuth, hubEnrollmentAuth, hubMetadataAuth,
	} {
		assert.NotEqual(t, "Bearer global-token", got)
	}
}

func TestClientGetInstanceDecodesAuthPrincipal(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, map[string]any{
			"instance_uid":   "01HZNQ7VFPK1XGD8R5MABCD4EA",
			"version":        "dev",
			"schema_version": 1,
			"auth": map[string]any{
				"kind":  "db_token",
				"actor": "operator",
			},
		})
	})

	instance, err := c.GetInstance(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "db_token", instance.Auth.Kind)
	assert.Equal(t, "operator", instance.Auth.Actor)
}

func TestTUIHubAdminClientRejectsPlainHTTPHostnameWithoutAllowInsecure(t *testing.T) {
	_, _, err := newHubAdminClient(context.Background(), daemonTarget{
		Name:  "hub",
		URL:   "http://hub.internal:7777",
		Token: "hub-admin-token",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_insecure")
}

func TestTUIHubEnrollmentClientCarriesAllowInsecureForPlainHTTPHostname(t *testing.T) {
	_, err := newHubEnrollmentClient(context.Background(), "http://hub.internal:7777", "enrollment-token", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_insecure")

	client, err := newHubEnrollmentClient(context.Background(), "http://hub.internal:7777", "enrollment-token", true)
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestClient_ListIssues_BuildsExpectedURLAndDecodes(t *testing.T) {
	var gotURL string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		respondJSON(t, w, map[string]any{
			"issues": []map[string]any{
				{"short_id": "aaa1", "title": "a", "status": "open"},
				{"short_id": "bbb2", "title": "b", "status": "open"},
			},
		})
	})
	got, err := c.ListIssues(context.Background(), 7, ListFilter{Status: "open"})
	require.NoError(t, err)
	if !strings.Contains(gotURL, "/api/v1/projects/7/issues") {
		t.Fatalf("unexpected URL: %s", gotURL)
	}
	if !strings.Contains(gotURL, "status=open") {
		t.Fatalf("status filter missing: %s", gotURL)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issues, want 2", len(got))
	}
}

func TestClient_ListIssues_SendsLimit(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	})
	_, err := c.ListIssues(context.Background(), 7, ListFilter{Limit: 2001})
	require.NoError(t, err)
	if !strings.Contains(gotQuery, "limit=2001") {
		t.Fatalf("limit not sent: %q", gotQuery)
	}
	if strings.Contains(gotQuery, "status=") {
		t.Fatalf("empty status must not be sent: %q", gotQuery)
	}
}

func TestClient_LocalUnixTransportFailureRetriesWithRefreshedClient(t *testing.T) {
	oldRefresh := refreshLocalHTTPClientForTUI
	t.Cleanup(func() { refreshLocalHTTPClientForTUI = oldRefresh })
	var refreshed atomic.Bool
	refreshLocalHTTPClientForTUI = func(context.Context) (*http.Client, error) {
		refreshed.Store(true)
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(t, map[string]any{
				"issues": []map[string]any{
					{"short_id": "aaa1", "title": "recovered", "status": "open"},
				},
			}), nil
		})}, nil
	}

	c := NewClient(clientpkg.UnixBase, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial unix /tmp/missing.sock: connect: no such file or directory")
		},
	)})

	got, err := c.ListIssues(context.Background(), 7, ListFilter{Limit: 2001})
	require.NoError(t, err)
	require.True(t, refreshed.Load(), "local socket failure should refresh the daemon client")
	require.Len(t, got, 1)
	assert.Equal(t, "aaa1", got[0].ShortID)
}

func TestClient_LocalUnixTransportFailureDoesNotRetryMutationWithoutIdempotencyKey(t *testing.T) {
	oldRefresh := refreshLocalHTTPClientForTUI
	t.Cleanup(func() { refreshLocalHTTPClientForTUI = oldRefresh })
	var refreshed atomic.Bool
	refreshLocalHTTPClientForTUI = func(context.Context) (*http.Client, error) {
		refreshed.Store(true)
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("non-idempotent mutation should not be retried")
			return nil, nil
		})}, nil
	}

	c := NewClient(clientpkg.UnixBase, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("read unix /tmp/kata.sock: connection reset by peer")
		},
	)})

	_, err := c.AddComment(context.Background(), 7, "abc4", "note", "alice")
	require.Error(t, err)
	require.True(t, refreshed.Load(), "non-idempotent mutation should refresh the stale client")
	assert.Contains(t, err.Error(), "local kata daemon connection failed")
}

func TestClient_LocalUnixTransportFailureRetriesMutationWithIdempotencyKey(t *testing.T) {
	oldRefresh := refreshLocalHTTPClientForTUI
	t.Cleanup(func() { refreshLocalHTTPClientForTUI = oldRefresh })
	var refreshed atomic.Bool
	var gotKey string
	refreshLocalHTTPClientForTUI = func(context.Context) (*http.Client, error) {
		refreshed.Store(true)
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotKey = r.Header.Get("Idempotency-Key")
			return jsonResponse(t, map[string]any{
				"issue":   map[string]any{"short_id": "aaa1", "title": "created", "status": "open"},
				"changed": true,
			}), nil
		})}, nil
	}

	c := NewClient(clientpkg.UnixBase, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("read unix /tmp/kata.sock: connection reset by peer")
		},
	)})

	_, err := c.CreateIssue(context.Background(), 7, CreateIssueBody{
		Title: "created", Actor: "alice", IdempotencyKey: "create-issue-key",
	})
	require.NoError(t, err)
	require.True(t, refreshed.Load(), "idempotent mutation should refresh and retry")
	assert.Equal(t, "create-issue-key", gotKey)
}

func TestClient_LocalUnixTransportFailureLogsAndHidesSyntheticHost(t *testing.T) {
	oldRefresh := refreshLocalHTTPClientForTUI
	oldLogPath := tuiClientLogPathForTUI
	t.Cleanup(func() {
		refreshLocalHTTPClientForTUI = oldRefresh
		tuiClientLogPathForTUI = oldLogPath
	})
	logPath := filepath.Join(t.TempDir(), "tui.log")
	tuiClientLogPathForTUI = func() (string, error) { return logPath, nil }
	refreshLocalHTTPClientForTUI = func(context.Context) (*http.Client, error) {
		return nil, errors.New("no unix-socket daemon found")
	}
	c := NewClient(clientpkg.UnixBase, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial unix /tmp/missing.sock: connect: no such file or directory")
		},
	)})

	_, err := c.ListIssues(context.Background(), 7, ListFilter{Limit: 2001})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local kata daemon connection failed")
	assert.NotContains(t, err.Error(), "kata.invalid")

	//nolint:gosec // logPath is a test-controlled file under t.TempDir().
	bs, readErr := os.ReadFile(logPath)
	require.NoError(t, readErr)
	logged := string(bs)
	assert.Contains(t, logged, "GET")
	assert.Contains(t, logged, "/api/v1/projects/7/issues?limit=2001")
	assert.Contains(t, logged, "dial unix /tmp/missing.sock")
	assert.Contains(t, logged, "retry")
}

func TestModel_FetchInitialUsesQueueFetchFilter(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	})
	m := Model{
		api:   c,
		scope: scope{projectID: 7},
		list:  listModel{filter: ListFilter{Status: "closed"}},
	}
	msg := m.fetchInitial()()
	fetched, ok := msg.(initialFetchMsg)
	if !ok {
		t.Fatalf("fetchInitial msg = %T, want initialFetchMsg", msg)
	}
	require.NoError(t, fetched.err)
	if !strings.Contains(gotQuery, "limit=2001") {
		t.Fatalf("limit not sent: %q", gotQuery)
	}
	if strings.Contains(gotQuery, "status=") {
		t.Fatalf("render status leaked into initial fetch: %q", gotQuery)
	}
	if !cacheKeysEqual(fetched.dispatchKey, cacheKey{projectID: 7, limit: queueFetchLimit}) {
		t.Fatalf("dispatchKey = %+v", fetched.dispatchKey)
	}
}

func TestClient_GetIssueDetail_DecodesWrappedEnvelope(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respondJSON(t, w, map[string]any{
			"issue": map[string]any{
				"uid": "01JZ0000000000000000000001", "project_uid": "01JZ0000000000000000000002",
				"short_id": "abc4", "title": "fix", "status": "open",
			},
			"comments": []any{},
			"links": []map[string]any{
				{
					"id": 1, "type": "blocks",
					"from": map[string]any{
						"uid":      "01JZ0000000000000000000001",
						"short_id": "abc4",
					},
					"to": map[string]any{
						"uid":      "01JZ0000000000000000000003",
						"short_id": "def4",
					},
				},
			},
			"labels": []any{},
		})
	})
	detail, err := c.GetIssueDetail(context.Background(), 7, "abc4")
	require.NoError(t, err)
	if gotPath != "/api/v1/projects/7/issues/abc4" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	got := detail.Issue
	if got == nil || got.ShortID != "abc4" || got.Title != "fix" {
		t.Fatalf("unexpected issue: %+v", got)
	}
	if got.UID != "01JZ0000000000000000000001" {
		t.Fatalf("issue UID = %q", got.UID)
	}
	if got.ProjectUID != "01JZ0000000000000000000002" {
		t.Fatalf("project UID = %q", got.ProjectUID)
	}
	links, err := c.ListLinks(context.Background(), 7, "abc4")
	require.NoError(t, err)
	if len(links) != 1 || links[0].From.UID != "01JZ0000000000000000000001" ||
		links[0].To.UID != "01JZ0000000000000000000003" {
		t.Fatalf("link UIDs not decoded: %+v", links)
	}
	if links[0].From.ShortID != "abc4" || links[0].To.ShortID != "def4" {
		t.Fatalf("link short_ids not decoded: %+v", links)
	}
}

func TestClient_ShowIssue_DecodesHierarchy(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, map[string]any{
			"issue": map[string]any{"short_id": "abc4", "title": "fix", "status": "open"},
			"parent": map[string]any{
				"uid": "01JZ0000000000000000000007", "short_id": "wp77",
				"title": "workspace polish", "status": "open",
			},
			"children": []map[string]any{
				{
					"short_id": "ch43", "title": "child", "status": "open",
					"labels": []string{"bug", "ux"},
					"child_counts": map[string]any{
						"open": 1, "total": 2,
					},
				},
			},
			"comments": []any{},
			"links":    []any{},
			"labels": []map[string]any{
				{"issue_id": 1, "label": "prio-1", "author": "a"},
				{"issue_id": 1, "label": "bug", "author": "a"},
			},
		})
	})
	got, err := c.GetIssueDetail(context.Background(), 7, "abc4")
	require.NoError(t, err)
	if got.Issue == nil || got.Issue.ShortID != "abc4" {
		t.Fatalf("unexpected issue: %+v", got.Issue)
	}
	if got.Parent == nil || got.Parent.ShortID != "wp77" || got.Parent.Title != "workspace polish" {
		t.Fatalf("unexpected parent: %+v", got.Parent)
	}
	if len(got.Children) != 1 || got.Children[0].ShortID != "ch43" {
		t.Fatalf("unexpected children: %+v", got.Children)
	}
	if len(got.Children[0].Labels) != 2 || got.Children[0].Labels[0] != "bug" {
		t.Fatalf("child labels not decoded: %+v", got.Children[0].Labels)
	}
	if got.Children[0].ChildCounts == nil || got.Children[0].ChildCounts.Total != 2 {
		t.Fatalf("child counts not decoded: %+v", got.Children[0].ChildCounts)
	}
	if len(got.Issue.Labels) != 2 || got.Issue.Labels[0] != "bug" || got.Issue.Labels[1] != "prio-1" {
		t.Fatalf("issue labels not sorted: %+v", got.Issue.Labels)
	}
}

func TestClient_CreateIssue_SendsIdempotencyHeader(t *testing.T) {
	var gotKey string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		respondJSON(t, w, map[string]any{
			"issue":   map[string]any{"short_id": "aaa1", "title": "t", "status": "open"},
			"changed": true,
		})
	})
	_, err := c.CreateIssue(context.Background(), 7, CreateIssueBody{
		Title: "t", Actor: "alice", IdempotencyKey: "my-key",
	})
	require.NoError(t, err)
	if gotKey != "my-key" {
		t.Fatalf("Idempotency-Key not forwarded: %q", gotKey)
	}
}

func TestClient_DecodeError_ReturnsAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(
			`{"status":404,"error":{"code":"project_not_initialized",` +
				`"message":"no .kata.toml ancestor","hint":"run kata init"}}`))
	})
	_, err := c.GetIssueDetail(context.Background(), 7, "abc4")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "project_not_initialized" {
		t.Fatalf("Code = %q, want project_not_initialized", apiErr.Code)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Fatalf("Status = %d, want 404", apiErr.Status)
	}
	if apiErr.Hint != "run kata init" {
		t.Fatalf("Hint = %q, want run kata init", apiErr.Hint)
	}
	if !strings.Contains(apiErr.Error(), "project_not_initialized") {
		t.Fatalf("Error() = %q, want it to mention the code", apiErr.Error())
	}
}

func TestClient_RemoveLabel_PathEscapesLabel(t *testing.T) {
	var gotRawURI, gotMethod, gotActor string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawURI = r.RequestURI
		gotMethod = r.Method
		gotActor = r.URL.Query().Get("actor")
		respondJSON(t, w, map[string]any{
			"issue":   map[string]any{"short_id": "aaa1", "title": "t", "status": "open"},
			"changed": true,
		})
	})
	_, err := c.RemoveLabel(context.Background(), 7, "abc4", "team/backend", "alice")
	require.NoError(t, err)
	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", gotMethod)
	}
	if !strings.Contains(gotRawURI, "labels/team%2Fbackend") {
		t.Fatalf("label not path-escaped, raw URI = %s", gotRawURI)
	}
	if gotActor != "alice" {
		t.Fatalf("actor query missing: %q", gotActor)
	}
}

func TestClient_ListComments_RoutesThroughShowIssue(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respondJSON(t, w, map[string]any{
			"issue":    map[string]any{"short_id": "abc4", "title": "t", "status": "open"},
			"comments": []map[string]any{{"id": 1, "author": "a", "body": "hi"}},
			"links":    []any{},
			"labels":   []any{},
		})
	})
	got, err := c.ListComments(context.Background(), 7, "abc4")
	require.NoError(t, err)
	if gotPath != "/api/v1/projects/7/issues/abc4" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if len(got) != 1 || got[0].Body != "hi" {
		t.Fatalf("unexpected comments: %+v", got)
	}
}

func TestClient_AssignEmptyOwnerRoutesToUnassign(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respondJSON(t, w, map[string]any{
			"issue":   map[string]any{"short_id": "aaa1", "title": "t", "status": "open"},
			"changed": true,
		})
	})
	_, err := c.Assign(context.Background(), 7, "abc4", "", "alice")
	require.NoError(t, err)
	if !strings.HasSuffix(gotPath, "/actions/unassign") {
		t.Fatalf("expected unassign path, got %s", gotPath)
	}
}

// TestClient_AddLinkSendsToRef pins the wire-shape rename for the link
// POST body: the daemon's CreateLinkRequest.Body carries {actor, type,
// to_ref}, where to_ref accepts a short_id, qualified short_id, or
// 26-char ULID. A pre-cutover client that still sends "to_number"
// would be silently rejected as an unknown field; this regression-
// locks the new shape.
func TestClient_AddLinkSendsToRef(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(bs, &gotBody))
		respondJSON(t, w, map[string]any{
			"issue":   map[string]any{"short_id": "abc4", "title": "t", "status": "open"},
			"changed": true,
		})
	})
	_, err := c.AddLink(context.Background(), 7, "abc4",
		LinkBody{Type: "blocks", ToRef: "xyz4"}, "alice")
	require.NoError(t, err)
	assert.Equal(t, "blocks", gotBody["type"])
	assert.Equal(t, "xyz4", gotBody["to_ref"])
	assert.Equal(t, "alice", gotBody["actor"])
	_, hasOldKey := gotBody["to_number"]
	assert.False(t, hasOldKey, "pre-cutover to_number must not appear on the wire")
}

func TestClient_ListEvents_FiltersByIssueShortID(t *testing.T) {
	matched := "abc4"
	other := "xyz9"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/7/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("after_id") != "" {
			respondJSON(t, w, map[string]any{
				"events":         []map[string]any{},
				"next_after_id":  3,
				"reset_required": false,
			})
			return
		}
		respondJSON(t, w, map[string]any{
			"events": []map[string]any{
				{
					"event_id": 1, "type": "issue.commented",
					"issue_short_id": matched, "actor": "a",
					"project_uid": "01JZ0000000000000000000002",
					"issue_uid":   "01JZ0000000000000000000001",
				},
				{
					"event_id":       2,
					"type":           "issue.commented",
					"issue_short_id": other,
					"actor":          "a",
				},
				{
					"event_id": 3, "type": "issue.labeled",
					"issue_short_id": matched, "actor": "a",
					"project_uid":       "01JZ0000000000000000000002",
					"issue_uid":         "01JZ0000000000000000000001",
					"related_issue_uid": "01JZ0000000000000000000004",
				},
			},
			"next_after_id":  3,
			"reset_required": false,
		})
	})
	got, err := c.ListEvents(context.Background(), 7, "abc4")
	require.NoError(t, err)
	if len(got) != 2 {
		t.Fatalf("got %d events for abc4, want 2", len(got))
	}
	for _, e := range got {
		if e.Type != "issue.commented" && e.Type != "issue.labeled" {
			t.Fatalf("unexpected event leaked through filter: %+v", e)
		}
	}
	if got[0].ProjectUID != "01JZ0000000000000000000002" ||
		got[0].IssueUID != "01JZ0000000000000000000001" {
		t.Fatalf("event UIDs not decoded: %+v", got[0])
	}
	if got[1].RelatedIssueUID != "01JZ0000000000000000000004" {
		t.Fatalf("related issue UID not decoded: %+v", got[1])
	}
}

func TestClient_ListEvents_PaginatesProjectEventStream(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/api/v1/projects/7/events", r.URL.Path)
		require.Equal(t, "1000", r.URL.Query().Get("limit"))
		switch r.URL.Query().Get("after_id") {
		case "":
			respondJSON(t, w, map[string]any{
				"events": []map[string]any{
					{"event_id": 1, "type": "issue.created", "issue_short_id": "aaa1", "actor": "a"},
				},
				"next_after_id":  1,
				"reset_required": false,
			})
		case "1":
			respondJSON(t, w, map[string]any{
				"events": []map[string]any{
					{"event_id": 2, "type": "issue.created", "issue_short_id": "abc4", "actor": "a"},
					{"event_id": 3, "type": "issue.labeled", "issue_short_id": "abc4", "actor": "a"},
				},
				"next_after_id":  3,
				"reset_required": false,
			})
		case "3":
			respondJSON(t, w, map[string]any{
				"events":         []map[string]any{},
				"next_after_id":  3,
				"reset_required": false,
			})
		default:
			t.Fatalf("unexpected after_id: %q", r.URL.Query().Get("after_id"))
		}
	})

	got, err := c.ListEvents(context.Background(), 7, "abc4")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, int64(2), got[0].ID)
	assert.Equal(t, int64(3), got[1].ID)
	assert.Equal(t, 3, calls)
}

// TestClient_ListIssues_NotNilOnSuccess guards the bug where listIssuesAt
// returned resp.Issues evaluated *before* c.do filled it (the do call was
// the second operand of the comma-statement, so resp was nil at capture).
func TestClient_ListIssues_NotNilOnSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"short_id":"aaa1","title":"a","status":"open"}]}`))
	})
	got, err := c.ListIssues(context.Background(), 7, ListFilter{})
	require.NoError(t, err)
	if len(got) != 1 || got[0].ShortID != "aaa1" {
		t.Fatalf("got %+v, want one issue with short_id=aaa1", got)
	}
}

// TestClient_ListAllIssues_NotNilOnSuccess covers the same regression on
// the cross-project endpoint.
func TestClient_ListAllIssues_NotNilOnSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"short_id":"bbb2","title":"b","status":"open"}]}`))
	})
	got, err := c.ListAllIssues(context.Background(), ListFilter{})
	require.NoError(t, err)
	if len(got) != 1 || got[0].ShortID != "bbb2" {
		t.Fatalf("got %+v, want one issue with short_id=bbb2", got)
	}
}

// TestClient_ListProjects_NotNilOnSuccess is the analogue for ListProjects.
func TestClient_ListProjects_NotNilOnSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[{"id":7,"identity":"x","name":"k"}]}`))
	})
	got, err := c.ListProjects(context.Background())
	require.NoError(t, err)
	if len(got) != 1 || got[0].ID != 7 {
		t.Fatalf("got %+v, want one project with id=7", got)
	}
}

// TestClient_ListIssues_FilterShape asserts only the daemon-honored
// query params land on the wire. Owner/Author/Search/Labels are kept on
// the struct for client-side filtering but must not leak as URL params.
func TestClient_ListIssues_FilterShape(t *testing.T) {
	var gotURL string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	})
	_, err := c.ListIssues(context.Background(), 7, ListFilter{
		Status: "open",
		Owner:  "alice",
		Author: "bob",
		Search: "foo",
		Labels: []string{"x"},
	})
	require.NoError(t, err)
	if !strings.Contains(gotURL, "status=open") {
		t.Fatalf("status not sent: %s", gotURL)
	}
	for _, leaked := range []string{"owner=", "author=", "q=", "label=", "include_deleted="} {
		if strings.Contains(gotURL, leaked) {
			t.Fatalf("client leaked %q to wire (daemon ignores it): %s", leaked, gotURL)
		}
	}
}

// TestListIssues_TUIDecodePopulatesLabels (Plan 8 commit 5b): the
// daemon's list response now embeds a labels slice per row (api.IssueOut
// = db.Issue + Labels). The TUI's Issue struct already has the right
// `json:"labels,omitempty"` tag (commit 1), so the wire→struct mapping
// is automatic — this test pins that promise so a future struct-tag
// removal doesn't silently drop labels from the list view.
func TestListIssues_TUIDecodePopulatesLabels(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, map[string]any{
			"issues": []map[string]any{
				{
					"short_id": "aaa1", "title": "first", "status": "open",
					"labels": []string{"bug", "prio-1"},
				},
				{
					"short_id": "bbb2", "title": "second", "status": "open",
					"labels": []string{"enhancement"},
				},
				{
					"short_id": "ccc3", "title": "third", "status": "open",
					// no labels field — omitempty on the wire.
				},
			},
		})
	})
	got, err := c.ListIssues(context.Background(), 7, ListFilter{})
	require.NoError(t, err)
	if len(got) != 3 {
		t.Fatalf("got %d issues, want 3", len(got))
	}
	wantPerShortID := map[string][]string{
		"aaa1": {"bug", "prio-1"},
		"bbb2": {"enhancement"},
		"ccc3": nil,
	}
	for _, iss := range got {
		want := wantPerShortID[iss.ShortID]
		if len(iss.Labels) != len(want) {
			t.Fatalf("issue #%s labels = %v, want %v", iss.ShortID, iss.Labels, want)
		}
		for i := range want {
			if iss.Labels[i] != want[i] {
				t.Fatalf("issue #%s labels[%d] = %q, want %q",
					iss.ShortID, i, iss.Labels[i], want[i])
			}
		}
	}
}

// TestShowIssue_PopulatesLabelsFromTopLevel: the daemon ships labels as
// a sibling slice on the show envelope (one IssueLabel per row, no
// guaranteed order). showIssue extracts the label names, sorts them
// alphabetically, and assigns them to resp.Issue.Labels so downstream
// rendering doesn't have to re-sort. Wire absence (omitempty on the
// Issue struct) means a show response with no labels leaves a
// previously-populated Labels slice empty — covered by other tests.
func TestShowIssue_PopulatesLabelsFromTopLevel(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, map[string]any{
			"issue":    map[string]any{"short_id": "abc4", "title": "fix", "status": "open"},
			"comments": []any{},
			"links":    []any{},
			"labels": []map[string]any{
				{"issue_id": 1, "label": "prio-1", "author": "a"},
				{"issue_id": 1, "label": "bug", "author": "a"},
				{"issue_id": 1, "label": "needs-design", "author": "a"},
			},
		})
	})
	resp, err := c.showIssue(context.Background(), 7, "abc4")
	require.NoError(t, err)
	got := resp.Issue.Labels
	want := []string{"bug", "needs-design", "prio-1"}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestAPIError_EmptyBodyFallback covers the 404 with no body case where
// Code and Message are both blank. Without the fallback, Error() would
// return ": ".
func TestAPIError_EmptyBodyFallback(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.GetIssueDetail(context.Background(), 7, "abc4")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	msg := apiErr.Error()
	if !strings.Contains(msg, "HTTP 404") {
		t.Fatalf("Error() = %q, want it to mention HTTP 404", msg)
	}
	if !strings.Contains(msg, "/api/v1/projects/7/issues/abc4") {
		t.Fatalf("Error() = %q, want it to mention the path", msg)
	}
}

// TestClient_ListProjectsWithStats_Decodes pins that the typed client
// decodes the ?include=stats wire shape into ProjectSummaryWithStats,
// including the optional Stats field. Spec §7.3.
func TestClient_ListProjectsWithStats_Decodes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects", r.URL.Path)
		require.Equal(t, "stats", r.URL.Query().Get("include"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "projects": [
                {"id": 7, "identity": "github.com/wesm/x", "name": "x",
                 "stats": {"open": 3, "closed": 1, "last_event_at": "2026-05-04T12:00:00.000Z"}},
                {"id": 9, "identity": "github.com/wesm/empty", "name": "empty",
                 "stats": {"open": 0, "closed": 0, "last_event_at": null}}
            ]
        }`))
	})

	got, err := c.ListProjectsWithStats(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.NotNil(t, got[0].Stats)
	assert.Equal(t, 3, got[0].Stats.Open)
	assert.Equal(t, 1, got[0].Stats.Closed)
	require.NotNil(t, got[0].Stats.LastEventAt)

	require.NotNil(t, got[1].Stats)
	assert.Equal(t, 0, got[1].Stats.Open)
	assert.Nil(t, got[1].Stats.LastEventAt, "null wire → nil pointer")
}

// TestClient_ListProjectsWithStats_NotNilOnSuccess pins the same
// regression covered for ListIssues / ListAllIssues: a 200 with an empty
// array returns []ProjectSummaryWithStats{}, never nil — callers iterate
// without nil-checks. Spec §7.3.
func TestClient_ListProjectsWithStats_NotNilOnSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects": []}`))
	})

	got, err := c.ListProjectsWithStats(t.Context())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Len(t, got, 0)
}

// TestClient_ResolveProject_PropagatesParseError guards against a
// malformed .kata.toml silently falling through to a start_path
// request. In remote-client mode the daemon cannot stat the
// client's path, so the failure mode is a confusing "no such file"
// rather than the actual broken-config error the user can fix.
func TestClient_ResolveProject_PropagatesParseError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), //nolint:gosec // test fixture
		[]byte("not = valid = toml ==="), 0o644))

	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called.Add(1)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client())

	_, err := c.ResolveProject(t.Context(), dir)
	require.Error(t, err)
	assert.Zero(t, called.Load(), "TUI must reject parse errors before reaching the daemon")
}

// TestClient_ResolveProject_FallsBackOnMissingConfig confirms the
// missing case still works: no .kata.toml means start_path is sent
// for daemon-side filesystem resolution.
func TestClient_ResolveProject_FallsBackOnMissingConfig(t *testing.T) {
	dir := t.TempDir() // no .kata.toml

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client())

	_, err := c.ResolveProject(t.Context(), dir)
	require.NoError(t, err)
	assert.Equal(t, dir, got["start_path"])
	_, hasName := got["name"]
	assert.False(t, hasName)
}

// TestClient_ResolveProject_SendsNameAndAliasForWorkspaceConfig is
// regression coverage for issue #35: when .kata.toml is readable, the
// TUI must send {name, alias} so a daemon on another host can resolve
// without stat'ing the client's filesystem.
func TestClient_ResolveProject_SendsNameAndAliasForWorkspaceConfig(t *testing.T) {
	dir := testfix.InitGitRepo(t)
	require.NoError(t, config.WriteProjectConfig(dir, "project-name"))

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"project-name"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client())

	_, err := c.ResolveProject(t.Context(), dir)
	require.NoError(t, err)
	assert.Equal(t, "project-name", got["name"])
	alias, ok := got["alias"].(map[string]any)
	require.True(t, ok, "alias must be sent alongside name so daemon can do alias-first repair")
	assert.NotEmpty(t, alias["identity"])
	_, hasStartPath := got["start_path"]
	assert.False(t, hasStartPath, "request must be path-free")
}

// TestClient_ResolveProject_SendsAliasOnlyForGitWorkspaceWithoutKataToml
// covers a git workspace without .kata.toml: client sends alias alone.
// Resolve must not derive a project name from the git remote (init
// owns by-convention).
func TestClient_ResolveProject_SendsAliasOnlyForGitWorkspaceWithoutKataToml(t *testing.T) {
	dir := testfix.InitGitRepo(t)

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"x"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client())

	_, err := c.ResolveProject(t.Context(), dir)
	require.NoError(t, err)
	alias, ok := got["alias"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, alias["identity"])
	_, hasName := got["name"]
	assert.False(t, hasName)
	_, hasStartPath := got["start_path"]
	assert.False(t, hasStartPath)
}

// TestClient_ResolveProject_RewritesStaleKataToml verifies the
// rename-repair handoff to the client: when the daemon returns a
// canonical name that differs from the local .kata.toml, the TUI
// rewrites the file. Mirrors the CLI behavior so both clients keep
// .kata.toml fresh.
func TestClient_ResolveProject_RewritesStaleKataToml(t *testing.T) {
	dir := testfix.InitGitRepo(t)
	require.NoError(t, config.WriteProjectConfig(dir, "stale-name"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"canonical-name"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client())

	_, err := c.ResolveProject(t.Context(), dir)
	require.NoError(t, err)

	cfg, _, err := config.FindProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "canonical-name", cfg.Project.Name,
		"stale .kata.toml must be rewritten to the daemon's canonical name")
}
