package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
	"pgregory.net/rapid"
)

const unboundRemoteProjectMessage = `no .kata.toml ancestor and no git ancestor — run "kata init" or pass --project`

type projectResolutionRequest struct {
	path          string
	authorization string
	body          map[string]any
}

type projectResolutionRecorder struct {
	mu       sync.Mutex
	requests []projectResolutionRequest
}

func (r *projectResolutionRecorder) take() []projectResolutionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	requests := r.requests
	r.requests = nil
	return requests
}

// Substitute only legacy paths to model separate client/daemon filesystems.
// Requests still run through the real daemon's authentication and storage.
func projectResolutionProxy(t *testing.T, env *testenv.Env, daemonPath string) (*httptest.Server, *projectResolutionRecorder) {
	t.Helper()
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	recorder := &projectResolutionRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects") {
			var body map[string]any
			if r.Body != nil {
				data, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					http.Error(w, readErr.Error(), http.StatusBadRequest)
					return
				}
				if len(data) > 0 {
					if err := json.Unmarshal(data, &body); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
				}
				recorder.mu.Lock()
				recorder.requests = append(recorder.requests, projectResolutionRequest{r.URL.Path, r.Header.Get("Authorization"), maps.Clone(body)})
				recorder.mu.Unlock()
				if daemonPath != "" && r.URL.Path == "/api/v1/projects/resolve" && body["start_path"] != nil {
					body["start_path"] = daemonPath
					encoded, marshalErr := json.Marshal(body)
					if marshalErr != nil {
						http.Error(w, marshalErr.Error(), http.StatusBadRequest)
						return
					}
					data = encoded
				}
				r.Body = io.NopCloser(bytes.NewReader(data))
				r.ContentLength = int64(len(data))
			} else {
				recorder.mu.Lock()
				recorder.requests = append(recorder.requests, projectResolutionRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization")})
				recorder.mu.Unlock()
			}
		}
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

func selectProjectResolutionRemote(t *testing.T, env *testenv.Env, workspace, endpoint, source string) []string {
	t.Helper()
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "fixture-token")
	switch source {
	case "environment":
		t.Setenv("KATA_SERVER", endpoint)
	case "workspace":
		require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
			[]byte(fmt.Sprintf("version = 1\n[server]\nurl = %q\n", endpoint)), 0o600))
	case "active", "named":
		// The catalog credential must win over a configured local token.
		// An explicit KATA_AUTH_TOKEN is a deliberate override, so clear it.
		t.Setenv("KATA_AUTH_TOKEN", "")
		catalog := fmt.Sprintf("[auth]\ntoken = \"unrelated-token\"\n[[daemon]]\nname = \"example-remote\"\nurl = %q\ntoken = \"fixture-token\"\n", endpoint)
		if source == "active" {
			catalog = "active_daemon = \"example-remote\"\n" + catalog
		}
		require.NoError(t, os.WriteFile(filepath.Join(env.Home, "config.toml"), []byte(catalog), 0o600))
		if source == "named" {
			return []string{"--daemon", "example-remote"}
		}
	default:
		t.Fatalf("unknown remote source %q", source)
	}
	return nil
}

func TestRemoteProjectResolutionWithoutBinding(t *testing.T) {
	for _, source := range []string{"environment", "active", "named"} {
		t.Run(source, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
			_, err := env.DB.CreateProject(t.Context(), "daemon-project")
			require.NoError(t, err)
			bound := t.TempDir()
			testfix.WriteKataToml(t, bound, "daemon-project")
			for _, daemonState := range []string{"missing", "bound"} {
				t.Run(daemonState, func(t *testing.T) {
					daemonPath := bound
					if daemonState == "missing" {
						daemonPath = filepath.Join(t.TempDir(), "missing-daemon-directory")
					}
					server, recorder := projectResolutionProxy(t, env, daemonPath)
					workspace := t.TempDir()
					t.Chdir(workspace)
					selection := selectProjectResolutionRemote(t, env, workspace, server.URL, source)
					for _, missing := range []bool{false, true} {
						for _, command := range [][]string{{"list"}, {"search", "example"}, {"create", "Example task"}} {
							for _, mode := range []string{"--agent", "--json"} {
								t.Run(fmt.Sprintf("%s/%s/missing=%t", command[0], mode, missing), func(t *testing.T) {
									args := append(append([]string{}, command...), selection...)
									args = append(args, mode)
									if missing {
										args = append(args, "--workspace", filepath.Join(workspace, "missing-client-directory"))
									}
									_, stderr, err := executeRootCapture(t, t.Context(), args...)
									requests := recorder.take()
									ce := requireCLIError(t, err, ExitNotFound)
									assert.Equal(t, kindNotFound, ce.Kind)
									assert.Equal(t, "project_not_initialized", ce.Code)
									assert.Equal(t, unboundRemoteProjectMessage, ce.Message)
									assert.Empty(t, requests, "client paths must never reach the remote project API")
									if mode == "--json" {
										envelope := parseErrorEnvelope(t, []byte(stderr))
										assert.Equal(t, "project_not_initialized", envelope.Error.Code)
										assert.Equal(t, ExitNotFound, envelope.Error.ExitCode)
										assert.Equal(t, unboundRemoteProjectMessage, envelope.Error.Message)
									} else {
										assert.Contains(t, stderr, "ERR "+command[0]+" not_found: "+unboundRemoteProjectMessage)
									}
								})
							}
						}
					}
				})
			}
		})
	}
}

func TestRemoteProjectResolutionUnboundDescendants(t *testing.T) {
	resetFlags(t)
	root := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) {
		parts := rapid.SliceOf(rapid.Uint64()).Draw(rt, "path components")
		path := root
		// Bound filesystem work at materialization; the generated domain stays whole.
		for i, part := range parts {
			if i == 8 {
				break
			}
			path = filepath.Join(path, strconv.FormatUint(part, 10))
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			rt.Fatal(err)
		}
		if rapid.Bool().Draw(rt, "missing") {
			path = filepath.Join(path, "missing")
		}
		source := rapid.SampledFrom([]client.DaemonSource{
			client.DaemonSourceServerEnv, client.DaemonSourceLocalConfig, client.DaemonSourceActiveDaemon,
		}).Draw(rt, "remote source")
		ctx := context.WithValue(t.Context(), resolvedDaemonContextKey{}, client.ResolvedDaemon{Source: source})
		body, repair, err := buildResolveRequest(ctx, path)
		var ce *cliError
		if !errors.As(err, &ce) || ce.Code != "project_not_initialized" || ce.Kind != kindNotFound ||
			ce.ExitCode != ExitNotFound || ce.Message != unboundRemoteProjectMessage || body != nil || repair != nil {
			rt.Fatalf("unbound remote path %q: body=%v repair=%t err=%v", path, body, repair != nil, err)
		}
	})
}

func TestRemoteProjectResolutionSelectorsStayPathFree(t *testing.T) {
	for _, source := range []string{"environment", "workspace", "active", "named"} {
		for _, selector := range []string{"explicit", "binding", "git"} {
			t.Run(source+"/"+selector, func(t *testing.T) {
				env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
				project, err := env.DB.CreateProject(t.Context(), "example-project")
				require.NoError(t, err)
				issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Author: "fixture-actor", Title: "Example task",
				})
				require.NoError(t, err)
				workspace := t.TempDir()
				t.Chdir(workspace)
				args := []string{"list", "--json"}
				switch selector {
				case "explicit":
					args = append(args, "--project", project.Name, "--workspace", filepath.Join(workspace, "missing"))
					// Anchor a workspace URL while proving --project bypasses
					// both the missing path and an unrelated local binding.
					if source == "workspace" {
						testfix.WriteKataToml(t, workspace, "other-project")
					}
				case "binding":
					testfix.WriteKataToml(t, workspace, project.Name)
				case "git":
					runGit(t, workspace, "init", "--quiet")
					runGit(t, workspace, "remote", "add", "origin", "https://github.com/example-org/example-project.git")
					_, err = env.DB.AttachAlias(t.Context(), project.ID, "github.com/example-org/example-project", "git")
					require.NoError(t, err)
				}
				server, recorder := projectResolutionProxy(t, env, "")
				args = append(args, selectProjectResolutionRemote(t, env, workspace, server.URL, source)...)
				stdout, stderr, err := executeRootCapture(t, t.Context(), args...)
				require.NoError(t, err, stderr)
				var response struct {
					Issues []db.Issue `json:"issues"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &response))
				require.Len(t, response.Issues, 1)
				assert.Equal(t, issue.UID, response.Issues[0].UID)
				requests := recorder.take()
				require.Len(t, requests, 2)
				assert.Equal(t, "/api/v1/projects/resolve", requests[0].path)
				assert.NotContains(t, requests[0].body, "start_path")
				assert.Equal(t, fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), requests[1].path)
				for _, request := range requests {
					assert.Equal(t, "Bearer fixture-token", request.authorization)
				}
				if selector == "explicit" {
					assert.Equal(t, map[string]any{"name": project.Name}, requests[0].body)
				}
			})
		}
	}
}

func TestRemoteProjectResolutionWorkspaceURLMissingPath(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
	workspace := t.TempDir()
	testfix.WriteKataToml(t, workspace, "example-project")
	t.Chdir(workspace)
	server, recorder := projectResolutionProxy(t, env, "")
	selectProjectResolutionRemote(t, env, workspace, server.URL, "workspace")
	for _, command := range [][]string{{"list"}, {"search", "example"}, {"create", "Example task"}} {
		args := append(append([]string{}, command...), "--json", "--workspace", filepath.Join(workspace, "missing"))
		_, _, err := executeRootCapture(t, t.Context(), args...)
		ce := requireCLIError(t, err, ExitNotFound)
		assert.Equal(t, "project_not_initialized", ce.Code)
		assert.Equal(t, unboundRemoteProjectMessage, ce.Message)
		assert.Empty(t, recorder.take())
	}
}

func TestRemoteProjectResolutionMalformedBinding(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version = ["), 0o600))
	t.Chdir(workspace)
	server, recorder := projectResolutionProxy(t, env, "")
	selectProjectResolutionRemote(t, env, workspace, server.URL, "environment")
	_, _, err := executeRootCapture(t, t.Context(), "list", "--json")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, ce.Message, "read .kata.toml:")
	assert.Empty(t, recorder.take())
}

func TestRemoteMCPProjectResolutionWithoutBinding(t *testing.T) {
	for _, source := range []string{"environment", "workspace", "active", "named"} {
		t.Run(source, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
			workspace := t.TempDir()
			if source == "workspace" {
				testfix.WriteKataToml(t, workspace, "example-project")
			}
			t.Chdir(workspace)
			server, recorder := projectResolutionProxy(t, env, filepath.Join(t.TempDir(), "missing"))
			selection := selectProjectResolutionRemote(t, env, workspace, server.URL, source)
			for _, missing := range []bool{false, true} {
				if source == "workspace" && !missing {
					// Workspace URLs require an ancestor binding; only the
					// missing descendant exercises its path fallback.
					continue
				}
				args := append([]string{"mcp", "serve", "--json"}, selection...)
				if missing {
					args = append(args, "--workspace", filepath.Join(workspace, "missing"))
				}
				_, _, err := executeRootCapture(t, t.Context(), args...)
				requests := recorder.take()
				ce := requireCLIError(t, err, ExitNotFound)
				assert.Equal(t, "project_not_initialized", ce.Code)
				assert.Equal(t, unboundRemoteProjectMessage, ce.Message)
				assert.Empty(t, requests, "MCP scope resolution must not send client paths")
			}
		})
	}
}

func TestRemoteFederationProjectResolutionWithoutBinding(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
	workspace := t.TempDir()
	t.Chdir(workspace)
	t.Setenv("EXAMPLE_HUB_TOKEN", "fixture-token")
	server, recorder := projectResolutionProxy(t, env, filepath.Join(t.TempDir(), "missing"))
	for _, missing := range []bool{false, true} {
		args := []string{"federation", "enroll", "--json", "--hub-url", server.URL,
			"--spoke-instance", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--capabilities", "pull",
			"--hub-token-env", "EXAMPLE_HUB_TOKEN"}
		if missing {
			args = append(args, "--workspace", filepath.Join(workspace, "missing"))
		}
		_, _, err := executeRootCapture(t, t.Context(), args...)
		requests := recorder.take()
		ce := requireCLIError(t, err, ExitNotFound)
		assert.Equal(t, "project_not_initialized", ce.Code)
		assert.Equal(t, unboundRemoteProjectMessage, ce.Message)
		assert.Empty(t, requests, "hub scope resolution must not send client paths")
	}
}

func TestLocalProjectResolutionKeepsPathFallback(t *testing.T) {
	env := testenv.New(t)
	resetFlags(t)
	for _, tc := range []struct {
		name     string
		resolved client.ResolvedDaemon
	}{
		{"injected", client.ResolvedDaemon{Source: client.DaemonSourceInjected}},
		{"local runtime", client.ResolvedDaemon{Source: client.DaemonSourceLocalRuntime}},
		{"local catalog", client.ResolvedDaemon{Source: client.DaemonSourceNamedCatalog}},
		{"local profile", client.ResolvedDaemon{Source: client.DaemonSourceLocalConfig, LocalProfile: &client.LocalProfileIdentity{Home: env.Home}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), resolvedDaemonContextKey{}, tc.resolved)
			a := daemonAPI{ctx: ctx, baseURL: env.URL, client: env.HTTP, resolved: tc.resolved}
			for _, missing := range []bool{false, true} {
				path := t.TempDir()
				if missing {
					path = filepath.Join(path, "missing")
				}
				body, repair, err := buildResolveRequest(ctx, path)
				require.NoError(t, err)
				assert.Nil(t, repair)
				assert.Equal(t, map[string]any{"start_path": path}, body)
				_, _, err = resolveProjectIDAndNameWithClient(a, path)
				if missing {
					ce := requireCLIError(t, err, ExitValidation)
					assert.Equal(t, "validation", ce.Code)
					assert.Contains(t, ce.Message, path)
				} else {
					ce := requireCLIError(t, err, ExitNotFound)
					assert.Equal(t, "project_not_initialized", ce.Code)
					assert.Equal(t, "no .kata.toml ancestor and no git ancestor", ce.Message)
				}
			}
		})
	}
}
