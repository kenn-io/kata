package uid_test

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
)

var fixedTestTime = time.Date(2026, 5, 4, 1, 2, 3, 456_000_000, time.UTC)

func TestNewReturnsValidULID(t *testing.T) {
	got, err := uid.New()
	require.NoError(t, err)
	assert.Len(t, got, 26)
	assert.True(t, uid.Valid(got))
}

func TestNewIsUniqueAndMonotonic(t *testing.T) {
	const n = 100_000
	seen := make(map[string]bool, n)
	values := make([]string, 0, n)
	for i := range n {
		got, err := uid.New()
		require.NoError(t, err)
		require.False(t, seen[got], "duplicate UID at %d: %s", i, got)
		seen[got] = true
		values = append(values, got)
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	assert.Equal(t, sorted, values)
}

func TestFromTimeEncodesTimestampWithRandomEntropy(t *testing.T) {
	a, err := uid.FromTime(fixedTestTime)
	require.NoError(t, err)
	b, err := uid.FromTime(fixedTestTime)
	require.NoError(t, err)

	assert.NotEqual(t, a, b)
	assert.Equal(t, fixedTestTime.UnixMilli(), uid.MustTime(a).UnixMilli())
	assert.Equal(t, fixedTestTime.UnixMilli(), uid.MustTime(b).UnixMilli())
}

func TestFromStableSeedIsDeterministic(t *testing.T) {
	a, err := uid.FromStableSeed([]byte("issue:7:42"), fixedTestTime)
	require.NoError(t, err)
	b, err := uid.FromStableSeed([]byte("issue:7:42"), fixedTestTime)
	require.NoError(t, err)
	c, err := uid.FromStableSeed([]byte("issue:7:43"), fixedTestTime)
	require.NoError(t, err)

	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.True(t, uid.Valid(a))
	assert.Equal(t, fixedTestTime.UnixMilli(), uid.MustTime(a).UnixMilli())
}

func TestValidAndValidPrefixRejectBadInput(t *testing.T) {
	valid, err := uid.FromStableSeed([]byte("project:1"), time.UnixMilli(1_777_777_777_000).UTC())
	require.NoError(t, err)

	assert.True(t, uid.Valid(valid))
	assert.False(t, uid.Valid(valid[:25]))
	assert.False(t, uid.Valid(valid+"0"))
	assert.False(t, uid.Valid("8"+valid[1:]))
	assert.False(t, uid.Valid(valid[:25]+"I"))

	assert.True(t, uid.ValidPrefix(valid[:1]))
	assert.True(t, uid.ValidPrefix(valid[:8]))
	assert.True(t, uid.ValidPrefix(valid))
	assert.False(t, uid.ValidPrefix(""))
	assert.False(t, uid.ValidPrefix(valid+"0"))
	assert.False(t, uid.ValidPrefix("8"))
	assert.False(t, uid.ValidPrefix(valid[:7]+"I"))
}
