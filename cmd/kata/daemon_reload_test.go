package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonReload_NoRunningDaemon_ExitUsage(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	_, _, err := executeRootCapture(t, t.Context(), "daemon", "reload")
	require.Error(t, err)
	var ce *cliError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, ExitUsage, ce.ExitCode)
	assert.Contains(t, strings.ToLower(ce.Message), "no daemon")
}
