package cron

import (
	"encoding/json/v2"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// The reviewed contract uses the latest evidence time, a timeout plus 15m,
// or 24h without a timeout. Equality with the deadline is already stale.
func TestStalenessDeadline(t *testing.T) {
	updated := time.Date(2026, 11, 3, 12, 0, 0, 123, time.UTC)
	for _, tc := range []struct {
		timeout int64
		delay   time.Duration
		basis   string
	}{{3600, 75 * time.Minute, "timeout_seconds"}, {0, 24 * time.Hour, "default"}, {-1, 24 * time.Hour, "default"}} {
		deadline := updated.Add(tc.delay)
		got := Staleness("running", updated, tc.timeout, deadline.Add(-time.Nanosecond))
		require.Equal(t, deadline, got.StaleAt)
		require.Equal(t, tc.basis, got.Basis)
		require.False(t, got.Stale)
		require.True(t, Staleness("running", updated, tc.timeout, deadline).Stale)
	}
}

func TestStalenessNonRunning(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "cancelled", "unknown"} {
		t.Run(status, func(t *testing.T) {
			require.Nil(t, Staleness(status, time.Time{}, math.MaxInt64, time.Time{}))
		})
	}
}

// Integer arithmetic is the oracle, independently of duration conversions.
func TestStalenessExactDeadlineAndSaturation(t *testing.T) {
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	hegel.Test(t, func(ht *hegel.T) {
		updated := time.Unix(hegel.Draw(ht, hegel.Integers[int64](-62135596800, 253402300799)), int64(hegel.Draw(ht, hegel.Integers(0, 999999999)))).UTC()
		timeout := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		seconds := big.NewInt(86400)
		basis := "default"
		if timeout > 0 {
			seconds = new(big.Int).Add(big.NewInt(timeout), big.NewInt(900))
			basis = "timeout_seconds"
		}
		epoch := new(big.Int).Add(big.NewInt(updated.Unix()), seconds)
		want := last
		if epoch.Cmp(big.NewInt(last.Unix())) <= 0 {
			want = time.Unix(epoch.Int64(), int64(updated.Nanosecond())).UTC()
		}
		got := Staleness("running", updated, timeout, updated)
		if !got.StaleAt.Equal(want) || got.Basis != basis {
			ht.Fatalf("updated=%v timeout=%d got=%+v want=%v basis=%s", updated, timeout, got, want, basis)
		}
		if _, err := json.Marshal(got); err != nil {
			ht.Fatal(err)
		}
	}, hegel.WithTestCases(3000), hegel.WithDatabase(t.TempDir()))
}

func TestStalenessMonotoneClock(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		updated := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		timeout := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		a := hegel.Draw(ht, hegel.Integers[int64](-62135596800, 253402300799))
		b := hegel.Draw(ht, hegel.Integers[int64](a, 253402300799))
		first := Staleness("running", updated, timeout, time.Unix(a, 0))
		second := Staleness("running", updated, timeout, time.Unix(b, 0))
		if first.Stale && !second.Stale {
			ht.Fatal("advancing the clock cleared stale evidence")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
