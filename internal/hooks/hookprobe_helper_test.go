package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	hookprobeOnce sync.Once
	hookprobeBin  string
	hookprobeErr  error
)

// hookprobePath returns the absolute path to the built hookprobe binary,
// building it on first call. Subsequent calls reuse the same binary.
func hookprobePath(t testing.TB) string {
	t.Helper()
	hookprobeOnce.Do(func() {
		// Not t.TempDir(): cleanup must outlive whichever test wins the sync.Once
		// race; TestMain handles removal explicitly below.
		dir, err := os.MkdirTemp("", "hookprobe-")
		if err != nil {
			hookprobeErr = err
			return
		}
		out := filepath.Join(dir, "hookprobe")
		if runtime.GOOS == "windows" {
			out += ".exe" // Windows won't exec a binary without a recognized extension
		}
		// Test-only build of an in-tree helper; args are constants.
		cmd := exec.Command("go", "build", "-o", out, "./hookprobe") //nolint:gosec // test build
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			hookprobeErr = fmt.Errorf("go build ./hookprobe: %w: %s", err, stderr.String())
			return
		}
		hookprobeBin = out
	})
	if hookprobeErr != nil {
		t.Fatalf("build hookprobe: %v", hookprobeErr)
	}
	return hookprobeBin
}

// TestMain cleans up the cached binary directory.
func TestMain(m *testing.M) {
	code := m.Run()
	if hookprobeBin != "" {
		_ = os.RemoveAll(filepath.Dir(hookprobeBin))
	}
	os.Exit(code)
}

// runHookprobe builds an exec.Cmd for the hookprobe binary, optionally wires
// stdin and extra env vars, runs it, and returns captured stdout/stderr along
// with the resolved exit code.
func runHookprobe(t testing.TB, stdin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(hookprobePath(t), args...) //nolint:gosec // bin is the test-built helper
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := exitCode(t, cmd.Run())
	return stdout.String(), stderr.String(), exit
}

func TestHookprobe_StdinEcho(t *testing.T) {
	stdout, _, exit := runHookprobe(t, "hello\n", nil, "stdin")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if stdout != "hello\n" {
		t.Fatalf("stdin echo = %q, want %q", stdout, "hello\n")
	}
}

func TestHookprobe_ExitCode(t *testing.T) {
	_, _, exit := runHookprobe(t, "", nil, "exit", "7")
	if exit != 7 {
		t.Fatalf("exit code = %d, want 7", exit)
	}
}

func TestHookprobe_EnvKey(t *testing.T) {
	stdout, _, exit := runHookprobe(t, "", []string{"KATA_TEST_X=hello-world"}, "env", "KATA_TEST_X")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if strings.TrimSpace(stdout) != "hello-world" {
		t.Fatalf("env value = %q, want %q", stdout, "hello-world")
	}
}

func TestHookprobe_Both(t *testing.T) {
	stdout, stderr, exit := runHookprobe(t, "", nil, "both", "outline", "errline")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !strings.Contains(stdout, "outline") {
		t.Fatalf("stdout = %q, want outline", stdout)
	}
	if !strings.Contains(stderr, "errline") {
		t.Fatalf("stderr = %q, want errline", stderr)
	}
}

func exitCode(t testing.TB, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("not an exit error: %v", err)
	return -1
}
