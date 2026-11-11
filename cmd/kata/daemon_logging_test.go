package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
)

func TestForegroundDaemonLoggingProcess(t *testing.T) {
	mode := os.Getenv("KATA_LOGGING_HELPER")
	if mode == "" {
		return
	}
	if mode == "entrypoint" {
		os.Args = []string{"kata", "daemon", "start", "--foreground", "--listen", "not-a-host-port"}
		main()
		return
	}
	if mode == "startup-panic" {
		resolveDaemonExecutable = func() (string, error) { panic("fixture fatal diagnostic") }
		_ = runDaemonWithListen(t.Context(), "not-a-host-port", false, true)
		return
	}
	oldOutput := log.Writer()
	writer, _, closeLog := startDaemonLogging()
	fmt.Fprintln(writer, "fixture plain diagnostic")
	slog.Warn("fixture structured diagnostic")
	log.Print("fixture standard diagnostic")
	closeLog()
	require.Equal(t, oldOutput, log.Writer())
	fmt.Fprintln(os.Stderr, "fixture stderr after cleanup")
}

func TestForegroundDaemonEntrypointErrorOnce(t *testing.T) {
	for _, sameFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-file=%t", sameFile), func(t *testing.T) {
			setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("PORT", "")
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(ns.DataDir, 0o700))
			logPath := filepath.Join(ns.DataDir, "daemon.log")
			stderrPath := filepath.Join(t.TempDir(), "stderr")
			if sameFile {
				stderrPath = logPath
			}
			stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			t.Cleanup(func() { _ = stderr.Close() })
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestForegroundDaemonLoggingProcess$")
			child.Env = append(os.Environ(), "KATA_LOGGING_HELPER=entrypoint")
			child.Stderr = stderr
			require.Error(t, child.Run())
			for _, path := range []string{stderrPath, logPath} {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, 1, strings.Count(string(data), "invalid --listen value"), "%s: %s", path, data)
				assert.NotContains(t, string(data), "kata daemon: kata daemon:")
			}
		})
	}
}

// Logging and crash configuration are process-global, so each mode runs in its
// own child so the startup panic never reaches the test runner.
func TestForegroundDaemonLoggingSinks(t *testing.T) {
	for _, mode := range []string{"discarded", "same-file", "stderr-failed", "startup-panic", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "stderr-failed" && runtime.GOOS != "linux" {
				t.Skip("fixture requires /dev/full")
			}
			setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(ns.DataDir, 0o700))
			path := filepath.Join(ns.DataDir, "daemon.log")
			if mode == "unavailable" {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else if mode == "same-file" {
				require.NoError(t, os.WriteFile(path, []byte("previous diagnostic\n"), 0o600))
			}
			// Detached children inherit this same append-only file as stderr.
			stderrPath := filepath.Join(t.TempDir(), "stderr")
			if mode == "same-file" {
				stderrPath = path
			} else if mode == "stderr-failed" {
				stderrPath = "/dev/full"
			}
			stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			defer stderr.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestForegroundDaemonLoggingProcess$")
			child.Env = append(os.Environ(), "KATA_LOGGING_HELPER="+mode, "GOTRACEBACK=all")
			if mode != "discarded" {
				child.Stderr = stderr
			}
			err = child.Run()
			if mode == "startup-panic" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if mode == "unavailable" {
				contents, readErr := os.ReadFile(stderrPath)
				require.NoError(t, readErr)
				assert.Contains(t, string(contents), "daemon.log")
				assert.Contains(t, string(contents), "fixture plain diagnostic")
				return
			}
			contents, err := os.ReadFile(path)
			require.NoError(t, err)
			if mode != "startup-panic" {
				for _, diagnostic := range []string{"fixture plain diagnostic", "fixture structured diagnostic", "fixture standard diagnostic"} {
					assert.Equal(t, 1, strings.Count(string(contents), diagnostic), diagnostic)
				}
			}
			if mode == "same-file" {
				assert.Contains(t, string(contents), "previous diagnostic")
			}
			if mode == "startup-panic" {
				assert.Contains(t, string(contents), "panic: fixture fatal diagnostic")
				assert.Contains(t, string(contents), "goroutine ")
			}
			info, err := os.Stat(path)
			require.NoError(t, err)
			if mode != "same-file" && runtime.GOOS != "windows" {
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
		})
	}
}

// A blocked synchronous stderr must not prevent the current diagnostic from
// reaching the retained log first.
func TestDaemonDiagnosticWriterRetainsBeforeBlockedStderr(t *testing.T) {
	var retained bytes.Buffer
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	writer := daemonDiagnosticWriter{stderr: blockingDiagnosticWriter{entered, release}, file: &retained}
	go func() {
		defer close(done)
		_, _ = writer.Write([]byte("fixture diagnostic"))
	}()
	<-entered
	assert.Equal(t, "fixture diagnostic", retained.String())
	close(release)
	<-done
}

type blockingDiagnosticWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w blockingDiagnosticWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}
