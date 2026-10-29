package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/testenv"
)

const (
	eventsE2ESecret   = "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	eventsE2ECallback = "https://example.com/inbox"
)

// signedReceiver is a TLS webhook receiver that verifies the Standard Webhooks
// signature of everything it accepts and answers verification challenges.
type signedReceiver struct {
	server *httptest.Server
	events chan mcpevents.Envelope
}

func newSignedReceiver(t *testing.T) *signedReceiver {
	t.Helper()
	receiver := &signedReceiver{events: make(chan mcpevents.Envelope, 32)}
	receiver.server = httptest.NewTLSServer(http.HandlerFunc(receiver.serve))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (r *signedReceiver) serve(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(req.Body, mcpevents.MaxEnvelopeBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(eventsE2ESecret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, req.Header.Get("Webhook-Id")+"."+req.Header.Get("Webhook-Timestamp")+".")
	_, _ = mac.Write(raw)
	valid := false
	for candidate := range strings.FieldsSeq(req.Header.Get("Webhook-Signature")) {
		if encoded, ok := strings.CutPrefix(candidate, "v1,"); ok {
			signature, decodeErr := base64.StdEncoding.DecodeString(encoded)
			valid = valid || (decodeErr == nil && hmac.Equal(signature, mac.Sum(nil)))
		}
	}
	if stamp, err := strconv.ParseInt(req.Header.Get("Webhook-Timestamp"), 10, 64); err != nil || !valid ||
		stamp < time.Now().Unix()-300 || stamp > time.Now().Unix()+300 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var verification struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(raw, &verification) == nil && verification.Type == "verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": verification.Challenge})
		return
	}
	var event mcpevents.Envelope
	if json.Unmarshal(raw, &event) != nil || event.EventID == "" || event.EventID != req.Header.Get("Webhook-Id") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.events <- event
	w.WriteHeader(http.StatusNoContent)
}

func (r *signedReceiver) next(t *testing.T, match func(mcpevents.Envelope) bool) mcpevents.Envelope {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-r.events:
			if match(event) {
				return event
			}
		case <-timer.C:
			require.FailNow(t, "expected signed delivery did not arrive")
		}
	}
}

// withRunningEventsDaemon enables the daemon-owned Events service with a test
// key provider, delivering to receiver, and runs its worker for the test.
func withRunningEventsDaemon(t *testing.T, receiver *signedReceiver) testenv.Option {
	return func(cfg *daemon.ServerConfig) {
		roots := x509.NewCertPool()
		roots.AddCert(receiver.server.Certificate())
		service, err := mcpevents.NewService(mcpevents.ServiceConfig{
			Store:     cfg.DB,
			Keys:      mcpevents.NewFileKeyProvider(t.TempDir() + "/mcp-events.key"),
			Authority: daemon.NewMCPEventsAuthority(*cfg),
			Webhook: mcpevents.NewWebhookClient(mcpevents.WebhookOptions{
				Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
				},
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, receiver.server.Listener.Addr().String())
				},
				RootCAs: roots,
			}),
			PollInterval: 20 * time.Millisecond,
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

// mcpRPC posts one 2026-07-28 request to the kata mcp serve endpoint.
func mcpRPC(t *testing.T, endpoint, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    currentMCPProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer test-mcp-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", currentMCPProtocolVersion)
	request.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		request.Header.Set("Mcp-Name", name)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded), string(body))
	return response.StatusCode, decoded
}

func mcpTool(t *testing.T, endpoint, name string, args map[string]any) map[string]any {
	t.Helper()
	status, decoded := mcpRPC(t, endpoint, "tools/call", map[string]any{"name": "kata." + name, "arguments": args})
	require.Equal(t, http.StatusOK, status, "%v", decoded)
	require.NotContains(t, decoded, "error", "%v", decoded)
	result := decoded["result"].(map[string]any)
	require.NotEqual(t, true, result["isError"], "%s: %v", name, result)
	structured, _ := result["structuredContent"].(map[string]any)
	return structured
}

func eventsSubscribeParams(recipient string, projectUIDs []string) map[string]any {
	arguments := map[string]any{"for": recipient}
	if projectUIDs != nil {
		arguments["project_uids"] = projectUIDs
	}
	return map[string]any{
		"name":      mcpevents.EventName,
		"arguments": arguments,
		"delivery":  map[string]any{"mode": "webhook", "url": eventsE2ECallback, "secret": eventsE2ESecret},
	}
}

// A cron job's notify action reaches the inbox through the daemon; `kata mcp
// serve` only forwards the subscription, and the daemon delivers the signed
// webhook. No Events state lives in the MCP process.
func TestMCPServeCronNotificationDeliversSignedWebhookThroughTheDaemon(t *testing.T) {
	receiver := newSignedReceiver(t)
	env := testenv.New(t, withRunningEventsDaemon(t, receiver))
	t.Setenv("KATA_AUTHOR", "example-agent")
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	endpoint := startMCPHTTPTestServerWithScope(t, env.URL, []string{"--projects", "spoke-project"})

	discovered, discoveryBody := mcpRPC(t, endpoint, "server/discover", map[string]any{})
	require.Equal(t, http.StatusOK, discovered, "%v", discoveryBody)
	capabilities := discoveryBody["result"].(map[string]any)["capabilities"].(map[string]any)
	assert.Equal(t, map[string]any{}, capabilities["events"])
	assert.Equal(t, map[string]any{}, capabilities["extensions"].(map[string]any)["io.modelcontextprotocol/events"])

	// A project-bound forwarder refuses a selection that follows every project,
	// and accepts one inside the projects it serves.
	status, refused := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", nil))
	assert.Equal(t, http.StatusOK, status, "%v", refused)
	refusal := refused["error"].(map[string]any)
	assert.InDelta(t, float64(mcpevents.CodeForbidden), refusal["code"], 0.5)
	assert.Equal(t, "Forbidden", refusal["message"])

	for _, loader := range []string{"load_issue_mutation", "load_issue_discovery", "load_coordination", "load_cron"} {
		mcpTool(t, endpoint, loader, map[string]any{})
	}
	parent := mcpTool(t, endpoint, "create", map[string]any{
		"project": "spoke-project", "title": "Recurring review", "idempotency_key": "cron-parent-fixture",
	})["issue"].(map[string]any)

	status, subscribed := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", []string{project.UID}))
	require.Equal(t, http.StatusOK, status, "%v", subscribed)
	require.NotContains(t, subscribed, "error", "%v", subscribed)
	result := subscribed["result"].(map[string]any)
	assert.Equal(t, "complete", result["resultType"])
	assert.NotEmpty(t, result["id"])

	jobUID := mcpTool(t, endpoint, "cron_new_uid", map[string]any{})["uid"].(string)
	created := mcpTool(t, endpoint, "cron_job_create", map[string]any{
		"project": "spoke-project", "uid": jobUID, "name": "Recurring review", "parent_ref": parent["uid"],
		"definition": map[string]any{
			"version": 1, "kind": "job", "enabled": true,
			"trigger": map[string]any{"kind": "interval", "interval_seconds": 60},
			"action":  map[string]any{"kind": "notify", "recipient": "example-agent/worker"},
			"issue":   map[string]any{"kind": "per-run", "title": "Review synthetic fixture"},
			"overlap": "allow", "catchup": "latest",
		},
	})["job"].(map[string]any)
	boundAt, err := time.Parse(time.RFC3339Nano, created["created_at"].(string))
	require.NoError(t, err)
	due := boundAt.Add(61 * time.Second)
	require.NoError(t, daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(nil, nil), "UTC").RunOnce(t.Context(), due))

	delivery := receiver.next(t, func(e mcpevents.Envelope) bool { return e.Data.Reason == "notification" })
	assert.Equal(t, "kata.inbox.changed", delivery.Name)
	assert.Equal(t, "example-agent/worker", delivery.Data.Recipient)
	assert.Equal(t, project.UID, delivery.Data.ProjectUID)
	assert.NotEmpty(t, delivery.Data.IssueUID)

	status, listed := mcpRPC(t, endpoint, "events/list", map[string]any{})
	require.Equal(t, http.StatusOK, status, "%v", listed)
	assert.Equal(t, "complete", listed["result"].(map[string]any)["resultType"])
	health := mcpTool(t, endpoint, "event_subscriptions", map[string]any{})
	assert.Equal(t, true, health["enabled"])
	subscriptions := health["subscriptions"].([]any)
	require.Len(t, subscriptions, 1)
	assert.Equal(t, result["id"], subscriptions[0].(map[string]any)["id"])

	status, unsubscribed := mcpRPC(t, endpoint, "events/unsubscribe", map[string]any{
		"name":      mcpevents.EventName,
		"arguments": map[string]any{"for": "example-agent/worker", "project_uids": []string{project.UID}},
		"delivery":  map[string]any{"mode": "webhook", "url": eventsE2ECallback},
	})
	require.Equal(t, http.StatusOK, status, "%v", unsubscribed)
	require.NotContains(t, unsubscribed, "error", "%v", unsubscribed)

	for _, path := range kataHomeFiles(t, env.Home) {
		assert.NotContains(t, path, "mcp-events", "the forwarder writes no Events state under KATA_HOME")
	}
}

// A forwarder bound to one project (--project) refuses a subscription that
// follows every project, and the real daemon accepts an explicit subset.
func TestMCPServeBoundProjectRefusesDynamicSubscriptionFromTheDaemon(t *testing.T) {
	receiver := newSignedReceiver(t)
	env := testenv.New(t, withRunningEventsDaemon(t, receiver))
	t.Setenv("KATA_AUTHOR", "example-agent")
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	endpoint := startMCPHTTPTestServerWithScope(t, env.URL, []string{"--project", "spoke-project"})

	_, refused := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", nil))
	refusal, ok := refused["error"].(map[string]any)
	require.True(t, ok, "%v", refused)
	assert.InDelta(t, float64(mcpevents.CodeForbidden), refusal["code"], 0.5)
	assert.Equal(t, "Forbidden", refusal["message"])

	_, accepted := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", []string{project.UID}))
	require.NotContains(t, accepted, "error", "%v", accepted)
}

// A daemon with Events disabled (the default) still sits behind a forwarder
// that advertises the extension. Listing returns no events instead of an
// error, while subscribing keeps reporting that Events is unavailable.
func TestMCPServeListsNoEventsWhenTheDaemonHasEventsDisabled(t *testing.T) {
	env := testenv.New(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	endpoint := startMCPHTTPTestServerWithScope(t, env.URL, []string{"--all"})

	status, listed := mcpRPC(t, endpoint, "events/list", map[string]any{})
	require.Equal(t, http.StatusOK, status, "%v", listed)
	require.NotContains(t, listed, "error", "%v", listed)
	result, ok := listed["result"].(map[string]any)
	require.True(t, ok, "%v", listed)
	assert.Empty(t, result["events"])
	assert.NotNil(t, result["events"])
	assert.Equal(t, "complete", result["resultType"])

	_, subscribed := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", nil))
	refusal, ok := subscribed["error"].(map[string]any)
	require.True(t, ok, "%v", subscribed)
	assert.InDelta(t, float64(mcpevents.CodeUnavailable), refusal["code"], 0.5)
	assert.Equal(t, "Unavailable", refusal["message"])
}
