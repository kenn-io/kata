package activity

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLeaseReleaseIsIdempotentAndForkIsExplicit(t *testing.T) {
	var parentReleased atomic.Int32
	var childReleased atomic.Int32
	parent := NewLease(func() { parentReleased.Add(1) }, func() (*Lease, bool) {
		return NewLease(func() { childReleased.Add(1) }, nil), true
	})

	child, admitted := parent.Fork()
	require.True(t, admitted)
	parent.Release()
	parent.Release()
	child.Release()
	child.Release()

	require.Equal(t, int32(1), parentReleased.Load())
	require.Equal(t, int32(1), childReleased.Load())
}

func TestLeaseWithoutForkSourceCannotFork(t *testing.T) {
	lease := NewLease(func() {}, nil)
	child, admitted := lease.Fork()
	require.False(t, admitted)
	require.Nil(t, child)
}
