package daemon_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/testenv"
)

const configureSigningPath = "/api/v1/federation/replicas/01HZNQ7VFPK1XGD8R5MABCD4EX/actions/configure-signing"

func TestConfigureFederationSigningUsesDaemonStore(t *testing.T) {
	for _, source := range []string{"env", "file"} {
		t.Run(source, func(t *testing.T) {
			credentials := newReplicaCredentialStore()
			current := config.FederationCredential{
				HubURL: "https://old-hub.example/mount", HubProjectID: 42,
				Token: "enrollment-secret", Actor: "example-actor", ManagedByConfig: true,
				HubCatalog: "hub-daemon", Signing: &federationsigning.Source{KeyID: "retired-key"},
			}
			credentials.put(replicaHubProjectUID, current)
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
			body := map[string]any{"key_id": "key-a"}
			selected := federationsigning.Source{KeyID: "key-a", HubURL: current.HubURL}
			if source == "env" {
				t.Setenv("TEST_DAEMON_SIGNING_KEY", strings.Repeat("k", 64))
				body["key_env"] = "TEST_DAEMON_SIGNING_KEY"
				selected.KeyEnv = "TEST_DAEMON_SIGNING_KEY"
			} else {
				keyPath := filepath.Join(t.TempDir(), "signing.key")
				require.NoError(t, os.WriteFile(keyPath, []byte(strings.Repeat("k", 64)), 0600))
				body["key_file"] = keyPath
				body["hub_url"] = "https://HUB.EXAMPLE:443/new-mount"
				selected.KeyFile, selected.HubURL = keyPath, "https://hub.example/new-mount"
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, configureSigningPath, body, nil)
			require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", raw)
			got, found, err := credentials.FederationCredential(t.Context(), replicaHubProjectUID)
			require.NoError(t, err)
			require.True(t, found)
			current.Signing = &selected
			require.Equal(t, current, got)
			require.Equal(t, 1, credentials.replaceCalls)
		})
	}
}

func TestConfigureFederationSigningRejectsInvalidSourceAndInactiveCredentials(t *testing.T) {
	for _, tc := range []struct {
		name             string
		body             map[string]any
		missing, leaving bool
		want             int
	}{
		{name: "source unavailable", body: map[string]any{"key_id": "key-a", "key_env": "TEST_MISSING_SIGNING_KEY"}, want: http.StatusBadRequest},
		{name: "relative file", body: map[string]any{"key_id": "key-a", "key_file": "signing.key"}, want: http.StatusBadRequest},
		{name: "missing credential", missing: true, want: http.StatusNotFound},
		{name: "leave pending", leaving: true, want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_MISSING_SIGNING_KEY", "")
			if tc.name == "relative file" {
				t.Chdir(t.TempDir())
				require.NoError(t, os.WriteFile("signing.key", []byte(strings.Repeat("k", 64)), 0600))
			}
			credentials := newReplicaCredentialStore()
			current := config.FederationCredential{HubURL: "https://hub.example", Token: "keep-token", LeavePending: tc.leaving}
			if !tc.missing {
				credentials.put(replicaHubProjectUID, current)
			}
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
			body := tc.body
			if body == nil {
				body = map[string]any{"key_id": "key-a", "key_env": "TEST_MISSING_SIGNING_KEY"}
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, configureSigningPath, body, nil)
			require.Equal(t, tc.want, resp.StatusCode, "%s", raw)
			got, _, err := credentials.FederationCredential(t.Context(), replicaHubProjectUID)
			require.NoError(t, err)
			if !tc.missing {
				require.Equal(t, current, got)
			}
			require.Zero(t, credentials.replaceCalls)
		})
	}
}

func TestConfigureFederationSigningRequiresOwnerAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		identity    bool
		want        int
	}{
		{name: "local owner", want: http.StatusNoContent},
		{name: "static admin", token: "admin-token", want: http.StatusNoContent},
		{name: "bootstrap admin", token: "admin-token", identity: true, want: http.StatusNoContent},
		{name: "user token", token: "user-token", identity: true, want: http.StatusForbidden},
		{name: "missing bearer", token: "", identity: true, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_DAEMON_SIGNING_KEY", strings.Repeat("k", 64))
			credentials := newReplicaCredentialStore()
			credentials.put(replicaHubProjectUID, config.FederationCredential{HubURL: "https://hub.example", Token: "keep-token"})
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.FederationCredentials = credentials
				if tc.name != "local owner" {
					cfg.Auth.Token = "admin-token"
				}
				cfg.Auth.RequireTokenIdentity = tc.identity
			})
			if tc.identity {
				_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "user-token", Actor: "example-actor", AdminActor: db.BootstrapActor})
				require.NoError(t, err)
			}
			headers := map[string]string{}
			if tc.token != "" {
				headers = bearer(tc.token)
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, configureSigningPath,
				map[string]any{"key_id": "key-a", "key_env": "TEST_DAEMON_SIGNING_KEY"}, headers)
			require.Equal(t, tc.want, resp.StatusCode, "%s", raw)
			if tc.want != http.StatusNoContent {
				require.Zero(t, credentials.replaceCalls)
			}
		})
	}
}

// Contract: selecting daemon-held signing secrets during replica creation
// requires the same owner authority as configure-signing, before the daemon
// reveals whether a referenced source exists.
func TestCreateFederationReplicaSigningReferencesRequireOwnerAuthority(t *testing.T) {
	t.Setenv("TEST_PRESENT_SIGNING_KEY", strings.Repeat("k", 64))
	t.Setenv("TEST_ABSENT_SIGNING_KEY", "")
	for _, tc := range []struct {
		name    string
		headers map[string]string
		setup   func(*daemon.ServerConfig)
	}{
		{
			name:    "identity user token",
			headers: bearer("user-token"),
			setup: func(cfg *daemon.ServerConfig) {
				cfg.Auth.Token = "admin-token"
				cfg.Auth.RequireTokenIdentity = true
			},
		},
		{
			name: "unauthenticated private-network writer",
			setup: func(cfg *daemon.ServerConfig) {
				cfg.Auth.AllowUnauthenticatedPrivateNetworkWrites = true
			},
		},
	} {
		for _, keyEnv := range []string{"TEST_PRESENT_SIGNING_KEY", "TEST_ABSENT_SIGNING_KEY"} {
			t.Run(tc.name+"/"+keyEnv, func(t *testing.T) {
				credentials := newReplicaCredentialStore()
				env := testenv.New(t, func(cfg *daemon.ServerConfig) {
					cfg.FederationCredentials = credentials
					tc.setup(cfg)
				})
				if tc.headers != nil {
					_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
						PlaintextToken: "user-token", Actor: "example-actor", AdminActor: db.BootstrapActor,
					})
					require.NoError(t, err)
				}
				resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
					"hub_url": "https://hub.example", "hub_project_id": 42,
					"hub_project_uid": replicaHubProjectUID, "project_name": "spoke-project",
					"replay_horizon_event_id": 1, "token": "enrollment-secret",
					"signing_key_id": "key-a", "signing_key_env": keyEnv,
				}, tc.headers)
				assertAPIError(t, resp.StatusCode, raw, http.StatusNotFound, "not_found")
				require.Zero(t, credentials.storeCalls)
			})
		}
	}
}

func TestConfigureFederationSigningRejectsBrowserAuthority(t *testing.T) {
	credentials := newReplicaCredentialStore()
	current := config.FederationCredential{HubURL: "https://hub.example", Token: "keep-token"}
	credentials.put(replicaHubProjectUID, current)
	d := openTestDB(t)
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: "http://127.0.0.1:27123", InstanceID: "exampleinstance", Writable: true,
	})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, WebSessions: manager, FederationCredentials: credentials,
	})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler, err := server.HandlerFor(daemon.ListenerPolicy{
		Kind: daemon.ListenerBrowser, Origin: manager.Origin(), RequireBrowserSession: true,
	})
	require.NoError(t, err)
	for _, kind := range []daemon.PrincipalKind{daemon.PrincipalWebLocal, daemon.PrincipalStaticToken} {
		t.Run(string(kind), func(t *testing.T) {
			issued, err := manager.IssueSession(daemon.Principal{Kind: kind}, "/kata")
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, manager.Origin()+configureSigningPath,
				strings.NewReader(`{"key_id":"key-a","key_env":"TEST_DAEMON_SIGNING_KEY"}`))
			request.Header.Set("Origin", manager.Origin())
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Kata-Web-Session", issued.Session)
			request.Header.Set("X-Kata-CSRF", issued.CSRF)
			request.AddCookie(manager.Cookie(issued.Cookie))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusForbidden, response.Code, "%s", response.Body.String())
			require.Zero(t, credentials.replaceCalls)
		})
	}
}
