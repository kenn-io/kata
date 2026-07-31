package federation_test

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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

const (
	testCatalogBearer = "catalog-bearer-secret"
	testEnrollment    = "enrollment-secret"
)

func TestHubClientPreservesCatalogPathPrefix(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		writeJSON(t, w, http.StatusOK, map[string]any{
			"project": map[string]any{
				"id": 42, "uid": hubProjectUID, "name": "hub-project",
			},
		})
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: server.URL + "/reverse-proxy", Token: testCatalogBearer,
		AllowInsecure: true,
	})
	require.NoError(t, err)

	_, err = hub.ResolveProject(context.Background(), "hub-project")
	require.NoError(t, err)
	assert.Equal(t, "/reverse-proxy/api/v1/projects/resolve", gotPath)
}

func TestHubClientLiteralTokenResolvesCreatesEnablesAndEnrolls(t *testing.T) {
	unsetHubClientGlobalAuth(t)

	var (
		mu       sync.Mutex
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		assert.Equal(t, "Bearer "+testCatalogBearer, r.Header.Get("Authorization"))

		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/projects/resolve":
			var body struct {
				Name string `json:"name"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "hub-project", body.Name)
			http.Error(w, "not found", http.StatusNotFound)
		case "POST /api/v1/projects":
			var body struct {
				Name string `json:"name"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "hub-project", body.Name)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"project": map[string]any{
					"id": 42, "uid": "01HZNQ7VFPK1XGD8R5MABCD4EX", "name": "hub-project",
				},
				"created": true,
			})
		case "POST /api/v1/projects/42/federation/enable":
			var body struct {
				Actor string `json:"actor"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "user-a", body.Actor)
			writeHubProject(t, w)
		case "POST /api/v1/federation/enrollments":
			assertEnrollmentRequest(t, r)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"id": 71, "actor": "identity-user",
			})
		case "POST /api/v1/federation/enrollments/actions/rotate":
			assertEnrollmentRequest(t, r)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"id": 72, "actor": "identity-user",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: server.URL, Token: testCatalogBearer, AllowInsecure: true,
	})
	require.NoError(t, err)

	project, err := hub.EnsureProject(context.Background(), "hub-project", "user-a")
	require.NoError(t, err)
	assert.Equal(t, federation.HubProject{
		ID: 42, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "hub-project",
		ReplayHorizonEventID: 9, BaselineThroughEventID: 6,
	}, project)

	enrollmentRequest := federation.EnrollmentRequest{
		ProjectID:                    42,
		SpokeInstanceUID:             "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Token:                        testEnrollment,
		Capabilities:                 "claim,pull,push",
		Actor:                        "user-a",
		AllowAdoptionSnapshotAuthors: true,
	}
	created, err := hub.EnsureEnrollment(context.Background(), enrollmentRequest)
	require.NoError(t, err)
	assert.Equal(t, federation.Enrollment{ID: 71, Actor: "identity-user"}, created)

	rotated, err := hub.RotateEnrollment(context.Background(), enrollmentRequest)
	require.NoError(t, err)
	assert.Equal(t, federation.Enrollment{ID: 72, Actor: "identity-user"}, rotated)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{
		"POST /api/v1/projects/resolve",
		"POST /api/v1/projects",
		"POST /api/v1/projects/42/federation/enable",
		"POST /api/v1/federation/enrollments",
		"POST /api/v1/federation/enrollments/actions/rotate",
	}, requests)
}

func TestHubClientEnsureProjectPreservesResolvedIdentity(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	const (
		requestedAlias = "hub-alias"
		canonicalName  = "hub-project"
		responseMarker = "raw-response-marker"
	)
	tests := []struct {
		name        string
		enabledID   int64
		enabledUID  string
		enabledName string
		wantErr     bool
	}{
		{
			name:        "canonical resolved identity",
			enabledID:   42,
			enabledUID:  hubProjectUID,
			enabledName: canonicalName,
		},
		{
			name:        "different numeric id",
			enabledID:   43,
			enabledUID:  hubProjectUID,
			enabledName: canonicalName,
			wantErr:     true,
		},
		{
			name:        "different uid",
			enabledID:   42,
			enabledUID:  recreatedProjectUID,
			enabledName: canonicalName,
			wantErr:     true,
		},
		{
			name:        "different canonical name",
			enabledID:   42,
			enabledUID:  hubProjectUID,
			enabledName: responseMarker,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects/resolve":
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project": map[string]any{
							"id": 42, "uid": hubProjectUID, "name": canonicalName,
						},
					})
				case "/api/v1/projects/42/federation/enable":
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project_id": tt.enabledID, "project_uid": tt.enabledUID,
						"project_name": tt.enabledName, "replay_horizon_event_id": 9,
						"baseline_through_event_id": 6,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: server.URL, AllowInsecure: true,
			})
			require.NoError(t, err)

			project, err := hub.EnsureProject(context.Background(), requestedAlias, "user-a")
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, federation.ErrHubValidation)
				assert.Empty(t, project)
				assert.NotContains(t, err.Error(), responseMarker)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, federation.HubProject{
				ID: 42, UID: hubProjectUID, Name: canonicalName,
				ReplayHorizonEventID: 9, BaselineThroughEventID: 6,
			}, project)
		})
	}
}

func TestHubClientResolveProjectIsReadOnly(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"project": map[string]any{
					"id": 42, "uid": hubProjectUID, "name": "hub-project",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: server.URL, AllowInsecure: true,
	})
	require.NoError(t, err)

	project, err := hub.ResolveProject(context.Background(), "hub-project")
	require.NoError(t, err)
	assert.Equal(t, federation.HubProject{
		ID: 42, UID: hubProjectUID, Name: "hub-project",
	}, project)
	assert.Equal(t, []string{"POST /api/v1/projects/resolve"}, requests)
}

func TestHubClientEnsureProjectPreservesCreatedIdentity(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	tests := []struct {
		name        string
		enabledUID  string
		enabledName string
		wantErr     bool
	}{
		{name: "created identity", enabledUID: hubProjectUID, enabledName: "hub-project"},
		{
			name: "different uid", enabledUID: recreatedProjectUID,
			enabledName: "hub-project", wantErr: true,
		},
		{
			name: "different name", enabledUID: hubProjectUID,
			enabledName: "different-project", wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects/resolve":
					http.NotFound(w, r)
				case "/api/v1/projects":
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project": map[string]any{
							"id": 42, "uid": hubProjectUID, "name": "hub-project",
						},
					})
				case "/api/v1/projects/42/federation/enable":
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project_id": 42, "project_uid": tt.enabledUID,
						"project_name": tt.enabledName, "replay_horizon_event_id": 9,
						"baseline_through_event_id": 6,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: server.URL, AllowInsecure: true,
			})
			require.NoError(t, err)

			project, err := hub.EnsureProject(context.Background(), "hub-project", "user-a")
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, federation.ErrHubValidation)
				assert.Empty(t, project)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, hubProjectUID, project.UID)
			assert.Equal(t, "hub-project", project.Name)
		})
	}
}

func TestHubClientRejectsMalformedProjectUIDBeforeEnable(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	const malformedUID = "not-a-valid-project-uid"
	tests := []struct {
		name       string
		resolveHit bool
	}{
		{name: "resolved project", resolveHit: true},
		{name: "created project", resolveHit: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enableCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects/resolve":
					if !tt.resolveHit {
						http.NotFound(w, r)
						return
					}
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project": map[string]any{
							"id": 42, "uid": malformedUID, "name": "hub-project",
						},
					})
				case "/api/v1/projects":
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project": map[string]any{
							"id": 42, "uid": malformedUID, "name": "hub-project",
						},
					})
				case "/api/v1/projects/42/federation/enable":
					enableCalls++
					writeJSON(t, w, http.StatusOK, map[string]any{
						"project_id": 42, "project_uid": malformedUID,
						"project_name": "hub-project", "replay_horizon_event_id": 9,
						"baseline_through_event_id": 6,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: server.URL, Token: testCatalogBearer, AllowInsecure: true,
			})
			require.NoError(t, err)

			project, err := hub.EnsureProject(context.Background(), "hub-project", "user-a")
			require.Error(t, err)
			assert.ErrorIs(t, err, federation.ErrHubValidation)
			assert.Empty(t, project)
			assert.Zero(t, enableCalls)
			assert.NotContains(t, err.Error(), malformedUID)
			assert.NotContains(t, err.Error(), testCatalogBearer)
		})
	}
}

func TestHubClientRejectsInvalidEnrollmentActor(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	const responseMarker = "raw-enrollment-response-marker"
	tests := []struct {
		name  string
		actor string
	}{
		{name: "empty", actor: "   "},
		{name: "reserved", actor: strings.ToUpper(db.BootstrapActor)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"id": 71, "actor": tt.actor,
					"detail": responseMarker + testEnrollment,
				})
			}))
			t.Cleanup(server.Close)

			hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: server.URL, Token: testCatalogBearer, AllowInsecure: true,
			})
			require.NoError(t, err)

			enrollment, err := hub.EnsureEnrollment(
				context.Background(),
				federation.EnrollmentRequest{
					ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
					Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
				},
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, federation.ErrHubValidation)
			assert.Empty(t, enrollment)
			assert.NotContains(t, err.Error(), responseMarker)
			assert.NotContains(t, err.Error(), testEnrollment)
			assert.NotContains(t, err.Error(), testCatalogBearer)
			if actor := strings.TrimSpace(tt.actor); actor != "" {
				assert.NotContains(t, err.Error(), actor)
			}
		})
	}
}

func TestHubClientTokenEnvUsesOnlySelectedCatalogCredential(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	t.Setenv("KATA_PRIMARY_HUB_TOKEN", testCatalogBearer)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": 71, "actor": "identity-user",
		})
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{ //nolint:gosec // TokenEnv is an environment variable name, not credential material.
		Name: "primary", URL: server.URL, TokenEnv: "KATA_PRIMARY_HUB_TOKEN", AllowInsecure: true,
	})
	require.NoError(t, err)

	_, err = hub.EnsureEnrollment(context.Background(), federation.EnrollmentRequest{
		ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+testCatalogBearer, authorization)
}

func TestHubClientEmptyTokenEnvIsClassifiedBeforeHTTPWithoutGlobalFallback(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	t.Setenv("KATA_AUTH_TOKEN", "global-env-secret")
	t.Setenv("KATA_PRIMARY_HUB_TOKEN", "   ")
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntoken = \"global-config-secret\"\n"), 0o600))

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{ //nolint:gosec // TokenEnv is an environment variable name, not credential material.
		Name: "primary", URL: server.URL, TokenEnv: "KATA_PRIMARY_HUB_TOKEN", AllowInsecure: true,
	})
	assert.Nil(t, hub)
	require.Error(t, err)
	assert.ErrorIs(t, err, federation.ErrHubAuthentication)
	assert.Zero(t, requests)
	assert.NotContains(t, err.Error(), "global-env-secret")
	assert.NotContains(t, err.Error(), "global-config-secret")
	assert.NotContains(t, err.Error(), testCatalogBearer)
}

func TestHubClientUnsetTokenEnvIsClassifiedBeforeHTTP(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	require.NoError(t, os.Unsetenv("KATA_PRIMARY_HUB_TOKEN"))

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{ //nolint:gosec // TokenEnv is an environment variable name, not credential material.
		Name: "primary", URL: server.URL, TokenEnv: "KATA_PRIMARY_HUB_TOKEN", AllowInsecure: true,
	})
	assert.Nil(t, hub)
	require.Error(t, err)
	assert.ErrorIs(t, err, federation.ErrHubAuthentication)
	assert.Zero(t, requests)
}

func TestHubClientCatalogBearerNeverCrossesRedirectOrigin(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	var (
		targetRequests int
		targetAuth     string
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		targetAuth = r.Header.Get("Authorization")
		writeHubProject(t, w)
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // test server intentionally redirects only to another local test server.
	}))
	t.Cleanup(source.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: source.URL, Token: testCatalogBearer, AllowInsecure: true,
	})
	require.NoError(t, err)

	_, err = hub.EnsureEnrollment(context.Background(), federation.EnrollmentRequest{
		ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, federation.ErrHubUnavailable)
	assert.Zero(t, targetRequests)
	assert.Empty(t, targetAuth)
	assert.NotContains(t, err.Error(), testCatalogBearer)
	assert.NotContains(t, err.Error(), testEnrollment)
}

func TestHubClientTokenlessEnrollmentNeverCrossesRedirectOrigin(t *testing.T) {
	unsetHubClientGlobalAuth(t)

	for _, status := range []int{
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var (
				mu             sync.Mutex
				targetRequests int
				targetBodies   []string
			)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				mu.Lock()
				targetRequests++
				targetBodies = append(targetBodies, string(body))
				mu.Unlock()
				writeJSON(t, w, http.StatusOK, map[string]any{
					"id": 71, "actor": "identity-user",
				})
			}))
			t.Cleanup(target.Close)

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, status) //nolint:gosec // test server intentionally redirects only to another local test server.
			}))
			t.Cleanup(source.Close)

			hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: source.URL, AllowInsecure: true,
			})
			require.NoError(t, err)

			_, err = hub.EnsureEnrollment(context.Background(), federation.EnrollmentRequest{
				ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, federation.ErrHubUnavailable)
			assert.NotContains(t, err.Error(), testEnrollment)

			mu.Lock()
			defer mu.Unlock()
			assert.Zero(t, targetRequests)
			assert.Empty(t, targetBodies)
		})
	}
}

func TestHubClientFollowsSameOriginEnrollmentRedirect(t *testing.T) {
	unsetHubClientGlobalAuth(t)

	var redirectedBody struct {
		Token string `json:"token"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/federation/enrollments":
			http.Redirect(w, r, "/redirected-enrollment", http.StatusTemporaryRedirect)
		case "/redirected-enrollment":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&redirectedBody))
			writeJSON(t, w, http.StatusOK, map[string]any{
				"id": 71, "actor": "identity-user",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: server.URL, AllowInsecure: true,
	})
	require.NoError(t, err)

	enrollment, err := hub.EnsureEnrollment(
		context.Background(),
		federation.EnrollmentRequest{
			ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
			Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
		},
	)
	require.NoError(t, err)
	assert.Equal(t, federation.Enrollment{ID: 71, Actor: "identity-user"}, enrollment)
	assert.Equal(t, testEnrollment, redirectedBody.Token)
}

func TestHubClientHTTPErrorIsTypedBoundedAndSecretSafe(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(testCatalogBearer + testEnrollment + string(make([]byte, 1<<20))))
	}))
	t.Cleanup(server.Close)

	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: server.URL, Token: testCatalogBearer, AllowInsecure: true,
	})
	require.NoError(t, err)

	_, err = hub.EnsureEnrollment(context.Background(), federation.EnrollmentRequest{
		ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, federation.ErrHubAuthentication)
	var hubErr *federation.HubError
	require.True(t, errors.As(err, &hubErr))
	assert.Equal(t, http.StatusUnauthorized, hubErr.StatusCode)
	assert.Less(t, len(err.Error()), 160)
	assert.NotContains(t, err.Error(), testCatalogBearer)
	assert.NotContains(t, err.Error(), testEnrollment)
}

func TestHubClientHTTPStatusCategories(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	tests := []struct {
		name   string
		status int
		kind   error
	}{
		{name: "bad request", status: http.StatusBadRequest, kind: federation.ErrHubValidation},
		{name: "unauthorized", status: http.StatusUnauthorized, kind: federation.ErrHubAuthentication},
		{name: "forbidden", status: http.StatusForbidden, kind: federation.ErrHubAuthentication},
		{name: "conflict", status: http.StatusConflict, kind: federation.ErrHubValidation},
		{name: "timeout", status: http.StatusRequestTimeout, kind: federation.ErrHubUnavailable},
		{name: "throttled", status: http.StatusTooManyRequests, kind: federation.ErrHubUnavailable},
		{name: "server failure", status: http.StatusServiceUnavailable, kind: federation.ErrHubUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(server.Close)
			hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: server.URL, AllowInsecure: true,
			})
			require.NoError(t, err)

			_, err = hub.EnsureEnrollment(context.Background(), federation.EnrollmentRequest{
				ProjectID: 42, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Token: testEnrollment, Capabilities: "claim,pull,push", Actor: "user-a",
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.kind)
			var hubErr *federation.HubError
			require.True(t, errors.As(err, &hubErr))
			assert.Equal(t, tt.status, hubErr.StatusCode)
		})
	}
}

func TestHubClientRevokesEnrollmentByExactID(t *testing.T) {
	unsetHubClientGlobalAuth(t)
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assert.Equal(t, http.MethodPost, r.Method)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 47, "revoked": true})
	}))
	t.Cleanup(server.Close)
	hub, err := federation.NewHubClient(context.Background(), config.CatalogDaemonConfig{
		Name: "primary", URL: server.URL, AllowInsecure: true,
	})
	require.NoError(t, err)

	require.NoError(t, hub.RevokeEnrollment(context.Background(), 47))
	assert.Equal(t, "/api/v1/federation/enrollments/47/revoke", gotPath)
}

func assertEnrollmentRequest(t *testing.T, r *http.Request) {
	t.Helper()
	var body struct {
		ProjectID                    int64  `json:"project_id"`
		SpokeInstanceUID             string `json:"spoke_instance_uid"`
		Token                        string `json:"token"`
		Capabilities                 string `json:"capabilities"`
		Actor                        string `json:"actor"`
		AllowAdoptionSnapshotAuthors bool   `json:"allow_adoption_snapshot_authors"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
	assert.Equal(t, int64(42), body.ProjectID)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EA", body.SpokeInstanceUID)
	assert.Equal(t, testEnrollment, body.Token)
	assert.Equal(t, "claim,pull,push", body.Capabilities)
	assert.Equal(t, "user-a", body.Actor)
	assert.True(t, body.AllowAdoptionSnapshotAuthors)
}

func writeHubProject(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeJSON(t, w, http.StatusOK, map[string]any{
		"project_id": 42, "project_uid": "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name": "hub-project", "replay_horizon_event_id": 9,
		"baseline_through_event_id": 6,
	})
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

func unsetHubClientGlobalAuth(t *testing.T) {
	t.Helper()
	for _, name := range []string{"KATA_AUTH_TOKEN", "KATA_SERVER"} {
		value, existed := os.LookupEnv(name)
		require.NoError(t, os.Unsetenv(name))
		t.Cleanup(func() {
			if existed {
				require.NoError(t, os.Setenv(name, value))
			} else {
				require.NoError(t, os.Unsetenv(name))
			}
		})
	}
	t.Setenv("KATA_HOME", t.TempDir())
}
