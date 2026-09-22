package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestEnvHTTPTimeout(t *testing.T) {
	const def = 5 * time.Second

	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "empty returns default", env: "", want: def},
		{name: "valid override", env: "30s", want: 30 * time.Second},
		{name: "minutes parse", env: "2m", want: 2 * time.Minute},
		{name: "garbage falls back", env: "not-a-duration", want: def},
		{name: "zero falls back", env: "0s", want: def},
		{name: "negative falls back", env: "-10s", want: def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertEnvDurationOverride(t, "KATA_HTTP_TIMEOUT", tc.env, def, tc.want, envHTTPTimeout)
		})
	}
}

func TestLongRunningClientForLeavesResponseHeadersUnbounded(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())

	c, err := longRunningClientFor(context.Background(), "http://127.0.0.1:7373")
	if err != nil {
		t.Fatalf("longRunningClientFor: %v", err)
	}

	if c.Timeout != 0 {
		t.Fatalf("long-running client timeout = %v, want no overall timeout", c.Timeout)
	}
	if tr, ok := c.Transport.(*http.Transport); ok && tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("response header timeout = %v, want no response-header cap", tr.ResponseHeaderTimeout)
	}
}

func TestRemoteCommand_UnavailableMapsToCLIError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1") // closed port
	t.Chdir(t.TempDir())

	_, stderr, err := executeRootCapture(t, t.Context(), "projects", "list", "--json")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ce := cliErrorForErr(err, true)
	if ce.Kind != kindDaemonUnavail {
		t.Errorf("expected Kind=%v, got %v", kindDaemonUnavail, ce.Kind)
	}
	if ce.ExitCode != ExitDaemonUnavail {
		t.Errorf("expected ExitCode=%d, got %d", ExitDaemonUnavail, ce.ExitCode)
	}
	assert.Equal(t, ExitDaemonUnavail, exitCodeForErr(err, true))
	assert.Contains(t, stderr, `"kind":"daemon_unavailable"`)
}

func TestFederationEnroll_UnavailableHubKeepsOperationalError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Chdir(t.TempDir())

	_, stderr, err := executeRootCapture(t, t.Context(), "federation", "enroll", "hub-project",
		"--hub-url", "http://127.0.0.1:1", "--spoke-instance", "example-spoke",
		"--capabilities", "pull", "--actor", "user-a", "--json")
	require.Error(t, err)
	assert.Equal(t, ExitInternal, exitCodeForErr(err, true))
	assert.Contains(t, stderr, `"kind":"internal"`)
	assert.NotContains(t, stderr, `"kind":"daemon_unavailable"`)
}

func TestDaemonClients_ClassifyOnlyTheirOwnDialFailures(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Chdir(t.TempDir())
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/external", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	sameOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/closed", http.StatusFound)
			return
		}
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(sameOrigin.Close)

	for _, constructor := range []struct {
		name     string
		url      func(context.Context, string) (*http.Client, error)
		resolved func(context.Context, client.ResolvedDaemon) (*http.Client, error)
	}{
		{name: "default", url: httpClientFor, resolved: httpClientForResolved},
		{name: "long-running", url: longRunningClientFor, resolved: longRunningClientForResolved},
		{name: "streaming", url: streamingClientFor, resolved: streamingClientForResolved},
	} {
		for _, resolved := range []bool{false, true} {
			for _, target := range []struct {
				name, url string
				exit      int
			}{
				{name: "daemon", url: "http://127.0.0.1:1", exit: ExitDaemonUnavail},
				{name: "same-origin redirect", url: sameOrigin.URL, exit: ExitDaemonUnavail},
				{name: "redirect", url: redirect.URL, exit: ExitInternal},
			} {
				t.Run(fmt.Sprintf("%s/resolved=%t/%s", constructor.name, resolved, target.name), func(t *testing.T) {
					var hc *http.Client
					var err error
					if resolved {
						hc, err = constructor.resolved(t.Context(), client.ResolvedDaemon{BaseURL: target.url})
					} else {
						hc, err = constructor.url(t.Context(), target.url)
					}
					require.NoError(t, err)
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.url, nil)
					require.NoError(t, err)
					resp, err := hc.Do(req) //nolint:gosec // loopback fixtures only
					if resp != nil {
						require.NoError(t, resp.Body.Close())
					}
					require.Error(t, err)
					assert.Equal(t, target.exit, exitCodeForErr(fmt.Errorf("request failed: %w", err), true))
				})
			}
		}
	}

	t.Run("redirect policy rejection is not a daemon transport failure", func(t *testing.T) {
		hc, err := markDaemonHTTPClient(sameOrigin.URL, &http.Client{
			Timeout: 200 * time.Millisecond,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("redirect refused by policy")
			},
		})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, sameOrigin.URL, nil)
		require.NoError(t, err)
		resp, err := hc.Do(req) //nolint:gosec // loopback fixture only
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.Error(t, err)
		var provenance daemonTransportProvenance
		assert.NotErrorAs(t, err, &provenance)
		assert.Equal(t, ExitInternal, exitCodeForErr(err, true))
	})
}

type daemonTransportProvenance interface {
	error
	daemonPhase() string
	selectedUnixSocket() bool
}

func TestDaemonClientClassifiesUnixFailuresAcrossGeneratedCall(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.Handler
		wantPhase string
		wantTime  bool
	}{
		{
			name: "headers timeout",
			handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}),
			wantPhase: "headers",
			wantTime:  true,
		},
		{
			name: "body timeout",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":`))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-r.Context().Done()
			}),
			wantPhase: "body",
			wantTime:  true,
		},
		{
			name: "connection closes before headers",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hijacker, ok := w.(http.Hijacker); ok {
					conn, _, err := hijacker.Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}
			}),
			wantPhase: "headers",
		},
		{
			name: "truncated body",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}),
			wantPhase: "body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newUnixGeneratedTestClient(t, tt.handler, 100*time.Millisecond)

			_, err := api.PingWithResponse(t.Context())

			require.Error(t, err)
			var provenance daemonTransportProvenance
			require.ErrorAs(t, err, &provenance)
			assert.Equal(t, tt.wantPhase, provenance.daemonPhase())
			assert.True(t, provenance.selectedUnixSocket())
			var netErr net.Error
			if assert.ErrorAs(t, err, &netErr) {
				assert.Equal(t, tt.wantTime, netErr.Timeout())
			}
			assert.Equal(t, ExitDaemonUnavail, exitCodeForErr(err, true))
		})
	}

	t.Run("complete malformed JSON stays a decode error", func(t *testing.T) {
		api := newUnixGeneratedTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":`))
		}), 100*time.Millisecond)

		resp, err := api.PingWithResponse(t.Context())

		require.Error(t, err)
		require.NotNil(t, resp)
		var provenance daemonTransportProvenance
		assert.NotErrorAs(t, err, &provenance)
		assert.Equal(t, ExitInternal, exitCodeForErr(err, true))
	})

	t.Run("complete structured error keeps API classification", func(t *testing.T) {
		api := newUnixGeneratedTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"temporary_failure","message":"try later"}}`))
		}), 100*time.Millisecond)

		resp, err := api.PingWithResponse(t.Context())

		require.Error(t, err)
		require.NotNil(t, resp)
		var provenance daemonTransportProvenance
		assert.NotErrorAs(t, err, &provenance)
		classified := cliErrorForErr(externalCLIResponseError(resp.StatusCode, resp.Body, err), true)
		assert.Equal(t, "temporary_failure", classified.Code)
		assert.Equal(t, "try later", classified.Message)
	})
}

func TestDaemonTransportErrorOutputModes(t *testing.T) {
	api := newUnixGeneratedTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), 100*time.Millisecond)
	_, err := api.PingWithResponse(t.Context())
	require.Error(t, err)

	classified := cliErrorForErr(err, true)
	assert.Equal(t, kindDaemonUnavail, classified.Kind)
	assert.Equal(t, ExitDaemonUnavail, classified.ExitCode)
	assert.Equal(t, "local Unix-socket daemon timed out while reading response headers", classified.Message)

	for _, mode := range []outputMode{outputHuman, outputJSON, outputAgent} {
		var out bytes.Buffer
		emitErrorForMode(&out, err, mode, true)
		assert.NotContains(t, out.String(), "kata.invalid")
		assert.NotContains(t, out.String(), "http://")
		assert.Contains(t, out.String(), classified.Message)
	}

	var jsonOut bytes.Buffer
	emitErrorForMode(&jsonOut, err, outputJSON, true)
	envelope := parseErrorEnvelope(t, jsonOut.Bytes())
	assert.Equal(t, string(kindDaemonUnavail), envelope.Error.Kind)
	assert.Equal(t, ExitDaemonUnavail, envelope.Error.ExitCode)
}

func TestDaemonTransportErrorMutationUncertainty(t *testing.T) {
	failures := []struct {
		name                string
		phase               string
		cause               error
		possiblyTransmitted bool
	}{
		{name: "headers timeout", phase: "headers", cause: context.DeadlineExceeded, possiblyTransmitted: true},
		{name: "body timeout", phase: "body", cause: context.DeadlineExceeded, possiblyTransmitted: true},
		{name: "unexpected EOF", phase: "body", cause: io.ErrUnexpectedEOF, possiblyTransmitted: true},
		{name: "connection drop", phase: "headers", cause: io.EOF, possiblyTransmitted: true},
		{name: "connection refusal", phase: "connect", cause: &net.OpError{Op: "dial", Err: os.ErrNotExist}},
	}
	methods := []struct {
		method string
		safe   bool
	}{
		{method: http.MethodGet, safe: true},
		{method: http.MethodHead, safe: true},
		{method: http.MethodOptions, safe: true},
		{method: http.MethodPost},
		{method: http.MethodPut},
		{method: http.MethodPatch},
		{method: http.MethodDelete},
		{method: http.MethodConnect},
	}

	for _, failure := range failures {
		for _, method := range methods {
			t.Run(failure.name+"/"+method.method, func(t *testing.T) {
				err := &daemonTransportError{
					selectedOrigin:      "http://kata.invalid",
					unixSocket:          true,
					method:              method.method,
					phase:               failure.phase,
					possiblyTransmitted: failure.possiblyTransmitted,
					cause:               failure.cause,
				}

				classified := cliErrorForErr(err, true)

				wantUnknown := failure.possiblyTransmitted && !method.safe
				assert.Equal(t, wantUnknown, strings.Contains(classified.Message, "mutation result may be unknown"))
				assert.Equal(t, kindDaemonUnavail, classified.Kind)
				assert.Equal(t, ExitDaemonUnavail, classified.ExitCode)
			})
		}
	}
}

func TestSameOriginRedirectPreservesMutationUncertainty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/stall", http.StatusFound)
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	hc, err := markDaemonHTTPClient(server.URL, &http.Client{Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/start", nil)
	require.NoError(t, err)

	_, err = hc.Do(req) //nolint:gosec // loopback fixture only

	require.Error(t, err)
	transportErr, ok := errors.AsType[*daemonTransportError](err)
	require.True(t, ok)
	assert.Equal(t, http.MethodPost, transportErr.method)
	assert.Contains(t, cliErrorForErr(err, true).Message, "mutation result may be unknown")
}

func TestSameOriginRedirectFinalBodyFailurePreservesMutationUncertainty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/truncated", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(server.Close)
	hc, err := markDaemonHTTPClient(server.URL, &http.Client{Timeout: time.Second})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/start", nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // loopback fixture only
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, err = io.ReadAll(resp.Body)

	require.Error(t, err)
	transportErr, ok := errors.AsType[*daemonTransportError](err)
	require.True(t, ok)
	assert.Equal(t, "body", transportErr.phase)
	assert.Equal(t, http.MethodPost, transportErr.method)
	assert.Contains(t, cliErrorForErr(err, true).Message, "mutation result may be unknown")
}

func TestSameOriginRedirectFinalBodyCloseStopsRequestBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/complete", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte("complete"))
	}))
	t.Cleanup(server.Close)
	hc, err := markDaemonHTTPClient(server.URL, &http.Client{Timeout: time.Second})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/start", nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // loopback fixture only
	require.NoError(t, err)
	state := daemonBudgetFromContext(resp.Request.Context())
	require.NotNil(t, state)
	select {
	case <-state.ctx.Done():
		t.Fatal("request budget stopped before final response body closed")
	default:
	}

	require.NoError(t, resp.Body.Close())
	select {
	case <-state.ctx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("request budget did not stop when final response body closed")
	}
}

func TestDifferentOriginRedirectDoesNotClaimMutationUncertainty(t *testing.T) {
	external := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(external.Close)
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, external.URL+"/stall", http.StatusFound)
	}))
	t.Cleanup(selected.Close)
	hc, err := markDaemonHTTPClient(selected.URL, &http.Client{Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, selected.URL, nil)
	require.NoError(t, err)

	_, err = hc.Do(req) //nolint:gosec // loopback fixtures only

	require.Error(t, err)
	var provenance daemonTransportProvenance
	assert.NotErrorAs(t, err, &provenance)
	assert.NotContains(t, cliErrorForErr(err, true).Message, "mutation result may be unknown")
}

func newUnixGeneratedTestClient(t *testing.T, handler http.Handler, timeout time.Duration) *kataclient.Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix-socket transport is not selected on Windows")
	}
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_HTTP_TIMEOUT", timeout.String())
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	httpClient, err := httpClientForResolved(t.Context(), client.ResolvedDaemon{
		BaseURL:    client.UnixBase,
		UnixSocket: socketPath,
	})
	require.NoError(t, err)
	api, err := kataclient.NewWithHTTPClient(client.UnixBase, httpClient)
	require.NoError(t, err)
	return api
}

func TestDaemonDialTimeoutDoesNotClaimCreateOutcomeUnknown(t *testing.T) {
	hc, err := markDaemonHTTPClient("https://daemon.example", &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{IsTimeout: true}}
		}),
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://daemon.example/issues", nil)
	require.NoError(t, err)
	_, err = hc.Do(req) //nolint:gosec // fixture transport does not make network requests
	require.Error(t, err)
	classified := cliErrorForErr(createRequestError(err, false), true)
	assert.Equal(t, "daemon_unavailable", classified.Code)
	assert.Equal(t, ExitDaemonUnavail, classified.ExitCode)
	assert.NotContains(t, classified.Message, "outcome unknown")
	assert.NotContains(t, classified.Message, "mutation result may be unknown")
}

func TestEnsureDaemonResolvedPreservesInjectedResolution(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "injected-token")
	ctx := context.WithValue(t.Context(), client.BaseURLKey{}, "https://daemon.example")

	resolved, err := ensureDaemonResolved(ctx)
	require.NoError(t, err)
	assert.Equal(t, client.DaemonSourceInjected, resolved.Source)
	assert.Equal(t, "https://daemon.example", resolved.BaseURL)
	assert.Equal(t, "injected-token", resolved.Token)
}

func TestDiscoverDaemonResolvedPreservesInjectedResolution(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "discover-injected-token")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
	ctx := context.WithValue(t.Context(), client.BaseURLKey{}, "https://daemon.example")

	resolved, err := discoverDaemonResolved(ctx)
	require.NoError(t, err)
	assert.Equal(t, client.DaemonSourceInjected, resolved.Source)
	assert.Equal(t, "https://daemon.example", resolved.BaseURL)
	assert.Equal(t, "discover-injected-token", resolved.Token)
	assert.True(t, resolved.TrustPrivateNetwork)
}

func TestHTTPClientForResolvedUsesResolvedPolicy(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_ALLOW_INSECURE", "")

	resolved := client.ResolvedDaemon{
		Source:        client.DaemonSourceNamedCatalog,
		Name:          "example-daemon",
		BaseURL:       "http://daemon.example:7777",
		Token:         "catalog-token",
		AllowInsecure: true,
	}
	constructors := []struct {
		name string
		new  func(context.Context, client.ResolvedDaemon) (*http.Client, error)
	}{
		{name: "default", new: httpClientForResolved},
		{name: "long running", new: longRunningClientForResolved},
		{name: "streaming", new: streamingClientForResolved},
	}
	for _, tt := range constructors {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.new(t.Context(), resolved)
			require.NoError(t, err)
			assert.NotNil(t, got)
		})
	}
}

func TestHTTPClientForResolvedRefusesUnsafeTargetWithoutPolicy(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_ALLOW_INSECURE", "")

	_, err := httpClientForResolved(t.Context(), client.ResolvedDaemon{
		Source:  client.DaemonSourceNamedCatalog,
		BaseURL: "http://daemon.example:7777",
		Token:   "catalog-token",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}

func TestHTTPClientForCompatibilityCarriesMatchedAllowInsecure(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_ALLOW_INSECURE", "")
	const baseURL = "http://daemon.example:7777"

	for _, tt := range []struct {
		name          string
		allowInsecure bool
		wantErr       bool
	}{
		{name: "rejects without opt-in", wantErr: true},
		{name: "accepts matched opt-in", allowInsecure: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			workspace := t.TempDir()
			flags.Workspace = workspace
			t.Setenv("KATA_HOME", t.TempDir())
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte(
				"version = 1\n\n[project]\nidentity = \"example.test/spoke-project\"\nname = \"spoke-project\"\n",
			), 0o600))
			localConfig := "version = 1\n\n[server]\nurl = \"" + baseURL + "\"\n"
			if tt.allowInsecure {
				localConfig += "allow_insecure = true\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
				[]byte(localConfig), 0o600))

			got, err := httpClientFor(t.Context(), baseURL)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, got)
		})
	}
}

func TestEnsureDaemonHTTPClientForUsesNamedCatalogCredential(t *testing.T) {
	for _, tt := range []struct {
		name         string
		catalogAuth  string
		authOverride string
		tokenEnv     string
		want         string
	}{
		{name: "catalog token", catalogAuth: `token = "catalog-token"`, want: "Bearer catalog-token"},
		{ //nolint:gosec // Fake credential literals exercise catalog precedence.
			name: "catalog token env", catalogAuth: `token_env = "KATA_SHARED_TOKEN"`,
			tokenEnv: "catalog-env-token", want: "Bearer catalog-env-token",
		},
		{ //nolint:gosec // Fake credential literals exercise catalog precedence.
			name: "auth token env override", catalogAuth: `token_env = "KATA_SHARED_TOKEN"`,
			authOverride: "override-token", tokenEnv: "catalog-env-token", want: "Bearer override-token",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			flags.Daemon = "shared"
			t.Setenv("KATA_AUTH_TOKEN", tt.authOverride)
			t.Setenv("KATA_SHARED_TOKEN", tt.tokenEnv)

			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ping" {
					_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
					return
				}
				got = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)

			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[auth]
token = "global-token"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
`+tt.catalogAuth+`
`), 0o600))

			baseURL, err := ensureDaemon(t.Context())
			require.NoError(t, err)
			hc, err := httpClientFor(t.Context(), baseURL)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/protected", nil)
			require.NoError(t, err)
			resp, err := hc.Do(req) //nolint:gosec // baseURL is the test's own httptest.Server
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnsureDaemonHTTPClientForUsesActiveCatalogToken(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_SERVER", "")

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
			return
		}
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "shared"

[auth]
token = "global-token"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token = "catalog-token"
`), 0o600))

	baseURL, err := ensureDaemon(t.Context())
	require.NoError(t, err)
	hc, err := httpClientFor(t.Context(), baseURL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/protected", nil)
	require.NoError(t, err)
	resp, err := hc.Do(req) //nolint:gosec // baseURL is the test's own httptest.Server
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "Bearer catalog-token", got)
}

func assertEnvDurationOverride(t *testing.T, envKey, envVal string, fallback, want time.Duration, parseFn func(time.Duration) time.Duration) {
	t.Helper()
	t.Setenv(envKey, envVal)
	got := parseFn(fallback)
	if got != want {
		t.Fatalf("%s=%q override failed: got %v, want %v", envKey, envVal, got, want)
	}
}
