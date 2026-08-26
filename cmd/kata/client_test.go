package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
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

func TestEnsureDaemon_RemoteUnavailableMapsToCLIError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1") // closed port

	_, err := ensureDaemon(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ce *cliError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *cliError, got %T (%v)", err, err)
	}
	if ce.Kind != kindDaemonUnavail {
		t.Errorf("expected Kind=%v, got %v", kindDaemonUnavail, ce.Kind)
	}
	if ce.ExitCode != ExitDaemonUnavail {
		t.Errorf("expected ExitCode=%d, got %d", ExitDaemonUnavail, ce.ExitCode)
	}
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

// TestDaemonAPIStatusMapsToCLIError pins the status→cliError contract in the
// one place it now lives. Before daemonAPI this rule was re-typed at ~31 call
// sites, which is how the >= 300 outlier at ensureFederationProjectByName
// survived unnoticed.
func TestDaemonAPIStatusMapsToCLIError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantErr  bool
		wantKind errKind
		wantExit int
		wantCode string
	}{
		{name: "200 ok", status: http.StatusOK, body: `{"ok":true}`},
		{name: "201 created", status: http.StatusCreated, body: `{"ok":true}`},
		{name: "204 no content", status: http.StatusNoContent, body: ``},
		{
			name: "400 validation", status: http.StatusBadRequest,
			body:    `{"error":{"code":"validation","message":"bad input"}}`,
			wantErr: true, wantKind: kindValidation, wantExit: ExitValidation, wantCode: "validation",
		},
		{
			name: "404 not found", status: http.StatusNotFound,
			body:    `{"error":{"code":"not_found","message":"missing"}}`,
			wantErr: true, wantKind: kindNotFound, wantExit: ExitNotFound, wantCode: "not_found",
		},
		{
			name: "409 conflict", status: http.StatusConflict,
			body:    `{"error":{"code":"conflict","message":"already exists"}}`,
			wantErr: true, wantKind: kindConflict, wantExit: ExitConflict, wantCode: "conflict",
		},
		{
			name: "500 internal", status: http.StatusInternalServerError,
			body:    `{"error":{"code":"internal","message":"boom"}}`,
			wantErr: true, wantKind: kindInternal, wantExit: ExitInternal, wantCode: "internal",
		},
		{
			name: "non-json error body", status: http.StatusBadGateway,
			body:    `upstream exploded`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			api := daemonAPI{ctx: context.Background(), baseURL: srv.URL, client: srv.Client()}
			bs, err := api.do(http.MethodGet, "/api/v1/health", nil)

			if !tt.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tt.body, string(bs))
				return
			}
			require.Error(t, err)
			var cerr *cliError
			require.ErrorAs(t, err, &cerr)
			if tt.wantKind != "" {
				assert.Equal(t, tt.wantKind, cerr.Kind)
				assert.Equal(t, tt.wantExit, cerr.ExitCode)
				assert.Equal(t, tt.wantCode, cerr.Code)
			}
		})
	}
}

// TestDaemonAPIURLJoinsAPIRelativePaths pins that paths are API-relative and
// joined in one place, so the API surface the CLI depends on is greppable.
func TestDaemonAPIURLJoinsAPIRelativePaths(t *testing.T) {
	api := daemonAPI{baseURL: "http://127.0.0.1:7777"}
	assert.Equal(t, "http://127.0.0.1:7777/api/v1/federation/status",
		api.url("/api/v1/federation/status"))

	trailing := daemonAPI{baseURL: "http://127.0.0.1:7777/"}
	assert.Equal(t, "http://127.0.0.1:7777/api/v1/federation/status",
		trailing.url("/api/v1/federation/status"))
}
