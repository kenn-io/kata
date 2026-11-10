package main

import (
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

// The foreground logging contract retains startup errors even when a supervisor
// discards stderr. The child uses only a synthetic home and storage namespace.
func TestForegroundDaemonRetainsStartupError(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	t.Setenv("PORT", "")
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestForegroundDaemonLoggingProcess$")
	child.Env = append(os.Environ(), "KATA_LOGGING_HELPER=startup")
	require.NoError(t, child.Run())
	data, err := os.ReadFile(filepath.Join(ns.DataDir, "daemon.log"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "not-a-host-port")
}

func TestForegroundDaemonLoggingProcess(t *testing.T) {
	mode := os.Getenv("KATA_LOGGING_HELPER")
	if mode == "" {
		return
	}
	if mode == "startup-panic" {
		resolveDaemonExecutable = func() (string, error) { panic("fixture fatal diagnostic") }
		_ = runDaemonWithListen(t.Context(), "not-a-host-port", false, true)
		return
	}
	if mode == "startup" {
		require.Error(t, runDaemonWithListen(t.Context(), "not-a-host-port", false, true))
		return
	}
	oldLogger, oldOutput, oldFlags, oldPrefix := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	writer, closeLog := startDaemonLogging(t.Context())
	fmt.Fprintln(writer, "fixture plain diagnostic")
	slog.Warn("fixture structured diagnostic")
	log.Print("fixture standard diagnostic")
	if mode == "fatal" {
		panic("fixture fatal diagnostic")
	}
	closeLog()
	require.Same(t, oldLogger, slog.Default())
	require.Equal(t, oldOutput, log.Writer())
	require.Equal(t, oldFlags, log.Flags())
	require.Equal(t, oldPrefix, log.Prefix())
	fmt.Fprintln(os.Stderr, "fixture stderr after cleanup")
}

// Logging and crash configuration are process-global, so each mode runs in its
// own child. The fatal mode never panics or signals the test runner.
func TestForegroundDaemonLoggingSinks(t *testing.T) {
	for _, mode := range []string{"discarded", "same-file", "fatal", "startup-panic", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
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
			if mode == "fatal" || mode == "startup-panic" {
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
				assert.Contains(t, string(contents), "fixture stderr after cleanup")
			}
			if mode == "fatal" || mode == "startup-panic" {
				assert.Contains(t, string(contents), "panic: fixture fatal diagnostic")
				assert.Contains(t, string(contents), "goroutine ")
			}
			info, err := os.Stat(path)
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
		})
	}
}
