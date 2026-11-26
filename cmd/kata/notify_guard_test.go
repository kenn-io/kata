package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotifyNoReplaceReportsHeldRequest(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "review the callback")
	runCLIAs(t, env, dir, "agent-a", "notify", ref, "--to", "reviewer", "--message", "first pass")

	_, err := runCLICapture(t, env, dir, "--as", "agent-b", "notify", ref, "--to", "reviewer", "--message", "second pass", "--no-replace")
	cli := requireCLIError(t, err, ExitConflict)
	assert.Equal(t, "notification_held", cli.Code)
	assert.Contains(t, cli.Message, "agent-a")
	assert.Contains(t, cli.Message, "first pass")
	assert.Contains(t, cli.Hint, "--no-replace")
}

func TestNotifyClearIfFromLeavesOtherSendersRequest(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "review the callback")
	runCLIAs(t, env, dir, "agent-a", "notify", ref, "--to", "reviewer", "--message", "first pass")

	out := runCLIAs(t, env, dir, "agent-b", "notify", ref, "--to", "reviewer", "--clear", "--if-from", "agent-b")
	assert.Contains(t, out, "left unchanged")
	metadata := notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.Contains(t, metadata, "notify.cmV2aWV3ZXI")
}

func TestNotifyGuardFlagsRejectOldDaemon(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "review the callback")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.51.0"}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path != "/api/v1/projects/resolve" {
			writes.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	old := *env
	old.URL = server.URL
	for _, args := range [][]string{
		{"--message", "m", "--no-replace"},
		{"--clear", "--if-from", "agent-a"},
	} {
		_, err := runCLICapture(t, &old, dir, append([]string{"notify", ref, "--to", "reviewer"}, args...)...)
		require.ErrorContains(t, err, "requires daemon API 0.72.0")
	}
	assert.Zero(t, writes.Load())
}

func TestNotifyGuardFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--to", "reader", "--message", "m", "--if-from", "a"},
		{"--to", "reader", "--clear", "--no-replace"},
		{"--broadcast", "--message", "m", "--no-replace"},
		{"--to", "reader", "--clear", "--if-from", " "},
	} {
		resetFlags(t)
		_, _, err := executeRootCapture(t, t.Context(), append([]string{"notify", "abcd"}, args...)...)
		_ = requireCLIError(t, err, ExitValidation)
	}
}

func TestNotificationAPIErrorShowsDisplayNameForFederatedSender(t *testing.T) {
	const stored = "Ana [member:11111111-1111-4111-8111-111111111111; enrollment:11111111-1111-4111-8111-111111111111]"
	body := []byte(`{"status":409,"error":{"code":"notification_held","message":"held","data":{"from":"` + stored + `","message":"first pass"}}}`)

	cli := notificationAPIError(http.StatusConflict, body)

	assert.Equal(t, "held: from Ana: first pass", cli.Message)
	assert.Contains(t, string(cli.Data), stored, "error.data keeps the stored sender")
}
