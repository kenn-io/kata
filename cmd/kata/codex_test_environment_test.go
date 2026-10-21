package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"

	"go.kenn.io/kata/internal/testfix"
	"go.kenn.io/kata/internal/version"
)

// Existing workspace-hook tests assume no user hook. Isolate the test process
// from the developer's real Codex config; user-hook tests select their own homes
// with t.Setenv. No production behavior or real user config is changed.
func TestMain(m *testing.M) {
	version.Version = "v1.2.3"
	for _, key := range []string{"CI", "DO_NOT_TRACK", "TELEMETRY_ENABLED", "KATA_TELEMETRY_ENABLED"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	flag.Parse()
	if runtime.GOOS == "windows" {
		// The PR job calls test.yml@main, so a branch-local workflow timeout does
		// not affect this package. Allow the Windows daemon lifecycle tests 15m.
		if timeout := flag.Lookup("test.timeout"); timeout != nil && timeout.Value.String() == "10m0s" {
			if err := flag.Set("test.timeout", "15m"); err != nil {
				fmt.Fprintf(os.Stderr, "extend Windows test timeout: %v\n", err)
				os.Exit(1)
			}
		}
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
