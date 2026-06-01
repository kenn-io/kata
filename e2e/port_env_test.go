//go:build !windows

package e2e_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPortEnvBind_ServesAndShutsDownCleanly walks the platform-$PORT
// path end-to-end: with PORT set in the environment, the daemon binds
// 0.0.0.0:$PORT, /api/v1/health returns 200 unauthenticated, the
// protected /api/v1/projects route gates on the bearer token, and the
// daemon exits cleanly on SIGTERM.
func TestPortEnvBind_ServesAndShutsDownCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	bin := buildKataBinary(t)

	port := freeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	home := t.TempDir()
	const token = "e2e-token"

	stderr := &safeBuffer{}
	//nolint:gosec // G204: bin is buildKataBinary's output
	cmd := exec.Command(bin, "daemon", "start")
	cmd.Env = append(os.Environ(),
		"KATA_HOME="+home,
		"KATA_DB="+filepath.Join(home, "kata.db"),
		"KATA_AUTH_TOKEN="+token,
		"KATA_TRUST_PRIVATE_NETWORK=1",
		// Defensively clear the marker so a polluted parent env can't
		// turn this into a Unix-socket run.
		"KATA_AUTOSTART=",
		fmt.Sprintf("PORT=%d", port),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon stderr:\n%s", stderr.String())
		}
		// Fallback teardown if the test bailed before SIGTERMing itself.
		if cmd.ProcessState == nil {
			stopDaemon(cmd)
		}
	})

	waitForPing(t, "http://"+addr, 5*time.Second)

	client := &http.Client{Timeout: 2 * time.Second}

	healthResp, err := client.Get("http://" + addr + "/api/v1/health") //nolint:noctx
	require.NoError(t, err)
	_ = healthResp.Body.Close()
	assert.Equal(t, http.StatusOK, healthResp.StatusCode,
		"unauthenticated /api/v1/health should 200")

	noTokenResp, err := client.Get("http://" + addr + "/api/v1/projects") //nolint:noctx
	require.NoError(t, err)
	_ = noTokenResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, noTokenResp.StatusCode,
		"protected GET without token should 401")

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/projects", nil) //nolint:noctx
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	authResp, err := client.Do(req) //nolint:gosec // G107: URL is "http://127.0.0.1:<free-port>" composed from freeTCPPort.
	require.NoError(t, err)
	_ = authResp.Body.Close()
	assert.NotEqual(t, http.StatusUnauthorized, authResp.StatusCode,
		"protected GET with valid bearer should not 401")

	// Graceful SIGTERM: the daemon's signal.NotifyContext wiring cancels
	// the root context, which triggers httpSrv.Shutdown. A clean exit is
	// code 0; we assert no error from cmd.Wait().
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		assert.NoErrorf(t, waitErr,
			"daemon should exit cleanly on SIGTERM (stderr: %s)", stderr.String())
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("daemon did not exit within 5s of SIGTERM (stderr: %s)", stderr.String())
	}
}

// TestPortEnvBind_AutostartMarkerSuppresses verifies the marker
// honored by daemonclient.autoStart: when KATA_AUTOSTART=1 is set, a
// stray PORT in the environment must not flip the daemon onto wildcard
// TCP. The daemon falls back to the platform-default local transport,
// the runtime file appears under KATA_HOME, and PORT remains unbound.
func TestPortEnvBind_AutostartMarkerSuppresses(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	bin := buildKataBinary(t)

	port := freeTCPPort(t)
	home := t.TempDir()

	env := append(os.Environ(),
		"KATA_HOME="+home,
		"KATA_DB="+filepath.Join(home, "kata.db"),
		"KATA_AUTOSTART=1",
		fmt.Sprintf("PORT=%d", port),
	)
	stderr := startDaemon(t, bin, env)

	// Wait for the daemon to publish its runtime file — proof that it
	// finished startup. The Unix-socket path writes the file just like
	// the TCP path; absence here would mean startup failed.
	runtimeGlob := filepath.Join(home, "runtime", "*", "daemon.*.json")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(runtimeGlob)
		if len(matches) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	matches, _ := filepath.Glob(runtimeGlob)
	require.NotEmptyf(t, matches,
		"daemon never wrote a runtime file (stderr: %s)", stderr.String())

	// The key assertion: PORT must remain unbound. If suppression had
	// failed, the daemon would be accepting on this address.
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 250*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatalf("PORT %d is bound; auto-start marker should have suppressed the PORT-env path (stderr: %s)",
			port, stderr.String())
	}
}
