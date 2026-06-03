package e2e_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestE2ETelemetryDisabledForSubprocesses(t *testing.T) {
	assert.Equal(t, "0", os.Getenv("KATA_TELEMETRY_ENABLED"))
}
