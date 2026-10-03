package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func doctorServerContext(t *testing.T, replies map[string]string, statuses map[string]int) context.Context {
	ts := doctorTestServer(t, replies, statuses)
	return context.WithValue(context.Background(), client.BaseURLKey{}, ts.URL)
}

func doctorTestServer(t *testing.T, replies map[string]string, statuses map[string]int) *httptest.Server {
	t.Helper()
	defaults := map[string]string{
		"/api/v1/ping":     fmt.Sprintf(`{"ok":true,"service":"kata","version":%q}`, version.Version),
		"/api/v1/instance": fmt.Sprintf(`{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01","version":%q,"auth":{"kind":"none"}}`, version.Version),
		"/api/v1/health":   fmt.Sprintf(`{"ok":true,"schema_version":28,"api_schema_version":%q,"version":%q}`, daemon.APISchemaVersion, version.Version),
		"/api/v1/projects": `{"projects":[{"id":1,"name":"example-project"}]}`,
	}
	maps.Copy(defaults, replies)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("doctor attempted mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(405)
			return
		}
		if code := statuses[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
		}
		if body, ok := defaults[r.URL.Path]; ok {
			_, _ = fmt.Fprint(w, body)
		} else {
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestDoctorRejectsOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"test","padding":%q}`, strings.Repeat("x", (8<<20)+1)); err != nil {
			t.Errorf("write oversized response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	apiClient, err := newDoctorAPIClient(server.URL, server.Client())
	require.NoError(t, err)
	_, err = apiClient.PingWithResponse(context.Background())
	require.ErrorIs(t, err, errDoctorResponseTooLarge, "doctor's generated client must preserve the bounded read budget")
}

func TestDoctorRejectsOversizedDecompressedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		body := fmt.Sprintf(`{"ok":true,"service":"kata","version":"test","padding":%q}`, strings.Repeat("x", doctorResponseLimit+1))
		compressed := gzip.NewWriter(w)
		if _, err := io.WriteString(compressed, body); err != nil {
			t.Errorf("write compressed response: %v", err)
		}
		if err := compressed.Close(); err != nil {
			t.Errorf("close compressed response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	t.Cleanup(transport.CloseIdleConnections)
	apiClient, err := newDoctorAPIClient(server.URL, &http.Client{Transport: transport})
	require.NoError(t, err)
	_, err = apiClient.PingWithResponse(context.Background())
	require.ErrorIs(t, err, errDoctorResponseTooLarge, "doctor must cap the decoded response body")
}

func TestDoctorDaemonAndProjectChecks(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, id, status string
		code                         int
	}{
		{"healthy", "", "", "daemon.health", "ok", 0},
		{"wrong service", "/api/v1/ping", `{"ok":true,"service":"other"}`, "daemon.connection", "fail", 0},
		{"unhealthy", "/api/v1/health", `{"ok":false}`, "daemon.health", "fail", 0},
		{"invalid health", "/api/v1/health", `not-json-secret-token`, "daemon.health", "fail", 0},
		{"missing schema", "/api/v1/health", `{"ok":true}`, "daemon.health", "fail", 0},
		{"health unauthorized", "/api/v1/health", `secret-token`, "daemon.health", "fail", 401},
		{"version skew", "/api/v1/health", `{"ok":true,"schema_version":28,"version":"older","api_schema_version":"old"}`, "daemon.version", "warn", 0},
		{"project found", "", "", "workspace.project", "ok", 0},
		{"project missing", "/api/v1/projects", `{"projects":[]}`, "workspace.project", "fail", 0},
		{"project archived", "/api/v1/projects", `{"projects":[{"id":1,"name":"example-project","deleted_at":"2026-10-02T00:00:00Z"}]}`, "workspace.project", "fail", 0},
		{"project catalog denied", "/api/v1/projects", `secret-token`, "workspace.project", "warn", 403},
		{"embeddings disabled", "/api/v1/health", `{"ok":true,"schema_version":28,"embeddings":{"configured":false}}`, "daemon.embeddings", "info", 0},
		{"embedding error", "/api/v1/health", `{"ok":true,"schema_version":28,"embeddings":{"configured":true,"last_error_status":401,"backlog":5}}`, "daemon.embeddings", "warn", 0},
		{"embedding transport error", "/api/v1/health", `{"ok":true,"schema_version":28,"embeddings":{"configured":true,"error_present":true}}`, "daemon.embeddings", "warn", 0},
		{"embedding credential missing", "/api/v1/health", `{"ok":true,"schema_version":28,"embeddings":{"configured":true,"credential":"missing"}}`, "daemon.embeddings", "warn", 0},
		{"embedding credential rejected", "/api/v1/health", `{"ok":true,"schema_version":28,"embeddings":{"configured":true,"credential":"rejected"}}`, "daemon.embeddings", "warn", 0},
		{"federation conflict", "/api/v1/health", `{"ok":true,"schema_version":28,"federation_config":{"configured":1,"conflicted":1}}`, "daemon.federation", "warn", 0},
		{"federation converged", "/api/v1/health", `{"ok":true,"schema_version":28,"federation_config":{"configured":1,"reconciled":1}}`, "daemon.federation", "ok", 0},
		{"federation incomplete", "/api/v1/health", `{"ok":true,"schema_version":28,"federation_config":{"configured":1,"reconciled":0}}`, "daemon.federation", "warn", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, workspace := doctorTestEnv(t)
			flags = globalFlags{Workspace: workspace, Project: "example-project"}
			ctx := doctorServerContext(t, map[string]string{tc.path: tc.body}, map[string]int{tc.path: tc.code})
			r := collectDoctor(ctx)
			require.Equal(t, tc.status, doctorFinding(t, r, tc.id).Status)
			for _, check := range r.Checks {
				require.NotContains(t, fmt.Sprintf("%+v", check), "secret-token")
			}
		})
	}
}

func TestDoctorWarnsWhenScopedCatalogCannotConfirmProject(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	server := doctorTestServer(t, map[string]string{
		"/api/v1/instance": fmt.Sprintf(`{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01","version":%q,"auth":{"kind":"db_token","scope":{"kind":"issue_subtree","project_uid":"01HZZZZZZZZZZZZZZZZZZZZZ02","root_issue_uid":"01HZZZZZZZZZZZZZZZZZZZZZ03"}}}`, version.Version),
		"/api/v1/projects": `{"projects":[{"id":2,"name":"visible-project"}]}`,
	}, nil)
	t.Setenv("KATA_SERVER", server.URL)
	flags = globalFlags{Workspace: workspace, Project: "example-project"}

	report := collectDoctor(context.Background())
	require.Equal(t, "ok", doctorFinding(t, report, "daemon.connection").Status)
	require.Equal(t, "warn", doctorFinding(t, report, "workspace.project").Status,
		"a scoped catalog cannot prove that an unlisted project is absent")
}

func TestDoctorDoesNotInferEmbeddingsAreDisabledFromScopedHealth(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	ctx := doctorServerContext(t, map[string]string{
		"/api/v1/instance": fmt.Sprintf(`{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01","version":%q,"auth":{"kind":"db_token","scope":{"kind":"issue_subtree","project_uid":"01HZZZZZZZZZZZZZZZZZZZZZ02","root_issue_uid":"01HZZZZZZZZZZZZZZZZZZZZZ03"}}}`, version.Version),
	}, nil)
	flags = globalFlags{Workspace: workspace, Project: "example-project"}

	report := collectDoctor(ctx)
	finding := doctorFinding(t, report, "daemon.embeddings")
	require.Equal(t, "info", finding.Status)
	require.Equal(t, "Selected daemon did not provide embedding health details; configuration and provider state are unknown", finding.Summary)
}

func TestDoctorReportsLexicalSearchWhenEmbeddingsAreExplicitlyDisabled(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	ctx := doctorServerContext(t, map[string]string{
		"/api/v1/health": `{"ok":true,"schema_version":28,"embeddings":{"configured":false}}`,
	}, nil)
	flags = globalFlags{Workspace: workspace, Project: "example-project"}

	report := collectDoctor(ctx)
	finding := doctorFinding(t, report, "daemon.embeddings")
	require.Equal(t, "info", finding.Status)
	require.Contains(t, finding.Summary, "lexical search remains available")
}

func TestDoctorProfileIdentityUsesBoundedTimeout(t *testing.T) {
	instanceCalls := &atomic.Int32{}
	server := setupDoctorLocalProfileTarget(t, func(uid string, w http.ResponseWriter, _ *http.Request) {
		instanceCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q,"version":%q,"auth":{"kind":"none"}}`, uid, version.Version)
	})
	t.Cleanup(server.Close)
	t.Setenv("KATA_HTTP_TIMEOUT", "1ns")

	report := collectDoctor(context.Background())
	require.Equal(t, "ok", doctorFinding(t, report, "daemon.connection").Status,
		"profile identity must use doctor's five-second client budget rather than KATA_HTTP_TIMEOUT")
	require.EqualValues(t, 1, instanceCalls.Load())
}

func TestDoctorProfileIdentityRejectsOversizedResponse(t *testing.T) {
	instanceCalls := &atomic.Int32{}
	server := setupDoctorLocalProfileTarget(t, func(uid string, w http.ResponseWriter, _ *http.Request) {
		instanceCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q,"version":%q,"auth":{"kind":"none"},"padding":%q}`,
			uid, version.Version, strings.Repeat("x", doctorResponseLimit+1))
	})
	t.Cleanup(server.Close)
	t.Setenv("KATA_HTTP_TIMEOUT", "10s")

	report := collectDoctor(context.Background())
	require.Equal(t, "fail", doctorFinding(t, report, "daemon.connection").Status,
		"the profile identity response must obey doctor's eight-MiB body limit")
	require.EqualValues(t, 1, instanceCalls.Load())
}

func setupDoctorLocalProfileTarget(t *testing.T, instanceHandler func(string, http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	home, workspace := doctorTestEnv(t)
	profileHome := t.TempDir()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(profileHome, "kata.db"))
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("doctor attempted mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/v1/ping":
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q}`, version.Version)
		case "/api/v1/instance":
			instanceHandler(uid, w, r)
		case "/api/v1/health":
			_, _ = fmt.Fprintf(w, `{"ok":true,"schema_version":28,"api_schema_version":%q,"version":%q}`, daemon.APISchemaVersion, version.Version)
		case "/api/v1/projects":
			_, _ = fmt.Fprint(w, `{"projects":[{"id":1,"name":"example-project"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	profileConfig := fmt.Sprintf("[[daemon]]\nname='example-local'\nlocal=true\nhome=%q\ninstance_uid=%q\n", profileHome, uid)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(profileConfig), 0600))
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "example-local", Local: true, Home: profileHome, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemon.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"),
	})
	require.NoError(t, err)
	flags = globalFlags{Workspace: workspace, Daemon: "example-local", Project: "example-project"}
	return server
}

func TestDoctorStoppedLocalProfileDoesNotCreateFiles(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	profileHome := filepath.Join(t.TempDir(), "missing-profile-home")
	config := fmt.Sprintf("[[daemon]]\nname='example-local'\nlocal=true\nhome=%q\ninstance_uid='01HZZZZZZZZZZZZZZZZZZZZZ01'\n", profileHome)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600))
	flags = globalFlags{Workspace: workspace, Daemon: "example-local"}
	require.Equal(t, "fail", doctorFinding(t, collectDoctor(context.Background()), "daemon.connection").Status)
	_, err := os.Stat(profileHome)
	require.ErrorIs(t, err, os.ErrNotExist, "doctor must not create profile storage or runtime directories")
}

func TestDoctorChecksExplicitDaemonWhenWorkspaceCannotResolve(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	pinged := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("doctor attempted mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/v1/ping":
			pinged = true
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q}`, version.Version)
		case "/api/v1/health":
			_, _ = fmt.Fprintf(w, `{"ok":true,"schema_version":28,"api_schema_version":%q,"version":%q}`, daemon.APISchemaVersion, version.Version)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf("[[daemon]]\nname = \"example-remote\"\nurl = %q\n", server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600))
	flags = globalFlags{Workspace: filepath.Join(workspace, "missing"), Daemon: "example-remote"}

	report := collectDoctor(context.Background())
	require.Equal(t, "ok", doctorFinding(t, report, "daemon.connection").Status)
	require.True(t, pinged, "doctor must contact the explicitly selected daemon without a valid workspace")
	require.Equal(t, "fail", doctorFinding(t, report, "config.workspace").Status)
}

func TestDoctorUsesKataServerWhenWorkspaceCannotResolve(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	server := doctorTestServer(t, nil, nil)
	t.Setenv("KATA_SERVER", server.URL)
	flags = globalFlags{Workspace: filepath.Join(workspace, "missing")}

	report := collectDoctor(context.Background())
	require.Equal(t, "ok", doctorFinding(t, report, "daemon.connection").Status)
	require.Equal(t, "fail", doctorFinding(t, report, "config.workspace").Status)
}

func TestDoctorUsesExplicitProjectWhenWorkspaceCannotResolve(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	server := doctorTestServer(t, nil, nil)
	t.Setenv("KATA_SERVER", server.URL)
	flags = globalFlags{
		Workspace: filepath.Join(workspace, "missing"),
		Project:   "example-project",
	}

	report := collectDoctor(context.Background())
	require.Equal(t, "fail", doctorFinding(t, report, "config.workspace").Status)
	require.Equal(t, "ok", doctorFinding(t, report, "daemon.connection").Status)
	require.Equal(t, "ok", doctorFinding(t, report, "workspace.project").Status,
		"an invalid workspace must not discard an explicitly selected project")
}

func TestDoctorUsesActiveDaemonWhenWorkspaceCannotResolve(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	server := doctorTestServer(t, nil, nil)
	config := fmt.Sprintf("active_daemon = \"example-remote\"\n[[daemon]]\nname = \"example-remote\"\nurl = %q\n", server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600))
	flags = globalFlags{Workspace: filepath.Join(workspace, "missing")}

	report := collectDoctor(context.Background())
	require.Equal(t, "ok", doctorFinding(t, report, "daemon.connection").Status)
	require.Equal(t, "fail", doctorFinding(t, report, "config.workspace").Status)
}

func TestDoctorBoundsNamedDaemonResolutionPingResponse(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	var pingRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("doctor attempted mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if pingRequests.Add(1) == 1 {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"padding":%q}`, version.Version, strings.Repeat("x", doctorResponseLimit+1))
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q}`, version.Version)
	}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf("[[daemon]]\nname = \"example-remote\"\nurl = %q\n", server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600))
	flags = globalFlags{Workspace: workspace, Daemon: "example-remote"}

	report := collectDoctor(context.Background())
	require.EqualValues(t, 1, pingRequests.Load(), "named discovery must not issue an unbounded ping before doctor's capped request")
	require.Equal(t, "fail", doctorFinding(t, report, "daemon.connection").Status)
}

func TestDoctorNamedLocalDiscoveryDoesNotRepairRuntimePermissions(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	assertUnchanged := makeDoctorRuntimeDirectoryInsecureForTest(t, ns.DataDir)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[[daemon]]\nname='example-local'\nlocal=true\n"), 0600))
	flags = globalFlags{Workspace: workspace, Daemon: "example-local"}

	require.Equal(t, "fail", doctorFinding(t, collectDoctor(context.Background()), "daemon.connection").Status)
	assertUnchanged()
}

func TestDoctorDiscoveryRejectsRedirects(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	flags = globalFlags{Workspace: workspace}
	contacted := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		if _, err := fmt.Fprint(w, `{"ok":true,"service":"kata"}`); err != nil {
			t.Errorf("write redirect target response: %v", err)
		}
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//nolint:gosec // The test intentionally redirects to a second origin to verify doctor rejects redirects.
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(source.Close)
	t.Setenv("KATA_SERVER", source.URL)
	require.Equal(t, "fail", doctorFinding(t, collectDoctor(context.Background()), "daemon.connection").Status)
	require.False(t, contacted, "discovery must never contact a redirected origin")
}

func TestDoctorIgnoresRawHTTPTimeoutValue(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	flags = globalFlags{Workspace: workspace}
	t.Setenv("KATA_HTTP_TIMEOUT", "secret-token")
	previous := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = previous
		if err := f.Close(); err != nil {
			t.Errorf("close stderr capture: %v", err)
		}
	})
	collectDoctor(doctorServerContext(t, nil, nil))
	os.Stderr = previous
	require.NoError(t, f.Sync())
	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NotContains(t, string(b), "secret-token")
}

func TestDoctorProjectLookupDoesNotRepairRenamedBinding(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	path := filepath.Join(workspace, ".kata.toml")
	body := []byte("version=1\n[project]\nname='renamed-project'\n")
	require.NoError(t, os.WriteFile(path, body, 0600))
	flags = globalFlags{Workspace: workspace}
	ctx := doctorServerContext(t, nil, nil)
	require.Equal(t, "fail", doctorFinding(t, collectDoctor(ctx), "workspace.project").Status)
	f, err := safefileio.OpenCurrentUserFile(path)
	require.NoError(t, err)
	got, readErr := io.ReadAll(f)
	closeErr := f.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	require.Equal(t, body, got)
}

func TestDoctorUnavailableRemoteDoesNotCreateLocalRuntime(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	flags = globalFlags{Workspace: workspace}
	require.Equal(t, "fail", doctorFinding(t, collectDoctor(context.Background()), "daemon.connection").Status)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestDoctorHookFindings(t *testing.T) {
	for _, tc := range []struct {
		name, body, id, status string
		code                   int
	}{
		{"unsupported", "", "daemon.hooks", "warn", 404},
		{"healthy", `{"hooks":{"available":true,"hooks":[]}}`, "daemon.hooks", "info", 0},
		{"custom sink", `{"hooks":{"available":false}}`, "daemon.hooks", "warn", 0},
		{"missing executable", `{"hooks":{"available":true,"hooks":[{"index":0,"command":"missing-doctor-hook","executable_available":false,"working_directory_available":true}]}}`, "daemon.hooks", "fail", 0},
		{"missing directory", `{"hooks":{"available":true,"hooks":[{"index":0,"command":"missing-doctor-hook","executable_available":true,"working_directory_available":false}]}}`, "daemon.hooks", "fail", 0},
		{"retained failures", `{"hooks":{"available":true,"hooks":[],"recent_runs":3,"recent_failures":2}}`, "daemon.hook_runs", "warn", 0},
		{"dropped events", `{"hooks":{"available":true,"hooks":[],"dropped":2}}`, "daemon.hook_queue", "warn", 0},
		{"partial history", `{"hooks":{"available":true,"hooks":[],"history_incomplete":true}}`, "daemon.hook_runs", "warn", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, workspace := doctorTestEnv(t)
			flags = globalFlags{Workspace: workspace, Project: "example-project"}
			ctx := doctorServerContext(t, map[string]string{"/api/v1/doctor": tc.body}, map[string]int{"/api/v1/doctor": tc.code})
			require.Equal(t, tc.status, doctorFinding(t, collectDoctor(ctx), tc.id).Status)
		})
	}
}

func TestDoctorHookNamesPATHMismatch(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	flags = globalFlags{Workspace: workspace, Project: "example-project"}
	exe, err := os.Executable()
	require.NoError(t, err)
	body := fmt.Sprintf(`{"hooks":{"available":true,"hooks":[{"index":0,"command":%q,"executable_available":false,"working_directory_available":true}]}}`, exe)
	ctx := doctorServerContext(t, map[string]string{"/api/v1/doctor": body}, nil)
	check := doctorFinding(t, collectDoctor(ctx), "daemon.hooks")
	require.Equal(t, "fail", check.Status)
	require.Contains(t, fmt.Sprint(check.Details), "PATH")
}
