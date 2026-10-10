package pagination

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCursorValidation(t *testing.T) {
	p := Position{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC), ID: 42}
	hash := Fingerprint([]string{"example-project", "open"})
	raw := Encode(p, hash)
	got, err := Decode(raw, hash)
	require.NoError(t, err)
	require.Equal(t, p, *got)
	_, err = Decode(raw, Fingerprint([]string{"example-project", "closed"}))
	require.Error(t, err)
	for _, bad := range []string{"!", raw + "=", strings.Repeat("x", 4097), base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"created_at":"2026-01-01T00:00:00Z","id":1,"filter":"` + hash + `"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"created_at":"bad","id":1,"filter":"` + hash + `"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"created_at":"2026-01-01T00:00:00Z","id":0,"filter":"` + hash + `"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"created_at":"2026-01-01T00:00:00Z","id":1,"filter":"` + hash + `","extra":true}`))} {
		_, err := Decode(bad, hash)
		require.Error(t, err, "%s", bad)
	}
	got, err = Decode("", hash)
	require.NoError(t, err)
	require.Nil(t, got)
}

// The cursor must preserve the exact creation instant and positive tie-breaker.
func FuzzCursorRoundTrip(f *testing.F) {
	f.Add(uint64(123000000), int64(42), "open")
	f.Fuzz(func(t *testing.T, nanos uint64, id int64, filters string) {
		p := Position{CreatedAt: time.Unix(0, int64(nanos%uint64(1<<62))).UTC(), ID: id}
		if p.ID <= 0 {
			p.ID = 1
		}
		hash := Fingerprint(filters)
		got, err := Decode(Encode(p, hash), hash)
		require.NoError(t, err)
		require.Equal(t, p, *got)
	})
}

// Every changed fingerprint must reject an otherwise valid continuation.
func FuzzCursorRejectChangedFilters(f *testing.F) {
	f.Add("open", "closed")
	f.Fuzz(func(t *testing.T, a, b string) {
		if a == b {
			return
		}
		p := Position{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: 1}
		_, err := Decode(Encode(p, Fingerprint([]byte(a))), Fingerprint([]byte(b)))
		require.Error(t, err)
	})
}

func FuzzCursorDecode(f *testing.F) {
	f.Add("garbage")
	f.Add(Encode(Position{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: 1}, Fingerprint("open")))
	f.Fuzz(func(t *testing.T, raw string) {
		hash := Fingerprint("open")
		p, err := Decode(raw, hash)
		if err == nil && raw != "" {
			require.Positive(t, p.ID)
			require.Equal(t, raw, Encode(*p, hash), fmt.Sprint(p))
		}
	})
}

// Distinct invalid UTF-8 bytes have distinct JSON binary representations.
func TestCursorRejectDistinctBinaryFilters(t *testing.T) {
	p := Position{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: 1}
	_, err := Decode(Encode(p, Fingerprint([]byte{0xe5})), Fingerprint([]byte{0x82}))
	require.Error(t, err)
}

// Nil project selection means unrestricted; an explicit empty set means none.
func TestFingerprintDistinguishesNilAndEmptyProjectScopes(t *testing.T) {
	require.NotEqual(t, Fingerprint([]int64(nil)), Fingerprint([]int64{}))
}
