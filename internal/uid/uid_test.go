package uid_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
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

func TestCanonicalNormalizesOnlyValidULIDs(t *testing.T) {
	const canonical = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	require.True(t, uid.Valid(canonical))

	assert.Equal(t, canonical, uid.Canonical(strings.ToLower(canonical)))
	assert.Equal(t, "legacy-Comment", uid.Canonical("legacy-Comment"))

	longS := strings.Replace(canonical, "S", "\u017F", 1)
	assert.Equal(t, longS, uid.Canonical(longS), "Unicode uppercasing must not alias malformed IDs to a valid ULID")
}

func TestCanonicalOnlyNormalizesStrictULIDsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		first := hegel.Draw(ht, hegel.SampledFrom([]string{"0", "1", "2", "3", "4", "5", "6", "7"}))
		tail := hegel.Draw(ht, hegel.Text().Alphabet("0123456789ABCDEFGHJKMNPQRSTVWXYZ").MinSize(24).MaxSize(24))
		canonical := first + "S" + tail
		require.True(ht, uid.Valid(canonical))

		mixedCase := []byte(canonical)
		lowered := false
		for i, char := range mixedCase {
			if char >= 'A' && char <= 'Z' && hegel.Draw(ht, hegel.Booleans()) {
				mixedCase[i] += 'a' - 'A'
				lowered = true
			}
		}
		if !lowered {
			mixedCase[1] += 'a' - 'A'
		}
		require.Equal(ht, canonical, uid.Canonical(string(mixedCase)))
		require.Equal(ht, canonical, uid.Canonical(uid.Canonical(string(mixedCase))))

		malformed := canonical[:1] + "\u017F" + canonical[2:]
		require.False(ht, uid.Valid(malformed))
		require.Equal(ht, malformed, uid.Canonical(malformed))
		require.NotEqual(ht, canonical, uid.Canonical(malformed))
	}, hegel.WithTestCases(200), hegel.WithDatabase(t.TempDir()))
}
