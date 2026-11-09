package tracker

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNamedRunClosesOnceWithCleanupContext(t *testing.T) {
	calls := 0
	ctx := WithRunCloser(t.Context(), func(cleanup context.Context) error { require.NoError(t, cleanup.Err()); calls++; return nil })
	require.NoError(t, CloseRun(ctx))
	require.NoError(t, CloseRun(ctx))
	require.Equal(t, 1, calls)
	require.NoError(t, CloseRun(t.Context()), "ordinary legacy runs need no worker cleanup")
}
