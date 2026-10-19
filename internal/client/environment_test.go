package client

import (
	"os"
	"testing"

	"go.kenn.io/kata/internal/localprofilestore"
	"go.kenn.io/kata/internal/testfix"
)

// The kata binary installs local profile storage at startup; these tests
// exercise the same configuration.
func TestMain(m *testing.M) {
	UseLocalProfileStorage(localprofilestore.Storage{})
	os.Exit(testfix.RunIsolated(m))
}
