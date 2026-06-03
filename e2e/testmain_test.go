package e2e_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("KATA_TELEMETRY_ENABLED", "0")
	os.Exit(m.Run())
}
