package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func stoppedProfileCLI(t *testing.T) (home, workspace, instanceUID, projectUID string) {
	t.Helper()
	personal := t.TempDir()
	home, workspace = t.TempDir(), t.TempDir()
	t.Setenv("KATA_HOME", personal)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "personal-token")
	t.Chdir(workspace)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	instanceUID = store.InstanceUID()
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	projectUID = project.UID
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(filepath.Join(personal, "config.toml"), []byte(fmt.Sprintf(`[[daemon]]
name = "work"
local = true
home = %q
instance_uid = %q
`, home, instanceUID)), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte("version = 1\n[server]\ndaemon = \"work\"\n"), 0600))
	return
}

func TestDaemonDiagnoseStoppedProfileReadOnly(t *testing.T) {
	home, _, uid, projectUID := stoppedProfileCLI(t)
	for _, mode := range []string{"--json", "--agent", ""} {
		args := []string{"daemon", "diagnose"}
		if mode != "" {
			args = append(args, mode)
		}
		out, _, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(t, err)
		assert.Contains(t, out, "stopped_local_profile")
		if mode == "--json" {
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &body))
			assert.Equal(t, home, body["home"])
			assert.Equal(t, uid, body["expected_instance_uid"])
			assert.Equal(t, uid, body["observed_instance_uid"])
			assert.Equal(t, projectUID, body["project_uid"])
			assert.Equal(t, "work", body["profile"])
		}
		assert.NotContains(t, out, "personal-token")
	}
	_, err := os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseReportsLivePrincipalCapabilities(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(fmt.Sprint(writable), func(t *testing.T) {
			home, _, uid, _ := stoppedProfileCLI(t)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken=\"profile-token\"\n"), 0600))
			policy := "bootstrap"
			if writable {
				policy = "identity"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ping" {
					_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
					return
				}
				assert.Equal(t, "Bearer profile-token", r.Header.Get("Authorization"))
				_, _ = fmt.Fprintf(w, `{"instance_uid":%q,"web_ui_capabilities":{"writable":%t,"actor_policy":%q}}`, uid, writable, policy)
			}))
			t.Cleanup(server.Close)
			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
			require.NoError(t, err)
			ns, err := daemonruntime.NewNamespaceForHome(home, profile.StorageID)
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
			require.NoError(t, err)
			for _, mode := range []string{"--json", "--agent", ""} {
				args := []string{"daemon", "diagnose"}
				if mode != "" {
					args = append(args, mode)
				}
				out, _, err := executeRootCapture(t, t.Context(), args...)
				require.NoError(t, err)
				if mode == "--json" {
					var body map[string]any
					require.NoError(t, json.Unmarshal([]byte(out), &body))
					require.Equal(t, writable, body["writable"])
					require.Equal(t, policy, body["actor_policy"])
				} else {
					require.Contains(t, out, fmt.Sprint(writable))
					require.Contains(t, out, policy)
				}
				require.NotContains(t, out, "profile-token")
				require.NotContains(t, out, "personal-token")
			}
		})
	}
}

func TestDaemonDiagnoseRefusesWrongProjectRecovery(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--expect-project-uid", "01HZZZZZZZZZZZZZZZZZZZZZ01", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "wrong_project")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover", "--expect-project-uid", "01HZZZZZZZZZZZZZZZZZZZZZ01", "--json")
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseNeverAcceptsAnUnverifiedProjectAssertion(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint(remote), func(t *testing.T) {
			setupKataEnv(t)
			t.Chdir(t.TempDir())
			t.Setenv("KATA_SERVER", "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			}))
			t.Cleanup(server.Close)
			if remote {
				t.Setenv("KATA_SERVER", server.URL)
			} else {
				ns, err := daemonruntime.NewNamespace()
				require.NoError(t, err)
				require.NoError(t, ns.EnsureDirs())
				_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
				require.NoError(t, err)
			}
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--expect-project-uid", "01HZZZZZZZZZZZZZZZZZZZZZ01", "--json")
			require.NoError(t, err)
			assertDiagnosticState(t, out, "project_unverifiable")
		})
	}
}

func TestDaemonDiagnoseUnknownLoopbackStaysPinned(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1/secret-path?token=remote-secret")
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "unknown_loopback_endpoint")
	assert.NotContains(t, out, "remote-secret")
	assert.NotContains(t, out, "secret-path")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover")
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonStatusSelectedProfileDoesNotReportPersonalReady(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	personal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
	}))
	defer personal.Close()
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(personal.URL, "http://")})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "status", "--json")
	assert.Equal(t, kindDaemonUnavail, requireCLIError(t, err, ExitDaemonUnavail).Kind)
	var body struct {
		Daemons  []any          `json:"daemons"`
		Selected map[string]any `json:"selected"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Len(t, body.Daemons, 1)
	assert.Equal(t, "stopped_local_profile", body.Selected["state"])
	assert.Equal(t, home, body.Selected["home"])
}

func TestDaemonStatusHumanScopesCurrentHomeAbsenceWithReadyProfile(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken=\"profile-token\"\n"), 0o600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q,"web_ui_capabilities":{"writable":false,"actor_policy":"bootstrap"}}`, uid)
	}))
	t.Cleanup(server.Close)
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)

	out, _, err := executeRootCapture(t, t.Context(), "daemon", "status")

	require.NoError(t, err)
	assert.Contains(t, out, "Selected daemon: ready")
	assert.Contains(t, out, "No kata daemon is running in the current KATA_HOME.")
}

func TestDaemonStatusKeepsCurrentHomeListWhenSelectionIsInvalid(t *testing.T) {
	_, _, _, _ = stoppedProfileCLI(t)
	t.Setenv("KATA_HOME", t.TempDir())
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:7777"})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "status", "--json")
	assert.Equal(t, kindValidation, requireCLIError(t, err, ExitValidation).Kind)
	var body struct {
		Daemons  []any          `json:"daemons"`
		Selected map[string]any `json:"selected"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Len(t, body.Daemons, 1)
	require.Equal(t, "selection_error", body.Selected["state"])
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover")
	require.Error(t, err, "partial status must not allow recovery of another home")
}

func TestDaemonDiagnoseBoundsCurrentHomeStorageProbe(t *testing.T) {
	setupKataEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_HTTP_TIMEOUT", "50ms")
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	release := make(chan struct{})
	defer close(release)
	defer func() { _ = listener.Close() }()
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			defer func() { _ = connection.Close() }()
			<-release
		}
	}()
	t.Setenv("KATA_DSN", fmt.Sprintf("postgres://profile_test@%s/example_db?sslmode=disable", listener.Addr()))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	out, _, err := executeRootCapture(t, ctx, "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "unreadable_storage")
	require.Less(t, time.Since(started), time.Second, "a stalled database must not hold diagnosis past its configured budget")
}

func TestDaemonHomeCommandsRejectDaemonSelectorBeforeEffects(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	for _, command := range []string{"start", "stop", "restart", "reload", "logs"} {
		_, _, err := executeRootCapture(t, t.Context(), "--daemon", "work", "daemon", command)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "KATA_HOME")
	}
	_, err := os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseMissingProfileStorage(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	require.NoError(t, os.Remove(filepath.Join(home, "kata.db")))
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "missing_profile_storage")
	_, err = os.Stat(filepath.Join(home, "kata.db"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func assertDiagnosticState(t *testing.T, body, state string) {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	assert.Equal(t, state, result["state"])
}

func TestDaemonLocateStoppedProfileDoesNotStart(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	_, _, err := executeRootCapture(t, t.Context(), "daemon", "locate", "--json")
	require.Error(t, err)
	var target *cliError
	require.ErrorAs(t, err, &target)
	assert.Equal(t, "stopped_local_profile", target.Code)
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestHealthSelectedProfileDoesNotUsePersonalRuntime(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	personal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"schema_version":29,"uptime":"1s","db_path":"personal.db"}`)
	}))
	defer personal.Close()
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(personal.URL, "http://")})
	require.NoError(t, err)
	_, _, err = executeRootCapture(t, t.Context(), "health", "--json")
	require.Error(t, err)
	var target *cliError
	require.ErrorAs(t, err, &target)
	assert.Equal(t, "stopped_local_profile", target.Code)
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseEmptyCurrentHomeDoesNotInitialize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "local_stopped")
	for _, name := range []string{"kata.db", "kata.db.import.lock", "runtime"} {
		_, err = os.Stat(filepath.Join(home, name))
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestDaemonStatusEmptyHomeDoesNotInitialize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	_, _, _ = executeRootCapture(t, t.Context(), "daemon", "status", "--json")
	_, err := os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestHealthReadyProfileIncludesSelectedIdentity(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken = \"work-token\"\n"), 0600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ping":
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
		case "/api/v1/instance":
			assert.Equal(t, "Bearer work-token", r.Header.Get("Authorization"))
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
		case "/api/v1/health":
			assert.Equal(t, "Bearer work-token", r.Header.Get("Authorization"))
			_, _ = fmt.Fprint(w, `{"ok":true,"schema_version":29,"uptime":"1s","db_path":"work.db"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ns, err := daemonruntime.NewNamespaceForHome(home, config.DBHash(filepath.Join(home, "kata.db")))
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, t.Context(), "health", "--json")
	require.NoError(t, err)
	var body struct {
		Selected map[string]any `json:"selected"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	assert.Equal(t, "ready", body.Selected["state"])
	assert.Equal(t, home, body.Selected["home"])
	assert.Equal(t, uid, body.Selected["observed_instance_uid"])
	assert.NotContains(t, out, "work-token")
	assert.NotContains(t, out, "personal-token")
	for _, mode := range []string{"--agent", "--format=human"} {
		out, _, err = executeRootCapture(t, t.Context(), "health", mode)
		require.NoError(t, err)
		if mode == "--agent" {
			assert.Contains(t, out, agentValue(home))
		} else {
			assert.Contains(t, out, home)
		}
		assert.Contains(t, out, uid)
	}
}

func TestDaemonDiagnoseWrongDatabaseRetainsObservedUID(t *testing.T) {
	home, _, observed, _ := stoppedProfileCLI(t)
	//nolint:gosec // KATA_HOME is the fixture temporary home.
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("KATA_HOME"), "config.toml"), []byte(fmt.Sprintf(`[[daemon]]
name = "work"
local = true
home = %q
instance_uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
`, home)), 0600))
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "wrong_database")
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, observed, result["observed_instance_uid"])
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover")
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseMissingProjectDoesNotCreate(t *testing.T) {
	home, workspace, _, _ := stoppedProfileCLI(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version = 1\n[project]\nname = \"missing-project\"\n"), 0600))
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "stopped_local_profile")
	out, _, err = executeRootCapture(t, t.Context(), "daemon", "diagnose", "--expect-project-uid", "01HZZZZZZZZZZZZZZZZZZZZZ01", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "missing_project")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover", "--expect-project-uid", "01HZZZZZZZZZZZZZZZZZZZZZ01")
	require.Error(t, err)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"), db.ReadOnly())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	projects, err := store.ListProjects(t.Context())
	require.NoError(t, err)
	for _, project := range projects {
		assert.NotEqual(t, "missing-project", project.Name)
	}
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseProjectReadFailureIsUnreadableStorage(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "ALTER TABLE projects RENAME TO unavailable_projects")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "unreadable_storage")
}

func TestDaemonDiagnoseUnsupportedSchemaCannotRecover(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "UPDATE meta SET value=? WHERE key='schema_version'", fmt.Sprint(db.CurrentSchemaVersion()+1))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "version_mismatch")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover")
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	version, err := sqlitestore.PeekSchemaVersion(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion()+1, version)
}

func TestDaemonDiagnoseCurrentHomeUnreadableSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	uid := store.InstanceUID()
	_, err = store.ExecContext(t.Context(), "UPDATE meta SET value=? WHERE key='schema_version'", "unreadable")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "unreadable_storage")
	assert.Contains(t, out, uid)
	assert.Contains(t, out, "restore access")
	assert.NotContains(t, out, "compatible binary")
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseCurrentHomeUnreadableDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(home, "kata.db"), []byte("not a sqlite database"), 0o600))

	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")

	require.NoError(t, err)
	assertDiagnosticState(t, out, "unreadable_storage")
	assert.Contains(t, out, "restore access")
	assert.NotContains(t, out, "kata daemon start")
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonDiagnoseCurrentHomeUnreadableIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "DELETE FROM meta WHERE key='instance_uid'")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "unreadable_storage")
	assert.Contains(t, out, "restore access")
	assert.NotContains(t, out, "kata daemon start")
	_, err = os.Stat(filepath.Join(home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestHealthRespondingUnhealthyRemote(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q}`, version.Version)
			return
		}
		if r.URL.Path == "/api/v1/health" {
			_, _ = fmt.Fprint(w, `{"ok":false,"schema_version":1}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	t.Setenv("KATA_SERVER", server.URL)
	out, _, err := executeRootCapture(t, t.Context(), "health", "--json")
	require.NoError(t, err)
	var result struct {
		OK       bool            `json:"ok"`
		Selected daemonDiagnosis `json:"selected"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.False(t, result.OK)
	assert.Equal(t, "remote", result.Selected.Kind)
	assert.Equal(t, "unhealthy", result.Selected.State)
	assert.Equal(t, server.URL, result.Selected.Endpoint)
}

func TestDaemonDiagnoseLiveVersionMismatchDoesNotStop(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"earlier-test-version","pid":%d}`, os.Getpid())
	}))
	defer server.Close()
	ns, err := daemonruntime.NewNamespaceForHome(home, config.DBHash(filepath.Join(home, "kata.db")))
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "version_mismatch")
	req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/ping", nil)
	require.NoError(t, reqErr)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func TestDaemonDiagnoseWrongLiveDatabaseReportsLiveUID(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	observed := "01HZZZZZZZZZZZZZZZZZZZZZ01"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, observed)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
	}))
	defer server.Close()
	ns, err := daemonruntime.NewNamespaceForHome(home, config.DBHash(filepath.Join(home, "kata.db")))
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "wrong_database")
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, observed, result["observed_instance_uid"])
}

func TestDaemonInspectionDoesNotRepairCurrentHomeRuntimePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	for _, command := range []string{"status", "diagnose"} {
		require.NoError(t, os.Chmod(ns.DataDir, 0755)) //nolint:gosec // Deliberately non-private regression fixture.
		_, _, _ = executeRootCapture(t, t.Context(), "daemon", command, "--json")
		info, err := os.Stat(ns.DataDir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0755), info.Mode().Perm(), command)
	}
}

func TestLocalProfileLocateRetainsConfiguredSource(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
	}))
	t.Cleanup(server.Close)
	ns, err := daemonruntime.NewNamespaceForHome(home, config.DBHash(filepath.Join(home, "kata.db")))
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	for _, selector := range []string{"workspace", "active", "explicit"} {
		t.Run(selector, func(t *testing.T) {
			args := []string{"daemon", "locate", "--json"}
			if selector == "active" {
				configPath, err := config.DaemonConfigPath()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("active_daemon=\"work\"\n[[daemon]]\nname=\"work\"\nlocal=true\nhome=%q\ninstance_uid=%q\n", home, uid)), 0600))
				args = append(args, "--workspace", t.TempDir(), "--project", "spoke-project")
			} // The catalog active profile remains selected.
			if selector == "explicit" {
				args = append(args, "--daemon", "work")
			}
			out, _, err := executeRootCapture(t, t.Context(), args...)
			require.NoError(t, err)
			var result struct{ Source string }
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			want := "configured"
			if selector == "explicit" {
				want = "daemon_flag"
			}
			assert.Equal(t, want, result.Source)
		})
	}
}

func TestDaemonLocateDoesNotRequireCurrentHomeStorage(t *testing.T) {
	for _, contents := range []string{"missing project", "unreadable storage"} {
		t.Run(contents, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("KATA_SERVER", "")
			workspace := t.TempDir()
			t.Chdir(workspace)
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version=1\n[project]\nname=\"missing-project\"\n"), 0600))
			if contents == "missing project" {
				store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
				require.NoError(t, err)
				require.NoError(t, store.Close())
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(home, "kata.db"), []byte("not a sqlite database"), 0600))
			}
			addr, cleanup := pipeServer(t)
			t.Cleanup(cleanup)
			require.NoError(t, writeRuntimeFor(home, addr))
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "locate", "--json")
			require.NoError(t, err)
			var result daemonLocateOutput
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			assert.Equal(t, addr, result.Address)
		})
	}
}

func TestDaemonDefaultVersionDifferenceIsInformational(t *testing.T) {
	home := setupKataEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"earlier-test-version","pid":%d}`, os.Getpid())
	}))
	t.Cleanup(server.Close)
	require.NoError(t, writeRuntimeFor(home, strings.TrimPrefix(server.URL, "http://")))
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "ready")
	assert.Contains(t, out, "earlier-test-version")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "locate", "--json")
	require.NoError(t, err)
}

func TestDaemonSelectionErrorExplainsRoutingFailure(t *testing.T) {
	stoppedProfileCLI(t)
	t.Setenv("KATA_HOME", t.TempDir())
	for _, mode := range []string{"--json", "--agent", "--format=human"} {
		out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", mode)
		require.NoError(t, err)
		assert.Contains(t, out, "selection_error")
		assert.Contains(t, out, "named daemon not found")
		assert.Contains(t, out, "work")

		out, _, err = executeRootCapture(t, t.Context(), "daemon", "status", mode)
		assert.Equal(t, kindValidation, requireCLIError(t, err, ExitValidation).Kind)
		assert.Contains(t, out, "selection_error")
		assert.Contains(t, out, "named daemon not found")
		assert.Contains(t, out, "work")
	}
}

func TestDaemonDiagnoseFederationReadFailureIsUnreadableStorage(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "ALTER TABLE federation_bindings RENAME TO unavailable_bindings")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "unreadable_storage")
	assert.Contains(t, out, "federation_bindings")
}

func TestDaemonRunningProfileDoesNotRequireProject(t *testing.T) {
	home, workspace, uid, _ := stoppedProfileCLI(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version=1\n[project]\nname=\"missing-project\"\n"), 0600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
	}))
	t.Cleanup(server.Close)
	ns, err := daemonruntime.NewNamespaceForHome(home, config.DBHash(filepath.Join(home, "kata.db")))
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	for _, command := range []string{"diagnose", "recover", "locate"} {
		out, _, err := executeRootCapture(t, t.Context(), "daemon", command, "--json")
		require.NoError(t, err)
		if command != "locate" {
			assertDiagnosticState(t, out, "ready")
			var result map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			assert.Equal(t, "missing_project", result["project_state"])
		}
	}
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover", "--expect-project-uid", "01HZZZZZZZZZZZZZZZZZZZZZ01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing_project")
}

// A supervised profile's runtime record can outlive its listener while the
// service manager restarts it. Recover must wait through that gap once the
// storage, schema, and project checks pass.
func TestDaemonRecoverWaitsThroughSupervisedRestartGap(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	var pings atomic.Int32
	supervisedProfileDaemon(t, home, uid, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			if pings.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
	})
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "recover", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"ready"`)
	assert.GreaterOrEqual(t, pings.Load(), int32(2))
}

// Waiting through a restart gap must not hide a credential failure.
func TestDaemonRecoverRejectsSupervisedAuthFailureWithoutWaiting(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	supervisedProfileDaemon(t, home, uid, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := executeRootCapture(t, ctx, "daemon", "recover")
	require.ErrorContains(t, err, "authentication failed")
}

// supervisedProfileDaemon publishes handler as the runtime of a profile home
// whose service manager owns startup.
func supervisedProfileDaemon(t *testing.T, home, uid string, handler http.HandlerFunc) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
}

func TestDaemonUnreachableProfileRecoveryAdvice(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())

	for _, tc := range []struct {
		name, command string
		managed       bool
	}{
		{name: "ordinary", command: "list"},
		{name: "service managed", command: "list", managed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.managed {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			address := strings.TrimPrefix(server.URL, "http://")
			_, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: address})
			require.NoError(t, err)
			if tc.managed {
				settledCLIUse(t, func(t *testing.T) {
					_, _, err := executeRootCapture(t, t.Context(), "--daemon", "work", tc.command)
					cliErr := requireCLIError(t, err, ExitDaemonUnavail)
					require.Equal(t, 1, strings.Count(cliErr.Message, "autostart = false"))
					require.NotContains(t, cliErr.Message, "service manager")
					require.Contains(t, cliErr.Hint, "service manager")
				})
				return
			}
			_, _, err = executeRootCapture(t, t.Context(), "--daemon", "work", tc.command)
			cliErr := requireCLIError(t, err, ExitDaemonUnavail)
			require.Equal(t, "local_unreachable", cliErr.Code)
			require.Contains(t, cliErr.Message, "503")
			require.Contains(t, cliErr.Hint, "daemon recover")
		})
	}
}

func TestDaemonDiagnoseSecondPing(t *testing.T) {
	for _, tc := range []struct {
		name, catalog, end string
		profile            bool
	}{
		{name: "current home"},
		{name: "named local", catalog: "work"},
		{name: "canceled", end: "cancel"},
		{name: "profile", profile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := ""
			uid := ""
			if tc.profile {
				home, _, uid, _ = stoppedProfileCLI(t)
			} else {
				home = setupKataEnv(t)
			}
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_DSN", "")
			t.Chdir(t.TempDir())
			resetFlags(t)
			flags.Daemon = tc.catalog
			if tc.catalog != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[[daemon]]\nname = 'work'\nlocal = true\n"), 0600))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var pings atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
					return
				}
				if pings.Add(1) == 1 {
					_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
					return
				}
				if tc.end == "cancel" {
					cancel()
				}
				http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			if tc.profile {
				profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
				require.NoError(t, err)
				ns, err = daemonruntime.NewNamespaceForHome(home, profile.StorageID)
				require.NoError(t, err)
			}
			require.NoError(t, ns.EnsureDirs())
			record := kitdaemon.NewRuntimeRecord("kata", version.Version, kitdaemon.Endpoint{Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
			_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(record)
			require.NoError(t, err)
			var result daemonDiagnosis
			if tc.profile {
				out, _, err := executeRootCapture(t, ctx, "--daemon", "work", "daemon", "diagnose", "--json")
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal([]byte(out), &result))
			} else {
				selection, err := client.InspectSelection(ctx, "", tc.catalog)
				require.NoError(t, err)
				result, _, err = probeDaemonSelection(ctx, selectedDaemonDiagnosis(selection, ""), selection)
				require.NoError(t, err)
			}
			require.EqualValues(t, 2, pings.Load())
			require.Equal(t, "local_unreachable", result.State)
			if tc.end == "cancel" {
				require.Error(t, ctx.Err())
				assert.NotContains(t, result.NextAction, "kill")
				assert.NotContains(t, result.NextAction, "start it")
				assert.NotContains(t, result.NextAction, "service manager")
				return
			}
			assert.Equal(t, os.Getpid(), result.PID)
			assert.Contains(t, result.Message, "503")
			if tc.profile {
				assert.Contains(t, result.NextAction, "daemon recover")
			}
		})
	}
}

func TestDaemonDiagnoseStorageBusy(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	path := filepath.Join(home, "kata.db")
	store, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "UPDATE meta SET value=? WHERE key='schema_version'", fmt.Sprint(db.CurrentSchemaVersion()-1))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	lock := flock.New(path + ".import.lock")
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "storage_busy")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "recover")
	require.ErrorContains(t, err, "wait for the SQLite upgrade or import")
	require.NoError(t, lock.Close())
	out, _, err = executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "version_mismatch")
}

func TestDaemonDiagnoseCurrentHomeStorageBusy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	path := filepath.Join(home, "kata.db")
	store, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	lock := flock.New(path + ".import.lock")
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { require.NoError(t, lock.Close()) }()
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--agent")
	require.NoError(t, err)
	assert.Contains(t, out, "state=storage_busy")
	assert.Contains(t, out, "wait for the SQLite upgrade or import")
	for _, suffix := range []string{"-wal", "-shm"} {
		_, err = os.Stat(path + suffix)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestDaemonDiagnoseMissingDatabaseWithHeldMarker(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	path := filepath.Join(home, "kata.db")
	require.NoError(t, os.Remove(path))
	lock := flock.New(path + ".import.lock")
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { require.NoError(t, lock.Close()) }()
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "storage_busy")
}

func TestDaemonDiagnoseReadOnlyDirectoryWithoutMarker(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes")
	}
	home, _, _, _ := stoppedProfileCLI(t)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "PRAGMA journal_mode=DELETE")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NoError(t, os.Chmod(home, 0500))
	t.Cleanup(func() { _ = os.Chmod(home, 0700) })
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "stopped_local_profile")
	_, err = os.Stat(filepath.Join(home, "kata.db.import.lock"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLockDiagnosisStoragePostgres(t *testing.T) {
	t.Chdir(t.TempDir())
	result := daemonDiagnosis{}
	release, busy, err := lockDiagnosisStorage(&result, "postgres://example.invalid/example")
	require.NoError(t, err)
	require.False(t, busy)
	release()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestDaemonDiagnoseAllowsConcurrentSQLiteOpen(t *testing.T) {
	home, workspace, _, _ := stoppedProfileCLI(t)
	resetFlags(t)
	path := filepath.Join(home, "kata.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=DELETE")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	selection, err := client.InspectSelection(t.Context(), workspace, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	diagnosis := make(chan daemonDiagnosis, 1)
	var opened chan error
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		cancel()
		if diagnosis != nil {
			select {
			case <-diagnosis:
			case <-time.After(10 * time.Second):
				t.Error("diagnosis did not stop")
			}
		}
		if opened != nil {
			select {
			case <-opened:
			case <-time.After(10 * time.Second):
				t.Error("SQLite open did not stop")
			}
		}
	}()
	go func() {
		result, _, _ := diagnoseDaemonSelection(ctx, "", selection)
		diagnosis <- result
	}()
	marker := flock.New(path + ".import.lock")
	defer func() { _ = marker.Close() }()
	require.Eventually(t, func() bool {
		locked, err := marker.TryLock()
		if err != nil {
			return false
		}
		if locked {
			_ = marker.Unlock()
			return false
		}
		return true
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, marker.Close())
	opened = make(chan error, 1)
	go func() {
		store, err := storeopen.Open(ctx, path)
		if err == nil {
			err = store.Close()
		}
		opened <- err
	}()
	var openErr error
	finished := false
	select {
	case openErr = <-opened:
		finished = true
		opened = nil
	case <-time.After(1500 * time.Millisecond):
	}
	_, err = conn.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	if !finished {
		select {
		case openErr = <-opened:
			opened = nil
		case <-time.After(5 * time.Second):
			t.Fatal("SQLite open did not finish")
		}
	}
	select {
	case result := <-diagnosis:
		diagnosis = nil
		require.NoError(t, openErr)
		require.Equal(t, "stopped_local_profile", result.State)
	case <-time.After(5 * time.Second):
		t.Fatal("diagnosis did not finish")
	}
}

func TestDaemonDiagnoseAllowsDelayedUpgrade(t *testing.T) {
	home, workspace, _, _ := stoppedProfileCLI(t)
	resetFlags(t)
	path := filepath.Join(home, "kata.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=DELETE")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DROP INDEX idx_events_project_moves")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE meta SET value='52' WHERE key='schema_version'")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	selection, err := client.InspectSelection(t.Context(), workspace, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	diagnosis := make(chan daemonDiagnosis, 1)
	var opened chan error
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		cancel()
		if diagnosis != nil {
			select {
			case <-diagnosis:
			case <-time.After(10 * time.Second):
				t.Error("diagnosis did not stop")
			}
		}
		if opened != nil {
			select {
			case <-opened:
			case <-time.After(10 * time.Second):
				t.Error("SQLite open did not stop")
			}
		}
	}()
	go func() {
		result, _, _ := diagnoseDaemonSelection(ctx, "", selection)
		diagnosis <- result
	}()
	marker := flock.New(path + ".import.lock")
	defer func() { _ = marker.Close() }()
	require.Eventually(t, func() bool {
		locked, err := marker.TryLock()
		if err != nil {
			return false
		}
		if locked {
			_ = marker.Unlock()
			return false
		}
		return true
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, marker.Close())
	opened = make(chan error, 1)
	go func() {
		opened <- jsonl.AutoCutover(ctx, path)
	}()
	var openErr error
	finished := false
	select {
	case openErr = <-opened:
		finished = true
		opened = nil
	case <-time.After(1500 * time.Millisecond):
	}
	_, err = conn.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	if !finished {
		select {
		case openErr = <-opened:
			opened = nil
		case <-time.After(5 * time.Second):
			t.Fatal("SQLite open did not finish")
		}
	}
	select {
	case result := <-diagnosis:
		diagnosis = nil
		require.NoError(t, openErr)
		require.Equal(t, "version_mismatch", result.State)
		version, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
		require.NoError(t, err)
		require.Equal(t, db.CurrentSchemaVersion(), version)
	case <-time.After(5 * time.Second):
		t.Fatal("diagnosis did not finish")
	}
}

func TestDaemonDiagnoseDuringReadinessInspection(t *testing.T) {
	home, _, _, _ := stoppedProfileCLI(t)
	path := filepath.Join(home, "kata.db")
	store, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "UPDATE meta SET value=? WHERE key='schema_version'", fmt.Sprint(db.CurrentSchemaVersion()-1))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	lock := flock.New(path + ".import.lock")
	require.NoError(t, lock.RLock())
	defer func() { _ = lock.Close() }()
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "version_mismatch")
}

func TestDaemonDiagnoseIgnoresOtherDatabaseCutover(t *testing.T) {
	stoppedProfileCLI(t)
	lock := flock.New(filepath.Join(t.TempDir(), "kata.db.import.lock"))
	require.NoError(t, lock.Lock())
	defer func() { _ = lock.Close() }()
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "diagnose", "--json")
	require.NoError(t, err)
	assertDiagnosticState(t, out, "stopped_local_profile")
}
