package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonReload_NoRunningDaemon_ExitUsage(t *testing.T) { //nolint:paralleltest // setupKataEnv sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	resetFlags(t)
	setupKataEnv(t)

	_, _, err := executeRootCapture(t, context.Background(), "daemon", "reload")
	require.Error(t, err)
	var ce *cliError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, ExitUsage, ce.ExitCode)
	assert.Contains(t, strings.ToLower(ce.Message), "no daemon")
}
