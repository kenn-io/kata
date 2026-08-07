package telemetry

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKitPostHogDisabledBuildTagDisablesStandaloneBinary(t *testing.T) {
	scratch := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, filepath.WalkDir(scratch, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o700)
		}))
	})
	binary := filepath.Join(scratch, "telemetry-testprogram")
	home := filepath.Join(scratch, "home")
	command := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-o", binary, "./testprogram")
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg-config"),
		"GOCACHE="+filepath.Join(scratch, "gocache"),
		"GOMODCACHE="+filepath.Join(scratch, "gomodcache"),
		"GOPATH="+filepath.Join(scratch, "gopath"),
	)
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "build standalone test program: %s", output)

	command = exec.Command(binary)
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
