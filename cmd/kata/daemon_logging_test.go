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
	if mode == "startup-panic" || mode == "panic-discarded" {
		resolveDaemonExecutable = func() (string, error) { panic("fixture fatal diagnostic") }
		_ = runDaemonWithListen(t.Context(), "not-a-host-port", false, true)
		return
	}
	if mode == "restart-after" {
		writer, _, closeLog := startDaemonLogging(t.Context())
		fmt.Fprintln(writer, "fixture after exec")
		fmt.Fprintln(os.Stderr, "fixture direct after exec")
		closeLog()
		return
	}
	oldOutput := log.Writer()
	writer, _, closeLog := startDaemonLogging(t.Context())
	fmt.Fprintln(writer, "fixture plain diagnostic")
	fmt.Fprintln(os.Stderr, "fixture direct stderr")
	if mode == "restart" {
		require.NoError(t, os.Setenv("KATA_LOGGING_HELPER", "restart-after"))
		err := reexecDaemon(t.Context(), os.Args[0], []string{"-test.run=^TestForegroundDaemonLoggingProcess$"}, os.Environ(), nil)
		require.NoError(t, err)
	}
	slog.Warn("fixture structured diagnostic")
	log.Print("fixture standard diagnostic")
	closeLog()
	require.Equal(t, oldOutput, log.Writer())
	fmt.Fprintln(os.Stderr, "fixture stderr after cleanup")
}

// Logging and crash configuration are process-global, so each mode runs in its
// own child so the startup panic never reaches the test runner.
func TestForegroundDaemonLoggingSinks(t *testing.T) {
	for _, mode := range []string{"discarded", "restart", "separate", "same-file", "stderr-failed", "startup-panic", "panic-discarded", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "restart" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
				t.Skip("in-place exec fixture requires Linux or macOS")
			}
			if mode == "stderr-failed" && runtime.GOOS != "linux" {
				t.Skip("fixture requires /dev/full")
			}
			setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("PORT", "")
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
			if mode != "discarded" && mode != "restart" && mode != "panic-discarded" {
				child.Stderr = stderr
			}
			err = child.Run()
			if mode == "startup-panic" || mode == "panic-discarded" {
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
			if mode != "startup-panic" && mode != "panic-discarded" && mode != "restart" {
				for _, diagnostic := range []string{"fixture plain diagnostic", "fixture structured diagnostic", "fixture standard diagnostic"} {
					assert.Equal(t, 1, strings.Count(string(contents), diagnostic), diagnostic)
				}
			}
			if mode == "separate" {
				stderrContents, err := os.ReadFile(stderrPath)
				require.NoError(t, err)
				assert.Contains(t, string(stderrContents), "fixture direct stderr")
				assert.Contains(t, string(stderrContents), "fixture plain diagnostic")
				assert.NotContains(t, string(contents), "fixture direct stderr")
			}
			if mode == "restart" {
				for _, diagnostic := range []string{"fixture direct stderr", "fixture after exec", "fixture direct after exec"} {
					assert.Equal(t, 1, strings.Count(string(contents), diagnostic), diagnostic)
				}
			}
			if mode == "discarded" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin") {
				assert.Equal(t, 1, strings.Count(string(contents), "fixture direct stderr"))
			}
			if mode == "same-file" {
				assert.Equal(t, 1, strings.Count(string(contents), "fixture direct stderr"))
				assert.Contains(t, string(contents), "previous diagnostic")
			}
			if mode == "startup-panic" || mode == "panic-discarded" {
				assert.Contains(t, string(contents), "panic: fixture fatal diagnostic")
				assert.Contains(t, string(contents), "goroutine ")
			}
			if mode == "separate" || mode == "same-file" || (mode == "discarded" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin")) {
				entrypoint := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestForegroundDaemonLoggingProcess$")
				entrypoint.Env = append(os.Environ(), "KATA_LOGGING_HELPER=entrypoint")
				if mode != "discarded" {
					entrypoint.Stderr = stderr
				}
				require.Error(t, entrypoint.Run())
				paths := []string{path}
				if mode == "separate" {
					paths = append(paths, stderrPath)
				}
				for _, outputPath := range paths {
					data, err := os.ReadFile(outputPath)
					require.NoError(t, err)
					assert.Equal(t, 1, strings.Count(string(data), "invalid --listen value"), "%s: %s", outputPath, data)
					assert.NotContains(t, string(data), "kata daemon: kata daemon:")
				}
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
