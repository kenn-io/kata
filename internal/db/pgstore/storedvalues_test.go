package pgstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoredTimeScanAcceptsEveryStoredLayout(t *testing.T) {
	want := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		src  any
	}{
		{"rfc3339nano", "2026-05-23T12:00:00.000000000Z"},
		{"rfc3339", "2026-05-23T12:00:00Z"},
		{"space offset", "2026-05-23 12:00:00+00:00"},
		{"space naive fractional", "2026-05-23 12:00:00.000000000"},
		{"space naive", "2026-05-23 12:00:00"},
		{"canonical bytes", []byte("2026-05-23T12:00:00.000Z")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got storedTime
			require.NoError(t, got.Scan(tc.src))
			assert.True(t, time.Time(got).Equal(want),
				"scanned %v, want %v", time.Time(got), want)
		})
	}
}

func TestStoredTimeValueEmitsCanonicalFormatOnly(t *testing.T) {
	// A non-UTC, sub-millisecond input must still write the one layout the
	// created_at indexes sort by; that is the read/write asymmetry this type
	// exists to close.
	zone := time.FixedZone("plus-two", 2*60*60)
	value, err := storedTime(time.Date(2026, 5, 23, 14, 0, 0, 999999, zone)).Value()
	require.NoError(t, err)
	assert.Equal(t, "2026-05-23T12:00:00.000Z", value)
	assert.Equal(t, time.Time(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)).Format(storedTimeFormat), value)
}

func TestStoredTimeScanRejectsUnparseableText(t *testing.T) {
	var got storedTime
	assert.Error(t, got.Scan("not-a-timestamp"))
	assert.Error(t, got.Scan(int64(7)))
}

func TestStoredNullTimeRoundTripsNull(t *testing.T) {
	existing := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := storedNullTime{Time: &existing}
	require.NoError(t, got.Scan(nil))
	assert.Nil(t, got.Time, "SQL NULL must leave the target pointer nil")

	value, err := storedNullTime{}.Value()
	require.NoError(t, err)
	assert.Nil(t, value)
}

func TestStoredNullTimeRoundTripsValue(t *testing.T) {
	var got storedNullTime
	require.NoError(t, got.Scan("2026-05-23T12:00:00.000Z"))
	require.NotNil(t, got.Time)
	assert.True(t, got.Time.Equal(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)))

	value, err := got.Value()
	require.NoError(t, err)
	assert.Equal(t, "2026-05-23T12:00:00.000Z", value)
}

func TestStoredBoolRoundTripsZeroAndOne(t *testing.T) {
	var got storedBool
	require.NoError(t, got.Scan(int64(0)))
	assert.False(t, bool(got))
	require.NoError(t, got.Scan(int64(1)))
	assert.True(t, bool(got))

	value, err := storedBool(true).Value()
	require.NoError(t, err)
	assert.Equal(t, int64(1), value)
	value, err = storedBool(false).Value()
	require.NoError(t, err)
	assert.Equal(t, int64(0), value)
}

func TestStoredBoolRejectsOutOfRangeIntegers(t *testing.T) {
	var got storedBool
	assert.Error(t, got.Scan(int64(2)), "CHECK(x IN (0,1)) must not be widened in Go")
	assert.Error(t, got.Scan(int64(-1)))
	assert.Error(t, got.Scan("1"))
	assert.Error(t, got.Scan(nil))
}

func TestStoredTimeFormatIsStableAcrossRoundTrip(t *testing.T) {
	// The ORDER BY created_at indexes depend on write output being the same
	// string for a value that was read back from the database.
	const canonical = "2026-05-23T12:00:00.000Z"
	var got storedTime
	require.NoError(t, got.Scan(canonical))
	value, err := got.Value()
	require.NoError(t, err)
	assert.Equal(t, canonical, value)
}
