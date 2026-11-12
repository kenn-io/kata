package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/testfix"
	"go.kenn.io/kata/internal/version"
)

// Existing workspace-hook tests assume no user hook. Isolate the test process
// from the developer's real Codex config; user-hook tests select their own homes
// with t.Setenv. No production behavior or real user config is changed.
func TestMain(m *testing.M) {
	if os.Getenv("KATA_TEST_CLI_PANIC") != "" {
		runCLIPanicFixture()
		return
	}
	version.Version = "v1.2.3"
	for _, key := range []string{"CI", "DO_NOT_TRACK", "TELEMETRY_ENABLED", "KATA_TELEMETRY_ENABLED", "KATA_CLOSE_TRANSCRIPT_ENABLED"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if os.Getenv("KATA_STARTUP_FIXTURE") == "1" && len(os.Args) > 1 && os.Args[1] == "daemon" {
		runStartupFixture()
	}
	flag.Parse()
	if runtime.GOOS == "windows" {
		// Allow the Windows daemon lifecycle tests 15m, including local runs.
		if timeout := flag.Lookup("test.timeout"); timeout != nil && timeout.Value.String() == "10m0s" {
			if err := flag.Set("test.timeout", "15m"); err != nil {
				fmt.Fprintf(os.Stderr, "extend Windows test timeout: %v\n", err)
				os.Exit(1)
			}
		}
	}
	if err := useSelectedNode(); err != nil {
		fmt.Fprintf(os.Stderr, "select test Node executable: %v\n", err)
		os.Exit(1)
	}
	home, err := os.MkdirTemp("", "kata-test-codex-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test Codex home: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("CODEX_HOME", home); err != nil {
		fmt.Fprintf(os.Stderr, "set test Codex home: %v\n", err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}
	code := testfix.RunIsolated(m)
	if err := cleanupExternalFixtures(); err != nil {
		fmt.Fprintf(os.Stderr, "remove external test fixtures: %v\n", err)
		code = 1
	}
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "remove test Codex home: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// useSelectedNode puts the Node executable a version manager selects first
// on PATH. The tests run Node under a temporary HOME, where the manager's
// shim can no longer find its own configuration. Without Node on PATH the
// tests that need it skip.
func useSelectedNode() error {
	output, err := exec.CommandContext(context.Background(), "node", "-p", "process.execPath").Output()
	if err != nil {
		return nil
	}
	executable := strings.TrimSpace(string(output))
	if !filepath.IsAbs(executable) {
		return nil
	}
	return os.Setenv("PATH", filepath.Dir(executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
}
