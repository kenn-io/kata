package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/harness"
)

func TestCLISendsTeammateHeader(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env, dir, pid := setupCLIWorkspace(t)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi", Provider: "zai", Model: "glm-5.3"}, harness.Detected)
	ref := createIssue(t, env, pid, "Existing issue")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	var mu sync.Mutex
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Get("X-Kata-Teammate"))
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	env.URL = server.URL
	for _, want := range []string{"pi.zai-glm-5.3", ""} {
		if want == "" {
			t.Setenv("KATA_TEAMMATE", "")
		}
		for _, args := range [][]string{
			{"label", "add", ref, "example-label"},
			{"edit", ref, "--title", "Updated"},
			{"close", ref, "--done", "--message", "Implemented requested behavior and verified the focused checks.", "--test", "focused checks"},
		} {
			mu.Lock()
			headers = nil
			mu.Unlock()
			_, err := runCLICapture(t, env, dir, args...)
			require.NoError(t, err)
			mu.Lock()
			observed := append([]string(nil), headers...)
			mu.Unlock()
			require.NotEmpty(t, observed)
			for _, got := range observed {
				require.Equal(t, want, got, "%v", args)
			}
		}
	}
}

func TestCLIInvalidTeammateStopsNativeRequests(t *testing.T) {
	for _, key := range []string{"KATA_TEAMMATE", "KATA_TEAMMATE_INFER"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			unsetTeammateEnv(t, "KATA_TEAMMATE")
			t.Setenv(key, "bad/handle")
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"},"issue":{"id":1,"short_id":"abc1","revision":1}}`))
			}))
			t.Cleanup(server.Close)
			for _, args := range [][]string{{"edit", "abc1", "--title", "Updated"}, {"label", "add", "abc1", "label"}, {"show", "abc1"}, {"notify", "abc1", "--to", "coordinator/worker", "--clear"}, {"inbox", "--for", "coordinator/worker"}} {
				_, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), append([]string{"--project", "example-project"}, args...)...)
				require.ErrorContains(t, err, "teammate")
			}
			require.Zero(t, requests)
		})
	}
}

func TestMCPTeammateTransportContext(t *testing.T) {
	for _, mode := range []string{"stdio", "http"} {
		t.Run(mode, func(t *testing.T) {
			setupKataEnv(t)
			t.Setenv("KATA_AUTHOR", "coordinator")
			unsetTeammateEnv(t, "KATA_TEAMMATE")
			t.Setenv("KATA_TEAMMATE_INFER", "model")
			fakeTeammateDetection(t, harness.Detection{Harness: "pi", Model: "example"}, harness.Detected)
			defaultHandle := "pi.example"
			if mode == "http" {
				t.Setenv("KATA_TEAMMATE_INFER", "invalid")
				defaultHandle = ""
			}
			var captureMu sync.Mutex
			captured := make(map[string]string)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveMCPTestHealth(w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
					_, _ = io.WriteString(w, `{"projects":[{"id":42,"name":"spoke-project"}]}`)
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/42/issues/abc1/comments":
					captureMu.Lock()
					captured[r.Header.Get("Idempotency-Key")] = r.Header.Get("X-Kata-Teammate")
					captureMu.Unlock()
					_, _ = io.WriteString(w, `{"comment":{"id":1,"uid":"01AAAAAAAAAAAAAAAAAAAAAAAA","body":"progress","author":"coordinator","created_at":"2030-01-01T00:00:00Z"}}`)
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/ui/telemetry":
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"status":"queued"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(daemon.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var transport sdkmcp.Transport
			if mode == "http" {
				transport = newAuthenticatedMCPHTTPTestTransport(startMCPHTTPTestServer(t, daemon.URL))
			} else {
				serverConn, clientConn := net.Pipe()
				cmd := newRootCmd()
				cmd.SetIn(serverConn)
				cmd.SetOut(serverConn)
				cmd.SetErr(io.Discard)
				cmd.SetArgs([]string{"mcp", "serve", "--all"})
				cmd.SetContext(contextWithBaseURL(ctx, daemon.URL))
				done := make(chan error, 1)
				go func() { done <- cmd.Execute() }()
				t.Cleanup(func() { cancel(); _ = clientConn.Close(); _ = serverConn.Close(); require.NoError(t, <-done) })
				transport = &sdkmcp.IOTransport{Reader: clientConn, Writer: clientConn}
			}
			session, err := newMCPHTTPTestClient().Connect(ctx, transport, nil)
			require.NoError(t, err)
			defer func() { _ = session.Close() }()
			result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.load_issue_mutation", Arguments: map[string]any{}})
			require.NoError(t, err)
			require.False(t, result.IsError)
			for i, tc := range []struct {
				want     string
				override any
			}{{defaultHandle, nil}, {"override", "override"}, {"", ""}, {defaultHandle, nil}} {
				args := map[string]any{"ref": "spoke-project#abc1", "body": "progress", "idempotency_key": fmt.Sprint(i)}
				if tc.override != nil {
					args["teammate"] = tc.override
				}
				result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: args})
				require.NoError(t, err)
				require.False(t, result.IsError, "%+v", result)
				captureMu.Lock()
				got, exists := captured[fmt.Sprint(i)]
				captureMu.Unlock()
				require.True(t, exists, "comment mutation was not captured")
				require.Equal(t, tc.want, got)
			}
		})
	}
}
