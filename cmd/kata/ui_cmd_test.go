package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/web"
)

func TestUICommandRejectsCompilationStub(t *testing.T) {
	oldPrepare := prepareWebUIForCommand
	oldValidate := validateWebUIAssetsForCommand
	prepareWebUIForCommand = func(context.Context, client.PrepareWebUIOptions) (client.PreparedWebUI, error) {
		return client.PreparedWebUI{}, nil
	}
	validateWebUIAssetsForCommand = web.ValidateEmbeddedRelease
	t.Cleanup(func() {
		prepareWebUIForCommand = oldPrepare
		validateWebUIAssetsForCommand = oldValidate
	})

	_, err := runCmdOutput(t, nil, "ui")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release binary")
}

func TestUICommandNoArgOpensInbox(t *testing.T) {
	stubUICommandLaunch(t, client.PreparedWebUI{
		BaseURL:          "https://daemon.example",
		ConfiguredRemote: true,
	})

	_, err := runCmdOutput(t, nil, "ui")
	require.NoError(t, err)
	assert.Equal(t, "/kata", openedUIReturnPath)
	assert.Contains(t, openedUIURL, "https://daemon.example/kata#")
}

func TestUICommandBrowserFailurePrintsReachableURL(t *testing.T) {
	handler, err := web.NewEmbeddedHandler()
	require.NoError(t, err)
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.WebHandler = handler
	})
	for name, openerErr := range map[string]error{
		"missing": &exec.Error{Name: "xdg-open", Err: exec.ErrNotFound},
		"failed":  errors.New("exit status 3"),
	} {
		t.Run(name, func(t *testing.T) {
			stubUICommandLaunch(t, client.PreparedWebUI{
				BaseURL: env.URL, Client: env.HTTP,
				Runtime: client.DiscoveredWebRuntime{Origin: env.URL, OriginStable: true, Capabilities: []string{"loopback", "sse"}},
			})
			openWebBrowserForCommand = func(context.Context, client.WebUILaunch) error { return openerErr }
			stdout, stderr, err := runCmdCapture(t, nil, "ui")
			require.NoError(t, err)
			assert.Equal(t, "Could not open a browser: "+openerErr.Error()+". Open the URL above.\n", stderr)
			printedURL := strings.TrimSpace(strings.TrimPrefix(stdout, "Opening "))
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, printedURL, nil)
			require.NoError(t, err)
			request.Header.Set("Accept", "text/html")
			response, err := env.HTTP.Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			assert.Equal(t, http.StatusOK, response.StatusCode)
		})
	}
}

func TestUICommandOutputModesBeforeBrowser(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"json", []string{"--json"}, "{\"kata_api_version\":1,\"url\":\"https://daemon.example/kata#login=1&return_path=%2Fkata\"}\n"},
		{"agent", []string{"--agent"}, "OK ui url=https://daemon.example/kata#login=1&return_path=%2Fkata\n"},
		{"human", nil, "Opening https://daemon.example/kata#login=1&return_path=%2Fkata\n"},
		{"quiet", []string{"-q"}, "Opening https://daemon.example/kata#login=1&return_path=%2Fkata\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubUICommandLaunch(t, client.PreparedWebUI{BaseURL: "https://daemon.example", ConfiguredRemote: true})
			resetFlags(t)
			cmd := newRootCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"ui"}, tc.args...))
			cmd.SetContext(t.Context())
			openWebBrowserForCommand = func(_ context.Context, launch client.WebUILaunch) error {
				assert.Equal(t, "https://daemon.example/kata#login=1&return_path=%2Fkata", launch.PublicURL)
				assert.Equal(t, tc.want, stdout.String())
				return nil
			}
			require.NoError(t, cmd.Execute())
		})
	}
}

func TestUICommandPropagatesCancellation(t *testing.T) {
	stubUICommandLaunch(t, client.PreparedWebUI{BaseURL: "https://daemon.example", ConfiguredRemote: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	openWebBrowserForCommand = func(context.Context, client.WebUILaunch) error {
		cancel()
		return errors.New("signal: killed")
	}
	_, _, err := executeRootCapture(t, ctx, "ui")
	require.ErrorIs(t, err, context.Canceled)
}

func TestUICommandOutputFailurePreventsBrowser(t *testing.T) {
	stubUICommandLaunch(t, client.PreparedWebUI{BaseURL: "https://daemon.example", ConfiguredRemote: true})
	openWebBrowserForCommand = func(context.Context, client.WebUILaunch) error {
		t.Fatal("browser launched after output failure")
		return nil
	}
	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetOut(extraFailWriter{})
	cmd.SetArgs([]string{"ui"})
	cmd.SetContext(t.Context())
	require.ErrorIs(t, cmd.Execute(), io.ErrClosedPipe)
}

func TestUICommandResolvesRefsToCanonicalUIDRoutes(t *testing.T) {
	env := testenv.New(t)
	workspace, _ := initLocalBoundWorkspace(t, env, "example-project")
	created := createIssueViaHTTPFull(t, env, workspace, "Open in browser")

	stubUICommandLaunch(t, client.PreparedWebUI{
		BaseURL:          env.URL,
		ConfiguredRemote: true,
		Client:           env.HTTP,
	})

	for name, args := range map[string][]string{
		"bare":      {"--workspace", workspace, "ui", created.ShortID},
		"qualified": {"ui", "example-project#" + created.ShortID},
		"full":      {"--workspace", workspace, "ui", created.UID},
	} {
		t.Run(name, func(t *testing.T) {
			openedUIReturnPath = ""
			out, err := runCmdOutput(t, nil, args...)
			require.NoError(t, err)
			assert.Equal(t, "/kata?scope="+created.ProjectUID+"&issue="+created.UID, openedUIReturnPath)
			assert.Equal(t, "Opening "+openedUIURL+"\n", out)
		})
	}
}

func TestUICommandRefusesUnresolvedOrAmbiguousBeforeOpen(t *testing.T) {
	env := testenv.New(t)
	stubUICommandLaunch(t, client.PreparedWebUI{
		BaseURL:          env.URL,
		ConfiguredRemote: true,
		Client:           env.HTTP,
	})

	_, err := runCmdOutput(t, nil, "--workspace", t.TempDir(), "ui", "abc4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no project bound")
	assert.Empty(t, openedUIURL)

	_, err = runCmdOutput(t, nil, "ui", "example-project#missing")
	require.Error(t, err)
	assert.Empty(t, openedUIURL)
}

func TestUICommandResolvesBeforeOpening(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/resolve":
			calls = append(calls, "resolve")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 7, "name": "example-project"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/ui/issue-reference" &&
			r.URL.Query().Get("project_id") == "7" && r.URL.Query().Get("ref") == "abc4":
			calls = append(calls, "issue")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"uid": "01HZNQ7VFPK1XGD8R5MABCD4EX", "project_uid": "01HZNQ7VFPK1XGD8R5MABCD4EY",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	stubUICommandLaunch(t, client.PreparedWebUI{
		BaseURL:          server.URL,
		ConfiguredRemote: true,
		Client:           server.Client(),
		Runtime: client.DiscoveredWebRuntime{
			Origin: server.URL, OriginStable: true,
			Capabilities: []string{"loopback", "sse"},
		},
	})
	_, err := runCmdOutput(t, nil, "--project", "example-project", "ui", "abc4")
	require.NoError(t, err)
	assert.Equal(t, []string{"resolve", "issue"}, calls)
	assert.Equal(t,
		"/kata?scope=01HZNQ7VFPK1XGD8R5MABCD4EY&issue=01HZNQ7VFPK1XGD8R5MABCD4EX",
		openedUIReturnPath)
}

func TestUICommandResolvesRefsThroughLocalDaemonGateway(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "example-selected", r.Header.Get("X-Kata-Web-Daemon"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/ui/proxy/api/v1/projects/resolve":
			calls = append(calls, "resolve")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 7, "name": "example-project"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/ui/proxy/api/v1/ui/issue-reference" &&
			r.URL.Query().Get("project_id") == "7" && r.URL.Query().Get("ref") == "abc4":
			calls = append(calls, "issue")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"uid": "01HZNQ7VFPK1XGD8R5MABCD4EX", "project_uid": "01HZNQ7VFPK1XGD8R5MABCD4EY",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	stubUICommandLaunch(t, client.PreparedWebUI{
		BaseURL:    server.URL,
		Client:     server.Client(),
		DaemonName: "example-selected",
		Runtime: client.DiscoveredWebRuntime{
			Origin: server.URL, OriginStable: true,
			Capabilities: []string{"loopback", "sse"},
		},
	})
	_, err := runCmdOutput(t, nil, "--project", "example-project", "ui", "abc4")
	require.NoError(t, err)
	assert.Equal(t, []string{"resolve", "issue"}, calls)
}

var (
	openedUIURL        string
	openedUIReturnPath string
)

func stubUICommandLaunch(t *testing.T, prepared client.PreparedWebUI) {
	t.Helper()
	if prepared.ConfiguredRemote && prepared.AnonymousClient == nil {
		prepared.AnonymousClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("X-Kata-Web-Origin", prepared.BaseURL)
			header.Set("X-Kata-Web-Authentication", "login")
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       http.NoBody,
				Header:     header,
			}, nil
		})}
	}
	oldPrepare := prepareWebUIForCommand
	oldOpen := openWebUIForCommand
	oldBrowser := openWebBrowserForCommand
	oldValidate := validateWebUIAssetsForCommand
	validateWebUIAssetsForCommand = func() error { return nil }
	prepareWebUIForCommand = func(context.Context, client.PrepareWebUIOptions) (client.PreparedWebUI, error) {
		return prepared, nil
	}
	openWebUIForCommand = func(ctx context.Context, target client.PreparedWebUI, returnPath string, opener client.WebUIOpener) error {
		openedUIReturnPath = returnPath
		return client.OpenWebUI(ctx, target, returnPath, opener)
	}
	openWebBrowserForCommand = func(_ context.Context, target client.WebUILaunch) error {
		openedUIURL = target.PublicURL
		return nil
	}
	openedUIURL = ""
	openedUIReturnPath = ""
	t.Cleanup(func() {
		prepareWebUIForCommand = oldPrepare
		openWebUIForCommand = oldOpen
		openWebBrowserForCommand = oldBrowser
		validateWebUIAssetsForCommand = oldValidate
		openedUIURL = ""
		openedUIReturnPath = ""
	})
}
