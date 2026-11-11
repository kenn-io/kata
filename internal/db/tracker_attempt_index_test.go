package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrackerAttemptKeyBoundsCoverOnlyCanonicalSequences(t *testing.T) {
	fence := TrackerExecutionFence{
		RootInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ProjectUID:      "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		Role:            "executor",
		WriteUID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	lower, upper := trackerAttemptKeyBounds(fence)
	prefix := trackerAttemptPrefix(fence)
	require.Equal(t, prefix+"00000000000000000001", lower)
	require.Equal(t, prefix+"09223372036854775808", upper)
	for _, sequence := range []uint64{1, 2, 999, 1<<63 - 1} {
		fence.Sequence = sequence
		key := trackerAttemptKey(fence)
		require.Equal(t, fmt.Sprintf("%s%020d", prefix, sequence), key)
		require.GreaterOrEqual(t, key, lower)
		require.Less(t, key, upper)
	}
	require.Less(t, prefix+"00000000000000000000", lower)
	require.GreaterOrEqual(t, prefix+"09223372036854775808", upper)
}
