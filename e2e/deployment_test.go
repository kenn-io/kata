//go:build !windows

package e2e_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// Contract: the stock binary serves an env-only deployment with health,
// private persisted auth, local CLI writes, backups and a restart, without TOML.
func TestVanillaDeploymentEnvironmentOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs isolated daemon binaries")
	}
	bin := buildKataBinary(t)
	home, workspace := t.TempDir(), t.TempDir()
	port := freeTCPPort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	env, err := config.LocalProfileEnvironment(config.LocalProfileConfig{Home: home}, false)
	require.NoError(t, err)
	env = append(env, "KATA_DB="+filepath.Join(home, "kata.db"),
		fmt.Sprintf("KATA_LISTEN=0.0.0.0:%d", port), "KATA_TRUST_PRIVATE_NETWORK=1", "KATA_WEB_PUBLIC_ORIGIN="+base,
		"KATA_BACKUP_DIR="+filepath.Join(home, "backups"), "KATA_BACKUP_INTERVAL=100ms", "KATA_BACKUP_RETAIN=1h", "KATA_TELEMETRY_ENABLED=off")
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "daemon", "start", "--foreground") //nolint:gosec // test-owned binary
		cmd.Env, cmd.Dir = env, workspace
		stderr := &safeBuffer{}
		cmd.Stderr = stderr
		cmd.Stdout = io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(stderr.String())
			}
			if cmd.ProcessState == nil {
				stopDaemon(cmd)
			}
		})
		waitForPing(t, base, 10*time.Second)
		return cmd
	}
	first := start()
	data, err := os.ReadFile(filepath.Join(home, "auth-token")) //nolint:gosec // G304: test-owned home
	require.NoError(t, err)
	token := strings.TrimSpace(string(data))
	require.Len(t, token, 64)
	info, err := os.Stat(filepath.Join(home, "auth-token"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	httpClient := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/api/v1/ping", "/api/v1/health"} {
		resp, err := httpClient.Get(base + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	resp, err := httpClient.Get(base + "/api/v1/projects")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/projects", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	resp, err = httpClient.Do(request)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	create := exec.Command(bin, "projects", "create", "example-project") //nolint:gosec // test-owned binary
	create.Env, create.Dir = env, workspace
	output, err := create.CombinedOutput()
	require.NoError(t, err, string(output))
	//nolint:kennlint // The subprocess uses a real clock; its published filesystem state cannot be observed with synctest.
	require.Eventually(t, func() bool {
		files, err := filepath.Glob(filepath.Join(home, "backups", "*", "*.jsonl"))
		if err != nil {
			return false
		}
		for _, file := range files {
			data, err := os.ReadFile(file) //nolint:gosec // G304: snapshots beneath the test-owned home
			if err == nil && bytes.Contains(data, []byte("example-project")) {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	require.True(t, os.IsNotExist(err))
	stopDaemon(first)
	second := start()
	reused, err := os.ReadFile(filepath.Join(home, "auth-token")) //nolint:gosec // G304: test-owned home
	require.NoError(t, err)
	require.Equal(t, data, reused)
	stopDaemon(second)
}
