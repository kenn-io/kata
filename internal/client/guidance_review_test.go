package client

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoveryAdviceDisplaysWindowsPathLiterally(t *testing.T) {
	err := &LocalDaemonUnreachableError{dataDir: `C:\Users\example\kata`}
	require.Contains(t, err.RecoveryAdvice(), `C:\Users\example\kata`)
}
