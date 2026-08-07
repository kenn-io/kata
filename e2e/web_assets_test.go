//go:build !windows

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const productionWebMarker = `<meta name="kata-web-distribution" content="production"`
const webReleaseCheckEnv = "KATA_RUN_WEB_RELEASE_CHECK"

func TestWebReleaseEnvironmentReplacesInheritedDaemonTargets(t *testing.T) {
	env := webReleaseEnvironment([]string{
		"PATH=/example/bin",
		"KATA_HOME=/srv/kata",
		"KATA_DB=/srv/kata/kata.db",
		"KATA_DSN=postgres://daemon.example/kata",
		"KATA_CONFIG=/srv/kata/config.toml",
		"KATA_SERVER=https://daemon.example",
		"KATA_WORKSPACE=/srv/example-workspace",
		"KATA_AUTH_TOKEN=secret-value",
		"PORT=8080",
	}, "/tmp/kata-web/home", "/tmp/kata-web/workspace")

	assertEnvironmentValue(t, env, "PATH", "/example/bin")
	assertEnvironmentValue(t, env, "KATA_HOME", "/tmp/kata-web/home")
	assertEnvironmentValue(t, env, "KATA_DB", "/tmp/kata-web/home/kata.db")
	assertEnvironmentValue(t, env, "KATA_WORKSPACE", "/tmp/kata-web/workspace")
	assertEnvironmentValue(t, env, "KATA_AUTH_TOKEN", "")
	assertEnvironmentValue(t, env, "KATA_SERVER", "")
	assertEnvironmentMissing(t, env, "KATA_DSN")
	assertEnvironmentMissing(t, env, "KATA_CONFIG")
	assertEnvironmentMissing(t, env, "PORT")
}

func TestReleaseBinaryContainsValidatedWebUI(t *testing.T) {
	if os.Getenv(webReleaseCheckEnv) != "1" {
		t.Skip("release asset smoke runs only through make web-release-check")
	}
	if testing.Short() {
		t.Skip("release asset smoke is slow")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("Bun is required to stage release web assets")
	}

	repositoryRoot, err := filepath.Abs("..")
	require.NoError(t, err)
	build := exec.Command("make", "web-embed")
	build.Dir = repositoryRoot
	build.Env = os.Environ()
	var buildLog bytes.Buffer
	build.Stdout = &buildLog
	build.Stderr = &buildLog
	require.NoErrorf(t, build.Run(), "stage validated web assets: %s", buildLog.String())

	bin := filepath.Join(t.TempDir(), "kata")
	compile := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-trimpath", "-buildvcs=false", "-o", bin, "./cmd/kata") //nolint:gosec // fixed build arguments and test-owned output
	compile.Dir = repositoryRoot
	compile.Env = os.Environ()
	buildLog.Reset()
	compile.Stdout = &buildLog
	compile.Stderr = &buildLog
	require.NoErrorf(t, compile.Run(), "compile release binary: %s", buildLog.String())
	verify := exec.Command(bin, "_web-assets-check") //nolint:gosec // bin is the test-owned release binary compiled above.
	verify.Env = os.Environ()
	buildLog.Reset()
	verify.Stdout = &buildLog
	verify.Stderr = &buildLog
	require.NoErrorf(t, verify.Run(), "validate assembled release binary: %s", buildLog.String())

	home := t.TempDir()
	workspace := t.TempDir()
	port := freeTCPPort(t)
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	config := fmt.Sprintf("[web]\nlisten = %q\n", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600))
	env := webReleaseEnvironment(os.Environ(), home, workspace)

	stderr := &safeBuffer{}
	//nolint:gosec // bin is the test-owned release binary compiled above.
	daemon := exec.Command(bin, "daemon", "start", "--foreground")
	daemon.Dir = workspace
	daemon.Env = env
	daemon.Stdout = io.Discard
	daemon.Stderr = stderr
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, daemon.Start())
	t.Cleanup(func() { stopDaemon(daemon) })
	waitForPing(t, origin, 10*time.Second)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}

	index := releaseGET(t, client, origin+"/")
	require.Contains(t, index, productionWebMarker)
	require.NotContains(t, index, "Kata UI assets are not built")
	assetPath := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindStringSubmatch(index)
	require.Len(t, assetPath, 2, "production index must reference a built asset")
	releaseGET(t, client, origin+assetPath[1])
	require.Contains(t, releaseGET(t, client, origin+"/kata?view=all-open"), productionWebMarker)

	waitForWebRuntime(t, home, origin, stderr)
	sessionResponse := releaseJSON(t, client, http.MethodPost, origin+"/api/v1/ui/session/local",
		map[string]string{"return_path": "/kata?view=all-open"}, map[string]string{"Origin": origin})
	var session struct {
		Session string `json:"session"`
	}
	require.NoError(t, json.Unmarshal(sessionResponse, &session))
	require.NotEmpty(t, session.Session)

	req, err := http.NewRequest(http.MethodGet, origin+"/api/v1/ui/snapshot?view=all-open", nil)
	require.NoError(t, err)
	req.Header.Set("X-Kata-Web-Session", session.Session)
	response, err := client.Do(req) //nolint:gosec // loopback origin selected by this test.
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode, "snapshot response: %s", body)
}

func webReleaseEnvironment(inherited []string, home, workspace string) []string {
	blocked := map[string]bool{
		"KATA_HOME":       true,
		"KATA_DSN":        true,
		"KATA_DB":         true,
		"KATA_CONFIG":     true,
		"KATA_SERVER":     true,
		"KATA_WORKSPACE":  true,
		"KATA_AUTH_TOKEN": true,
		"PORT":            true,
	}
	env := make([]string, 0, len(inherited)+5)
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] {
			env = append(env, entry)
		}
	}
	return append(env,
		"KATA_HOME="+home,
		"KATA_WORKSPACE="+workspace,
		"KATA_DB="+filepath.Join(home, "kata.db"),
		"KATA_AUTH_TOKEN=",
		"KATA_SERVER=",
	)
}

func assertEnvironmentValue(t *testing.T, env []string, name, want string) {
	t.Helper()
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			require.Equal(t, prefix+want, entry)
			return
		}
	}
	t.Fatalf("environment variable %s is missing", name)
}

func assertEnvironmentMissing(t *testing.T, env []string, name string) {
	t.Helper()
	prefix := name + "="
	for _, entry := range env {
		require.Falsef(t, strings.HasPrefix(entry, prefix), "environment variable %s is present", name)
	}
}

func releaseGET(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/html")
	response, err := client.Do(req) //nolint:gosec // loopback target created by the test.
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode, "GET %s: %s", target, body)
	return string(body)
}

func releaseJSON(t *testing.T, client *http.Client, method, target string, input any, headers map[string]string) []byte {
	t.Helper()
	body, err := json.Marshal(input)
	require.NoError(t, err)
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := client.Do(req) //nolint:gosec // loopback target created by the test.
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode, "%s %s: %s", method, target, responseBody)
	return responseBody
}

func waitForWebRuntime(t *testing.T, home, origin string, stderr *safeBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(home, "runtime", "*", "daemon.*.json"))
		for _, candidate := range matches {
			body, err := os.ReadFile(candidate) //nolint:gosec // glob is rooted beneath the test-owned home.
			if err != nil {
				continue
			}
			var runtime struct {
				Metadata map[string]string `json:"metadata"`
			}
			if json.Unmarshal(body, &runtime) == nil && runtime.Metadata["web_origin"] == origin {
				return candidate
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("web runtime did not become ready; daemon stderr: %s", stderr.String())
	return ""
}
