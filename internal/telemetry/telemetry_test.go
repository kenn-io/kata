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

func TestKitPostHogDisabledBuildTagDisablesStandaloneBinary(t *testing.T) {
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
	home := filepath.Join(scratch, "home")
	command := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-o", binary, "./testprogram") //nolint:gosec // Fixed Go tool and arguments build the test fixture.
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg-config"),
		"GOCACHE="+filepath.Join(scratch, "gocache"),
		"GOMODCACHE="+moduleCache,
		"GOPROXY=off",
	)
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "build standalone test program: %s", output)

	command = exec.Command(binary) //nolint:gosec // The test owns and just built this binary in its scratch directory.
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg-config"),
		"TELEMETRY_ENABLED=1",
		EnabledEnv+"=1",
	)
	output, err = command.CombinedOutput()
	require.NoErrorf(t, err, "run standalone test program: %s", output)
	assert.Equal(t, "disabled", strings.TrimSpace(string(output)))
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
