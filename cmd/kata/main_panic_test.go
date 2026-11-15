package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The explicit CLI contract is that a panic is an internal error (exit 1),
// including before PersistentPreRunE has resolved the output mode.
func TestCLIPanicExit(t *testing.T) {
	for _, phase := range []string{"initialize", "finalize"} {
		for _, mode := range []string{"human", "json", "agent"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				stdout, stderr, code := runCLIPanicProcess(t, phase, "version", "--format", mode)
				assert.Equal(t, ExitInternal, code, stderr)
				if phase == "initialize" {
					assert.Empty(t, stdout)
				}
				assert.NotContains(t, stderr, "fixture-secret")
				assert.NotContains(t, stderr, "goroutine")
				assert.Contains(t, stderr, "report")
				switch mode {
				case "json":
					var envelope struct {
						Error struct {
							Kind     string `json:"kind"`
							ExitCode int    `json:"exit_code"`
							Hint     string `json:"hint"`
						} `json:"error"`
					}
					require.NoError(t, json.Unmarshal([]byte(stderr), &envelope))
					assert.Equal(t, "internal", envelope.Error.Kind)
					assert.Equal(t, ExitInternal, envelope.Error.ExitCode)
					assert.Contains(t, envelope.Error.Hint, "report")
				case "agent":
					assert.Contains(t, stderr, "ERR ")
					assert.Contains(t, stderr, " internal: ")
				default:
					assert.Contains(t, stderr, "internal error")
				}
			})
		}
	}
}

func TestForegroundDaemonPanicIsUnrecovered(t *testing.T) {
	_, stderr, code := runCLIPanicProcess(t, "daemon", "daemon", "start", "--foreground")
	assert.Equal(t, 2, code, stderr)
	assert.Contains(t, stderr, "panic: fixture-secret")
	assert.Contains(t, stderr, "goroutine")
}

func runCLIPanicProcess(t *testing.T, phase string, args ...string) (string, string, int) {
	t.Helper()
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], args...) //nolint:gosec // G204: current test executable with fixture arguments.
	child.Dir = home
	child.Env = []string{
		"KATA_TEST_CLI_PANIC=" + phase,
		"KATA_HOME=" + home,
		"HOME=" + home,
		"USERPROFILE=" + home,
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"DO_NOT_TRACK=1",
		"KATA_TELEMETRY_ENABLED=0",
	}
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	require.NoError(t, ctx.Err(), stderr.String())
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, stderr.String())
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

// TestMain dispatches here before testing's panic recovery or fixture setup.
// Cobra's lifecycle hooks provide panic injection without a production flag.
func runCLIPanicFixture() {
	crash := func() { panic("fixture-secret: request body must not be printed") }
	switch os.Getenv("KATA_TEST_CLI_PANIC") {
	case "initialize":
		cobra.OnInitialize(crash)
	case "finalize":
		cobra.OnFinalize(crash)
	case "daemon":
		runDaemonForeground = func(context.Context, string, bool, bool) error {
			crash()
			return nil
		}
	}
	main()
}
