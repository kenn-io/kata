package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinishTransactionRollbackRunsCleanupOnce(t *testing.T) {
	cleanupCalls := 0
	cleanupErr := errors.New("recording rejected access failed")
	cause := AfterTransactionRollback(errors.New("peer access rejected"), func(context.Context) error {
		cleanupCalls++
		return cleanupErr
	})

	first := FinishTransactionRollback(context.Background(), cause, nil)
	second := FinishTransactionRollback(context.Background(), cause, nil)

	require.ErrorIs(t, first, ErrTransactionFinalizationFailed)
	require.ErrorIs(t, first, cleanupErr)
	require.ErrorIs(t, second, ErrTransactionFinalizationFailed)
	require.ErrorIs(t, second, cleanupErr)
	require.Equal(t, 1, cleanupCalls)
}
