package tui

import (
	"fmt"
	"os"
	"testing"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/storageinspect"
	"go.kenn.io/kata/internal/testfix"
	"go.kenn.io/kata/internal/version"
)

func TestMain(m *testing.M) {
	version.Version = "v1.2.3"
	for _, key := range []string{"CI", "DO_NOT_TRACK", "TELEMETRY_ENABLED", "KATA_TELEMETRY_ENABLED"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	// The kata binary installs its storage inspector at startup; reconnect
	// tests that restart a local profile need the same configuration.
	client.UseStorageInspector(storageinspect.Inspector{})
	os.Exit(testfix.RunIsolated(m))
}
