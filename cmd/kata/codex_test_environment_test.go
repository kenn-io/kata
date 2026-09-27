package main

import (
	"fmt"
	"os"
	"testing"
)

// Existing workspace-hook tests assume no user hook. Isolate the test process
// from the developer's real Codex config; user-hook tests select their own homes
// with t.Setenv. No production behavior or real user config is changed.
func TestMain(m *testing.M) {
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
	code := m.Run()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "remove test Codex home: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
