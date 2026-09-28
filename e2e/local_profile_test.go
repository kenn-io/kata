//go:build !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestLocalProfileTwoHomeRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process profile recovery")
	}
	bin := buildKataBinary(t)
	hub := func() (*httptest.Server, db.Project) {
		store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		project, err := store.CreateProject(t.Context(), "spoke-project")
		require.NoError(t, err)
		server := httptest.NewServer(daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now(), Broadcaster: daemon.NewEventBroadcaster()}).Handler())
		t.Cleanup(server.Close)
		return server, project
	}
	personalHub, personalProject := hub()
	workHub, workProject := hub()
	personalHome, workHome, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	personalAddr, workAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t)), fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	seed := func(home, addr, token, hubURL string, project db.Project) (string, string) {
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("listen = %q\n[auth]\ntoken = %q\n", addr, token)), 0600))
		store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
		require.NoError(t, err)
		local, err := store.CreateProjectWithUID(t.Context(), project.Name, project.UID)
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: local.ID, Title: "existing replica issue", Author: "operator"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, AllowInsecure: true})
		require.NoError(t, err)
		uid := store.InstanceUID()
		require.NoError(t, store.Close())
		return uid, issue.ShortID
	}
	personalUID, _ := seed(personalHome, personalAddr, "personal-token", personalHub.URL, personalProject)
	workUID, shortID := seed(workHome, workAddr, "work-token", workHub.URL, workProject)
	require.NotEqual(t, personalUID, workUID)
	require.NotEqual(t, personalProject.UID, workProject.UID)
	file, err := os.OpenFile(filepath.Join(personalHome, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600) //nolint:gosec // Created temporary test home.
	require.NoError(t, err)
	_, err = fmt.Fprintf(file, "\n[[daemon]]\nname = \"work\"\nlocal = true\nhome = %q\ninstance_uid = %q\n", workHome, workUID)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	env := func(home string) []string {
		result := []string{}
		for _, key := range []string{"PATH", "HOME", "USER", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH"} {
			if value, ok := os.LookupEnv(key); ok {
				result = append(result, key+"="+value)
			}
		}
		return append(result, "KATA_HOME="+home, "KATA_TELEMETRY=0")
	}
	run := func(home, dir string, extraEnv []string, args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-built executable and test-owned arguments
		command.Dir = dir
		command.Env = append(env(home), extraEnv...)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err := command.Run()
		if ctx.Err() != nil {
			t.Fatalf("command timeout: %s: %s", strings.Join(args, " "), stderr.String())
		}
		if err == nil {
			return stdout.String(), 0
		}
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, stderr.String())
		return stdout.String() + stderr.String(), exit.ExitCode()
	}
	personalProcess, personalLog := startDaemonCmd(t, bin, env(personalHome))
	waitForPing(t, "http://"+personalAddr, 10*time.Second)
	// Start then stop the work process: this models an existing stopped spoke,
	// not a new profile requiring bootstrap or enrollment.
	workProcess, workLog := startDaemonCmd(t, bin, env(workHome))
	waitForPing(t, "http://"+workAddr, 10*time.Second)
	stopDaemon(workProcess)
	workProcess.Process = nil // startDaemonCmd cleanup must not wait twice.
	bind := func(root string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.local.toml"), []byte("version = 1\n[server]\ndaemon = \"work\"\n"), 0600))
	}
	bind(workspace)
	child := filepath.Join(workspace, "child")
	require.NoError(t, os.Mkdir(child, 0700))
	external := t.TempDir()
	diagnose := func(dir string, args ...string) map[string]any {
		t.Helper()
		out, code := run(personalHome, dir, []string{"KATA_AUTH_TOKEN=personal-token"}, append(args, "daemon", "diagnose", "--json")...)
		require.Zero(t, code, out)
		var result map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &result), out)
		return result
	}
	for _, scope := range []struct {
		dir  string
		args []string
	}{{workspace, nil}, {child, nil}, {external, []string{"--workspace", workspace}}} {
		result := diagnose(scope.dir, scope.args...)
		assert.Equal(t, "stopped_local_profile", result["state"])
		assert.Equal(t, workHome, result["home"])
		assert.Equal(t, workUID, result["observed_instance_uid"])
		assert.Equal(t, workProject.UID, result["project_uid"])
		assert.Equal(t, workHub.URL, result["hub_origin"])
	}
	// Separate repository and linked worktree roots keep machine-local overrides
	// alongside their own binding, as existing URL workspace discovery requires.
	repository := t.TempDir()
	git := func(dir string, args ...string) {
		command := exec.Command("git", args...) //nolint:gosec // Fixed fixture commands initialize temporary repositories.
		command.Dir = dir
		command.Env = append(env(personalHome),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		out, err := command.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	git(repository, "init", "-q")
	bind(repository)
	git(repository, "add", ".kata.toml")
	git(repository, "-c", "user.name=operator", "-c", "user.email=operator@example.test", "commit", "-qm", "workspace binding")
	linked := filepath.Join(t.TempDir(), "linked")
	git(repository, "worktree", "add", "--detach", linked)
	bind(linked)
	for _, dir := range []string{repository, linked} {
		assert.Equal(t, "stopped_local_profile", diagnose(dir)["state"])
	}
	out, code := run(personalHome, workspace, nil, "daemon", "locate", "--json")
	require.NotZero(t, code, out)
	assert.Contains(t, out, "stopped_local_profile")
	out, code = run(personalHome, workspace, nil, "health", "--json")
	require.NotZero(t, code, out)
	assert.Contains(t, out, "stopped_local_profile")
	// Recovery cannot treat the same-named personal shadow as the work project.
	out, code = run(personalHome, workspace, nil, "daemon", "recover", "--expect-project-uid", personalProject.UID, "--json")
	require.NotZero(t, code, out)
	assert.Contains(t, out, "wrong_project")
	t.Cleanup(func() { _, _ = run(workHome, external, nil, "daemon", "stop") })
	out, code = run(personalHome, workspace, []string{"KATA_AUTH_TOKEN=personal-token", "PORT=8888"}, "daemon", "recover", "--expect-project-uid", workProject.UID, "--json")
	require.Zero(t, code, out+"\n"+workLog.String())
	var recovered map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &recovered))
	assert.Equal(t, "ready", recovered["state"])
	assert.Equal(t, workUID, recovered["observed_instance_uid"])
	assert.Equal(t, workProject.UID, recovered["project_uid"])
	workHub.Close()
	out, code = run(personalHome, workspace, []string{"KATA_AUTH_TOKEN=personal-token"}, "show", shortID, "--json")
	require.Zero(t, code, out+"\n"+workLog.String())
	assert.Contains(t, out, "existing replica issue")
	// A selected tunnel URL stays pinned and cannot be recovered as a profile.
	out, code = run(personalHome, workspace, []string{"KATA_SERVER=http://127.0.0.1:1"}, "daemon", "diagnose", "--json")
	require.Zero(t, code, out)
	assert.Contains(t, out, "unknown_loopback_endpoint")
	out, code = run(personalHome, workspace, []string{"KATA_SERVER=http://127.0.0.1:1"}, "daemon", "recover")
	require.NotZero(t, code, out)
	personalResult := diagnose(external, "--project", "spoke-project")
	assert.Equal(t, "ready", personalResult["state"])
	assert.Equal(t, personalUID, personalResult["observed_instance_uid"])
	assert.Equal(t, personalProject.UID, personalResult["project_uid"])
	assert.Equal(t, personalHub.URL, personalResult["hub_origin"])
	assert.NotEqual(t, float64(personalProcess.Process.Pid), recovered["pid"])
	assert.NotNil(t, personalProcess.Process)
	waitForPing(t, "http://"+personalAddr, 2*time.Second)
	t.Logf("personal process remained running: %d; logs: %s", personalProcess.Process.Pid, personalLog.String())
}
