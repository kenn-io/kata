package telemetry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testProgramOptOutOutput is what the standalone program prints when telemetry is
// off: the reporter is disabled and still answers an allowed app_opened capture
// with 202 disabled, so nothing is sent.
const testProgramOptOutOutput = "disabled\ncapture 202 {\"status\":\"disabled\"}"

type testProgram struct {
	binary  string
	scratch string
}

func buildTestProgram(t *testing.T, tags ...string) testProgram {
	t.Helper()
	goEnv := exec.Command("go", "env", "GOMODCACHE") //nolint:gosec // Fixed Go command resolves the caller's provisioned module cache.
	moduleCacheOutput, err := goEnv.CombinedOutput()
	require.NoErrorf(t, err, "resolve caller module cache: %s", moduleCacheOutput)
	moduleCache := strings.TrimSpace(string(moduleCacheOutput))
	require.NotEmpty(t, moduleCache)

	scratch := t.TempDir()
	binaryName := "telemetry-testprogram"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(scratch, binaryName)
	args := []string{"build"}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, "-o", binary, "./testprogram")
	command := exec.Command("go", args...) //nolint:gosec // Fixed Go tool and arguments build the test fixture.
	command.Env = append(os.Environ(),
		"HOME="+filepath.Join(scratch, "home"),
		"XDG_CACHE_HOME="+filepath.Join(scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg-config"),
		"GOCACHE="+filepath.Join(scratch, "gocache"),
		"GOMODCACHE="+moduleCache,
		"GOPROXY=off",
	)
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "build standalone test program: %s", output)
	return testProgram{binary: binary, scratch: scratch}
}

func (p testProgram) run(t *testing.T, env ...string) string {
	t.Helper()
	command := exec.Command(p.binary) //nolint:gosec // The test owns and just built this binary in its scratch directory.
	command.Env = append(os.Environ(),
		"HOME="+filepath.Join(p.scratch, "home"),
		"XDG_CACHE_HOME="+filepath.Join(p.scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(p.scratch, "xdg-config"),
	)
	command.Env = append(command.Env, env...)
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "run standalone test program: %s", output)
	return strings.TrimSpace(strings.ReplaceAll(string(output), "\r\n", "\n"))
}

func TestKitPostHogDisabledBuildTagDisablesStandaloneBinary(t *testing.T) {
	program := buildTestProgram(t, "kit_posthog_disabled")

	output := program.run(t, "TELEMETRY_ENABLED=1", EnabledEnv+"=1")

	assert.Equal(t, testProgramOptOutOutput, output)
}

func TestOptOutDropsAppOpenedInStandaloneBinary(t *testing.T) {
	program := buildTestProgram(t)
	for _, test := range []struct {
		name string
		env  []string
	}{
		{name: "kata opt-out", env: []string{EnabledEnv + "=0", "TELEMETRY_ENABLED=1"}},
		{name: "generic opt-out", env: []string{"TELEMETRY_ENABLED=0", EnabledEnv + "=1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, testProgramOptOutOutput, program.run(t, test.env...))
		})
	}
}

func TestAppOpenedSurfaceAllowlist(t *testing.T) {
	reporter, err := NewReporter(Options{DistinctID: "anonymous-instance-id"})
	require.NoError(t, err)

	for _, surface := range []string{"tui", "web"} {
		props, err := reporter.SanitizeProperties("app_opened", map[string]any{"surface": surface, "path": "/example"})
		require.NoError(t, err)
		assert.Equal(t, surface, props["surface"])
		assert.NotContains(t, props, "path")
	}
	for _, surface := range []any{"desktop", "", 1} {
		props, err := reporter.SanitizeProperties("app_opened", map[string]any{"surface": surface})
		require.NoError(t, err)
		assert.NotContainsf(t, props, "surface", "surface %#v must be dropped", surface)
	}
}

func TestEnabledFromEnvDisabledDuringGoTests(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "1")
	t.Setenv(EnabledEnv, "1")

	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledDuringGoTests(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "1")
	t.Setenv(EnabledEnv, "1")

	reporter, err := NewReporter(Options{})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func postCaptureEvent(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/ui/telemetry", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestCaptureHandlerAdmitsAppOpenedUnderGoTest(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "1")
	t.Setenv(EnabledEnv, "1")

	reporter, err := NewReporter(Options{})
	require.NoError(t, err)
	assert.False(t, reporter.Enabled())

	handler := CaptureHandler(reporter)
	accepted := postCaptureEvent(t, handler, `{"event":"app_opened"}`)
	assert.Equal(t, http.StatusAccepted, accepted.Code)
	assert.JSONEq(t, `{"status":"disabled"}`, accepted.Body.String())

	rejected := postCaptureEvent(t, handler, `{"event":"app_loaded"}`)
	assert.Equal(t, http.StatusBadRequest, rejected.Code)
}

type stubClient struct{}

func (stubClient) Enabled() bool                        { return true }
func (stubClient) Capture(string, map[string]any) error { return nil }
func (stubClient) Close() error                         { return nil }

func TestCaptureHandlerRejectsNonReporterClient(t *testing.T) {
	recorder := postCaptureEvent(t, CaptureHandler(stubClient{}), `{"event":"app_opened"}`)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}
