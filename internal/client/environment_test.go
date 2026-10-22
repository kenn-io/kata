package client

import (
	"os"
	"testing"

	"go.kenn.io/kata/internal/storageinspect"
	"go.kenn.io/kata/internal/testfix"
)

// The kata binary installs its storage inspector at startup; these tests
// exercise the same configuration.
func TestMain(m *testing.M) {
	UseStorageInspector(storageinspect.Inspector{})
	os.Exit(testfix.RunIsolated(m))
}
