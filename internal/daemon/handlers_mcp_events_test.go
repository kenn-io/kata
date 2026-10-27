package daemon_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/testenv"
)

const (
	mcpEventsSecret = "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	mcpEventsURL    = "https://example.com/events?route=inbox"
)

// mcpEventsWebhook returns a real webhook client whose challenge POST reaches a
// local TLS receiver that echoes the challenge.
func mcpEventsWebhook(t *testing.T) *mcpevents.WebhookClient {
	return mcpEventsWebhookRecording(t, nil)
}

// receivedPost is one non-challenge POST that reached the test receiver.
type receivedPost struct {
	Type      string
	ID        string
	Signature string
	ErrorName string
}

type postRecorder struct {
	mu    sync.Mutex
	posts []receivedPost
}

func (r *postRecorder) add(post receivedPost) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, post)
}

func (r *postRecorder) all() []receivedPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receivedPost(nil), r.posts...)
}

// mcpEventsWebhookRecording is mcpEventsWebhook that also records every
// non-challenge POST into recorder when it is set.
func mcpEventsWebhookRecording(t *testing.T, recorder *postRecorder) *mcpevents.WebhookClient {
	t.Helper()
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Type != "verification" && recorder != nil {
			recorder.add(receivedPost{Type: body.Type, ID: r.Header.Get("Webhook-Id"), Signature: r.Header.Get("Webhook-Signature"), ErrorName: body.Error.Message})
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": body.Challenge})
	}))
	t.Cleanup(receiver.Close)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	return mcpevents.NewWebhookClient(mcpevents.WebhookOptions{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
		},
		RootCAs: roots,
	})
}

// mcpEventsService builds the Events service the way the daemon does, over the
// server configuration's own authority.
func mcpEventsService(t *testing.T, cfg daemon.ServerConfig) *mcpevents.Service {
	t.Helper()
	service, err := mcpevents.NewService(mcpevents.ServiceConfig{
		Store:     cfg.DB,
		Keys:      mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "mcp-events.key")),
		Authority: daemon.NewMCPEventsAuthority(cfg),
		Webhook:   mcpEventsWebhook(t),
	})
	require.NoError(t, err)
	require.True(t, service.Availability().Enabled, service.Availability().Reason)
	return service
}

func withMCPEvents(t *testing.T) testenv.Option {
	return func(cfg *daemon.ServerConfig) { cfg.MCPEvents = mcpEventsService(t, *cfg) }
}

// withRunningMCPEventsObserved runs the worker with the given webhook client.
// A non-nil drains counts completed delivery cycles.
func withRunningMCPEventsObserved(t *testing.T, webhook *mcpevents.WebhookClient, drains *atomic.Int64) testenv.Option {
	return func(cfg *daemon.ServerConfig) {
		var drainHook func()
		if drains != nil {
			drainHook = func() { drains.Add(1) }
		}
		service, err := mcpevents.NewService(mcpevents.ServiceConfig{
			Store:        cfg.DB,
			Keys:         mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "mcp-events.key")),
			Authority:    daemon.NewMCPEventsAuthority(*cfg),
			Webhook:      webhook,
			PollInterval: 20 * time.Millisecond,
			DrainHook:    drainHook,
		})
		require.NoError(t, err)
		require.True(t, service.Availability().Enabled, service.Availability().Reason)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = service.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
		cfg.MCPEvents = service
	}
}

func mcpEventsCall(t *testing.T, env *testenv.Env, token, method, path string, body any) (int, []byte) {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	resp, raw := envDoRaw(t, env, method, path, body, headers)
	return resp.StatusCode, raw
}

// mcpEventsFailure decodes an error envelope and returns the HTTP status, the
// MCP wire code and the MCP data object.
func mcpEventsFailure(t *testing.T, status int, raw []byte) (int, map[string]any) {
	t.Helper()
	var envelope api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope), string(raw))
	code, ok := envelope.Error.Data[api.MCPEventsErrorCodeKey].(float64)
	require.Truef(t, ok, "error carries an MCP code: %s", raw)
	data, _ := envelope.Error.Data[api.MCPEventsErrorDataKey].(map[string]any)
	assert.Equal(t, status, envelope.Status)
	return int(code), data
}

func subscribeBody(args map[string]any, scope map[string]any) map[string]any {
	body := map[string]any{
		"name":      mcpevents.EventName,
		"arguments": args,
		"delivery":  map[string]any{"mode": "webhook", "url": mcpEventsURL, "secret": mcpEventsSecret},
	}
	if scope != nil {
		body["forwarder_scope"] = scope
	}
	return body
}

func unsubscribeBody(args map[string]any) map[string]any {
	return map[string]any{
		"name":      mcpevents.EventName,
		"arguments": args,
		"delivery":  map[string]any{"mode": "webhook", "url": mcpEventsURL},
	}
}

type mcpEventsFixture struct {
	env     *testenv.Env
	project db.Project
	other   db.Project
	root    db.Issue
	sibling db.Issue
}

func newMCPEventsFixture(t *testing.T, opts ...testenv.Option) mcpEventsFixture {
	t.Helper()
	all := append([]testenv.Option{testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity()}, opts...)
	env := testenv.New(t, all...)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	sibling := createScopedHTTPTestIssue(t, env, project.ID, "Sibling", nil)
	newScopedTokens(t, env, project, root)
	return mcpEventsFixture{env: env, project: project, other: other, root: root, sibling: sibling}
}

var allProjects = map[string]any{"all": true}

func TestMCPEventsRoutesRoundTrip(t *testing.T) {
	f := newMCPEventsFixture(t, withMCPEvents(t))
	args := map[string]any{"for": "example-user"}

	status, raw := mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/list", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var listed api.MCPEventsListResponse
	require.NoError(t, json.Unmarshal(raw, &listed.Body))
	require.Len(t, listed.Body.Events, 1)
	assert.Equal(t, mcpevents.EventName, listed.Body.Events[0].Name)
	assert.Equal(t, []string{"webhook"}, listed.Body.Events[0].Delivery)

	status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/subscribe", subscribeBody(args, allProjects))
	require.Equal(t, http.StatusOK, status, string(raw))
	var subscribed api.MCPEventsSubscribeResponse
	require.NoError(t, json.Unmarshal(raw, &subscribed.Body))
	assert.Contains(t, subscribed.Body.ID, "sub_")
	assert.NotEmpty(t, subscribed.Body.Cursor)
	assert.False(t, subscribed.Body.Truncated)
	require.NotNil(t, subscribed.Body.RefreshBefore, "an absent ttl gets the 24 hour default")
	assert.True(t, subscribed.Body.DeliveryStatus.Active)
	assert.NotContains(t, string(raw), mcpEventsSecret)
	assert.NotContains(t, string(raw), "example.com/events")

	status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var statuses api.MCPEventsSubscriptionsResponse
	require.NoError(t, json.Unmarshal(raw, &statuses.Body))
	assert.True(t, statuses.Body.Enabled)
	require.Len(t, statuses.Body.Subscriptions, 1)
	assert.Equal(t, subscribed.Body.ID, statuses.Body.Subscriptions[0].ID)
	assert.Equal(t, "example-user", statuses.Body.Subscriptions[0].Recipient)
	assert.Equal(t, "active", statuses.Body.Subscriptions[0].State)
	assert.NotContains(t, string(raw), mcpEventsSecret)
	assert.NotContains(t, string(raw), "example.com/events")

	// Another principal sees none of it.
	status, raw = mcpEventsCall(t, f.env, "worker-token", http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var others api.MCPEventsSubscriptionsResponse
	require.NoError(t, json.Unmarshal(raw, &others.Body))
	assert.Empty(t, others.Body.Subscriptions)

	status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/unsubscribe", unsubscribeBody(args))
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/unsubscribe", unsubscribeBody(args))
	code, data := mcpEventsFailure(t, status, raw)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, mcpevents.CodeNotFound, code)
	assert.Equal(t, "subscription", data["kind"])
}

func TestMCPEventsRoutesMapErrorsToTheWireCodes(t *testing.T) {
	f := newMCPEventsFixture(t, withMCPEvents(t))
	post := func(body map[string]any) (int, []byte) {
		return mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/subscribe", body)
	}
	args := map[string]any{"for": "example-user"}

	unsorted := map[string]any{"for": "example-user", "project_uids": []string{f.other.UID, f.project.UID}}
	status, raw := post(subscribeBody(unsorted, allProjects))
	code, data := mcpEventsFailure(t, status, raw)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, mcpevents.CodeInvalidParams, code)
	assert.Equal(t, "project_uids_not_sorted_unique", data["reason"])

	body := subscribeBody(args, allProjects)
	body["delivery"] = map[string]any{"mode": "poll", "url": mcpEventsURL, "secret": mcpEventsSecret}
	status, raw = post(body)
	code, data = mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeUnsupported, code)
	assert.Equal(t, "deliveryMode", data["feature"])

	body = subscribeBody(args, allProjects)
	body["name"] = "kata.unknown"
	status, raw = post(body)
	code, data = mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeNotFound, code)
	assert.Equal(t, "event", data["kind"])

	// ttl_ms and no_expiry contradict each other.
	body = subscribeBody(args, allProjects)
	body["ttl_ms"] = 60000
	body["no_expiry"] = true
	status, raw = post(body)
	code, data = mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeInvalidParams, code)
	assert.Equal(t, "ttl_conflict", data["reason"])

	// No expiry is honored.
	body = subscribeBody(args, allProjects)
	body["no_expiry"] = true
	status, raw = post(body)
	require.Equal(t, http.StatusOK, status, string(raw))
	var noExpiry api.MCPEventsSubscribeResponse
	require.NoError(t, json.Unmarshal(raw, &noExpiry.Body))
	assert.Nil(t, noExpiry.Body.RefreshBefore)
}

func TestMCPEventsRoutesForbidBootstrap(t *testing.T) {
	f := newMCPEventsFixture(t, withMCPEvents(t))
	args := map[string]any{"for": "example-user"}
	for name, call := range map[string]struct {
		method, path string
		body         any
	}{
		"list":        {http.MethodPost, "/api/v1/mcp-events/list", nil},
		"subscribe":   {http.MethodPost, "/api/v1/mcp-events/subscribe", subscribeBody(args, allProjects)},
		"unsubscribe": {http.MethodPost, "/api/v1/mcp-events/unsubscribe", unsubscribeBody(args)},
		"status":      {http.MethodGet, "/api/v1/mcp-events/subscriptions", nil},
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := mcpEventsCall(t, f.env, "bootstrap-token", call.method, call.path, call.body)
			code, _ := mcpEventsFailure(t, status, raw)
			assert.Equal(t, http.StatusForbidden, status)
			assert.Equal(t, mcpevents.CodeForbidden, code)
		})
	}
}

func TestMCPEventsRoutesConfineScopedTokensToTheirSubtree(t *testing.T) {
	f := newMCPEventsFixture(t, withMCPEvents(t))
	post := func(path string, body any) (int, []byte) {
		return mcpEventsCall(t, f.env, "worker-token", http.MethodPost, path, body)
	}

	status, raw := post("/api/v1/mcp-events/list", nil)
	require.Equal(t, http.StatusOK, status, "a scoped token reaches the routes: %s", raw)

	// An issue outside the subtree is refused, a project it cannot see too.
	status, raw = post("/api/v1/mcp-events/subscribe", subscribeBody(map[string]any{"for": "example-user", "issue_uid": f.sibling.UID}, allProjects))
	code, data := mcpEventsFailure(t, status, raw)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, mcpevents.CodeForbidden, code)
	assert.Equal(t, "issue_not_visible", data["reason"])

	status, raw = post("/api/v1/mcp-events/subscribe", subscribeBody(map[string]any{"for": "example-user", "project_uids": []string{f.other.UID}}, allProjects))
	code, data = mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeForbidden, code)
	assert.Equal(t, "scope_removed", data["reason"])

	// Inside the subtree it works, whatever the forwarder claims to serve.
	status, raw = post("/api/v1/mcp-events/subscribe", subscribeBody(map[string]any{"for": "example-user", "issue_uid": f.root.UID}, allProjects))
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = post("/api/v1/mcp-events/subscribe", subscribeBody(map[string]any{"for": "example-user", "project_uids": []string{f.project.UID}}, allProjects))
	require.Equal(t, http.StatusOK, status, string(raw))

	status, raw = mcpEventsCall(t, f.env, "worker-token", http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var statuses api.MCPEventsSubscriptionsResponse
	require.NoError(t, json.Unmarshal(raw, &statuses.Body))
	assert.Len(t, statuses.Body.Subscriptions, 2)

	status, raw = post("/api/v1/mcp-events/unsubscribe", unsubscribeBody(map[string]any{"for": "example-user", "issue_uid": f.root.UID}))
	require.Equal(t, http.StatusOK, status, string(raw))
}

func TestMCPEventsForwarderScopeOnlyNarrows(t *testing.T) {
	f := newMCPEventsFixture(t, withMCPEvents(t))
	post := func(token string, args, scope map[string]any) (int, []byte) {
		return mcpEventsCall(t, f.env, token, http.MethodPost, "/api/v1/mcp-events/subscribe", subscribeBody(args, scope))
	}
	narrow := map[string]any{"projects": []string{f.project.UID}}
	dynamic := map[string]any{"for": "example-user"}

	// A narrowed forwarder rejects an absent selection and one it does not serve.
	status, raw := post("coordinator-token", dynamic, narrow)
	code, data := mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeForbidden, code)
	assert.Equal(t, "project_scope", data["reason"])
	status, raw = post("coordinator-token", map[string]any{"for": "example-user", "project_uids": []string{f.other.UID}}, narrow)
	code, data = mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeForbidden, code)
	assert.Equal(t, "project_scope", data["reason"])
	status, raw = post("coordinator-token", map[string]any{"for": "example-user", "project_uids": []string{f.project.UID}}, narrow)
	require.Equal(t, http.StatusOK, status, string(raw))

	// Malformed scopes are InvalidParams.
	for name, scope := range map[string]map[string]any{
		"all and projects": {"all": true, "projects": []string{f.project.UID}},
		"neither":          {},
		"not a uid":        {"projects": []string{"not-a-uid"}},
		"duplicates":       {"projects": []string{f.project.UID, f.project.UID}},
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := post("coordinator-token", dynamic, scope)
			code, data := mcpEventsFailure(t, status, raw)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, mcpevents.CodeInvalidParams, code)
			assert.Equal(t, "invalid_forwarder_scope", data["reason"])
		})
	}
}

func TestMCPEventsRoutesServeStaticTokenAndLocalPrincipals(t *testing.T) {
	args := map[string]any{"for": "example-user"}
	for name, tc := range map[string]struct {
		opts  []testenv.Option
		token string
	}{
		"static token": {opts: []testenv.Option{testenv.WithAuthToken("static-token")}, token: "static-token"},
		"local":        {},
	} {
		t.Run(name, func(t *testing.T) {
			env := testenv.New(t, append(tc.opts, withMCPEvents(t))...)
			status, raw := mcpEventsCall(t, env, tc.token, http.MethodPost, "/api/v1/mcp-events/subscribe", subscribeBody(args, allProjects))
			require.Equal(t, http.StatusOK, status, string(raw))
			status, raw = mcpEventsCall(t, env, tc.token, http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var statuses api.MCPEventsSubscriptionsResponse
			require.NoError(t, json.Unmarshal(raw, &statuses.Body))
			assert.Len(t, statuses.Body.Subscriptions, 1)
			status, raw = mcpEventsCall(t, env, tc.token, http.MethodPost, "/api/v1/mcp-events/unsubscribe", unsubscribeBody(args))
			require.Equal(t, http.StatusOK, status, string(raw))
		})
	}
}

type mcpEventsHost struct{ operations []daemon.HostOperation }

func (h *mcpEventsHost) Authorize(_ context.Context, r daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	h.operations = append(h.operations, r.Operation)
	return daemon.HostAccessDecision{
		Revalidate:       func(context.Context) error { return nil },
		TransactionFence: func(context.Context, db.Transaction) error { return nil },
	}, nil
}

func TestMCPEventsRoutesServeHostPrincipals(t *testing.T) {
	env := testenv.New(t)
	host := &mcpEventsHost{}
	cfg := daemon.ServerConfig{DB: env.DB, HostAccess: host}
	cfg.MCPEvents = mcpEventsService(t, cfg)
	server := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	call := func(subject, method, path string, body any) (int, []byte) {
		var reader *bytes.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		request := httptest.NewRequest(method, path, reader)
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: subject, Actor: "reader"}))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder.Code, recorder.Body.Bytes()
	}
	args := map[string]any{"for": "example-user"}
	status, raw := call("host-a", http.MethodPost, "/api/v1/mcp-events/subscribe", subscribeBody(args, allProjects))
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NotEmpty(t, host.operations)
	assert.Equal(t, "mcpEventsSubscribe", host.operations[0].ID)
	assert.False(t, host.operations[0].Policy.Mutation)
	assert.True(t, host.operations[0].AllProjects)

	status, raw = call("host-a", http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var own api.MCPEventsSubscriptionsResponse
	require.NoError(t, json.Unmarshal(raw, &own.Body))
	assert.Len(t, own.Body.Subscriptions, 1)

	// Another host subject is another principal.
	status, raw = call("host-b", http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var other api.MCPEventsSubscriptionsResponse
	require.NoError(t, json.Unmarshal(raw, &other.Body))
	assert.Empty(t, other.Body.Subscriptions)
	status, raw = call("host-b", http.MethodPost, "/api/v1/mcp-events/unsubscribe", unsubscribeBody(args))
	code, _ := mcpEventsFailure(t, status, raw)
	assert.Equal(t, mcpevents.CodeNotFound, code)
}

func TestMCPEventsRoutesReportADisabledService(t *testing.T) {
	for name, reason := range map[string]string{
		"not configured":  "",
		"key unavailable": mcpevents.ReasonKeyUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			want := reason
			if want == "" {
				want = mcpevents.ReasonNotConfigured
			}
			f := newMCPEventsFixture(t, func(cfg *daemon.ServerConfig) { cfg.MCPEventsDisabledReason = reason })

			status, raw := mcpEventsCall(t, f.env, "coordinator-token", http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var statuses api.MCPEventsSubscriptionsResponse
			require.NoError(t, json.Unmarshal(raw, &statuses.Body))
			assert.False(t, statuses.Body.Enabled)
			assert.Equal(t, want, statuses.Body.Reason)
			assert.Empty(t, statuses.Body.Subscriptions)

			status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/subscribe",
				subscribeBody(map[string]any{"for": "example-user"}, allProjects))
			code, data := mcpEventsFailure(t, status, raw)
			assert.Equal(t, http.StatusServiceUnavailable, status)
			assert.Equal(t, mcpevents.CodeUnavailable, code)
			assert.Equal(t, want, data["reason"])

			// Listing reports no events when Events is not configured and
			// Unavailable for any other reason.
			status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/list", nil)
			if want == mcpevents.ReasonNotConfigured {
				require.Equal(t, http.StatusOK, status, string(raw))
				var listed api.MCPEventsListResponse
				require.NoError(t, json.Unmarshal(raw, &listed.Body))
				assert.NotNil(t, listed.Body.Events)
				assert.Empty(t, listed.Body.Events)
			} else {
				code, data = mcpEventsFailure(t, status, raw)
				assert.Equal(t, mcpevents.CodeUnavailable, code)
				assert.Equal(t, want, data["reason"])
			}

			// Ordinary daemon operation is unaffected.
			status, raw = mcpEventsCall(t, f.env, "coordinator-token", http.MethodGet, "/api/v1/projects", nil)
			assert.Equal(t, http.StatusOK, status, string(raw))
		})
	}
}

func TestRevokingATokenPromptlyEndsItsMCPEventsSubscriptions(t *testing.T) {
	var recorder postRecorder
	var drains atomic.Int64
	f := newMCPEventsFixture(t, withRunningMCPEventsObserved(t, mcpEventsWebhookRecording(t, &recorder), &drains))
	args := map[string]any{"for": "example-user"}
	status, raw := mcpEventsCall(t, f.env, "coordinator-token", http.MethodPost, "/api/v1/mcp-events/subscribe", subscribeBody(args, allProjects))
	require.Equal(t, http.StatusOK, status, string(raw))

	tokens, err := f.env.DB.ListAPITokens(t.Context())
	require.NoError(t, err)
	var revoked db.APIToken
	for _, token := range tokens {
		if token.Actor == "coordinator" {
			revoked = token
		}
	}
	require.NotZero(t, revoked.ID)
	principal := mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: strconv.FormatInt(revoked.ID, 10), Actor: "coordinator"}
	rows, err := f.env.DB.ListMCPEventSubscriptions(t.Context(), principal.Key())
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Let the worker mark the idle subscription: its first run starts on the
	// next cycle and its finish triggers another, so wait for three completed
	// cycles. Revoking writes an audit event, which moves the archive head and
	// makes the worker look again; no separate Recheck call is needed. The
	// revocation is noticed whether or not the mark was set, so this only
	// steers the test toward the idle path.
	settled := drains.Load()
	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool { return drains.Load() >= settled+3 }, 10*time.Second, 5*time.Millisecond, "the worker completes delivery cycles")
	status, raw = mcpEventsCall(t, f.env, "bootstrap-token", http.MethodPost, "/api/v1/tokens/"+strconv.FormatInt(revoked.ID, 10)+"/actions/revoke", nil)
	require.Equal(t, http.StatusOK, status, string(raw))

	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool {
		rows, err := f.env.DB.ListMCPEventSubscriptions(t.Context(), principal.Key())
		return err == nil && len(rows) == 1 && rows[0].State == db.MCPEventStateEnded && rows[0].EndReason == "principal_revoked"
	}, 10*time.Second, 25*time.Millisecond, "revocation ends the subscription without waiting for the one-minute sweep")

	// The terminated control reaches the receiver once, signed, and then the
	// row has nothing pending, so no further POST follows.
	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool {
		rows, err := f.env.DB.ListMCPEventSubscriptions(t.Context(), principal.Key())
		return err == nil && len(rows) == 1 && rows[0].PendingKind == db.MCPEventPendingNone
	}, 10*time.Second, 25*time.Millisecond, "the terminated control is acknowledged")
	posts := recorder.all()
	require.Len(t, posts, 1)
	assert.Equal(t, "terminated", posts[0].Type)
	assert.True(t, strings.HasPrefix(posts[0].ID, "msg_terminated_"), posts[0].ID)
	assert.True(t, strings.HasPrefix(posts[0].Signature, "v1,"), "the control is signed")
	assert.Equal(t, "Forbidden", posts[0].ErrorName)
}

func TestStaticTokenReplacementRevokesPersistedEventsAfterRestart(t *testing.T) {
	var cfg daemon.ServerConfig
	keyPath := filepath.Join(t.TempDir(), "events.key")
	webhook := mcpEventsWebhook(t)
	build := func() *mcpevents.Service {
		svc, err := mcpevents.NewService(mcpevents.ServiceConfig{
			Store: cfg.DB, Keys: mcpevents.NewFileKeyProvider(keyPath),
			Authority: daemon.NewMCPEventsAuthority(cfg), Webhook: webhook,
		})
		require.NoError(t, err)
		return svc
	}
	env := testenv.New(t, testenv.WithAuthToken("original-token"), func(c *daemon.ServerConfig) {
		cfg = *c
		c.MCPEvents = build()
	})
	body := subscribeBody(map[string]any{"for": "example-user"}, allProjects)
	body["no_expiry"] = true // replacing a credential must also stop indefinite grants
	status, raw := mcpEventsCall(t, env, "original-token", http.MethodPost, "/api/v1/mcp-events/subscribe", body)
	require.Equal(t, http.StatusOK, status, string(raw))
	rows, err := env.DB.ListDueMCPEventSubscriptions(t.Context(), time.Now().UTC(), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	require.Nil(t, row.ExpiresAt)
	require.NoError(t, build().Drain(t.Context()))
	live, err := env.DB.GetMCPEventSubscription(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, db.MCPEventStateActive, live.State, "same-token restart preserves the subscription")
	cfg.Auth.Token = "replacement-token"
	require.NoError(t, build().Drain(t.Context()))
	ended, err := env.DB.GetMCPEventSubscription(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, db.MCPEventStateEnded, ended.State)
	assert.Equal(t, "principal_revoked", ended.EndReason)
}
