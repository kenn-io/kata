package githubsync

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSince(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""}, {"  ", ""}, {"2026-01-01", "2026-01-01T00:00:00Z"},
		{" 2026-01-02T01:30:00+01:30 ", "2026-01-02T00:00:00Z"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseSince(tc.input)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tc.want, got.Format(time.RFC3339))
			}
		})
	}
	for _, input := range []string{"yesterday", "2026-02-30", "2026-01-01T00:00:00", "2026-01-01T00:00:00.001Z", "2026-01-01T00:00:00.0000000001Z", "2026-01-01T00:00:00.000Z"} {
		t.Run(input, func(t *testing.T) { _, err := ParseSince(input); require.Error(t, err) })
	}
}

func TestSinceConfigRoundtripAndLegacy(t *testing.T) {
	raw := []byte(`{"host":"github.com","owner":"example-owner","repo":"example-repo"}`)
	cfg, err := DecodeConfig(raw)
	require.NoError(t, err)
	since, err := cfg.SinceTime()
	require.NoError(t, err)
	assert.Nil(t, since)
	cfg.Since = "2026-01-01"
	encoded, err := EncodeConfig(cfg)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, json.Unmarshal(encoded, &values))
	assert.Equal(t, "2026-01-01T00:00:00Z", values["since"])
	cfg.Since = "bad"
	_, err = EncodeConfig(cfg)
	require.Error(t, err)
	_, err = DecodeConfig([]byte(`{"host":"github.com","owner":"example-owner","repo":"example-repo","since":"bad"}`))
	require.Error(t, err)
}

func FuzzParseSince(f *testing.F) {
	for _, seed := range []string{"2026-01-01", "2026-01-02T01:30:00+01:30", "bad", "", "2026-01-01T00:00:00.001Z", "2026-01-01T00:00:00.0000000001Z", "0000-01-01T00:00:00+01:00", "9999-12-31T23:00:00-02:00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := ParseSince(input)
		if err != nil || got == nil {
			return
		}
		roundtrip, err := ParseSince(got.Format(time.RFC3339))
		require.NoError(t, err)
		require.NotNil(t, roundtrip)
		assert.True(t, got.Equal(*roundtrip), "accepted timestamp loses precision on roundtrip")
		assert.Equal(t, 0, got.Nanosecond())
	})
}

func TestParseSinceRejectsOffsetsOutsideRFC3339UTCYears(t *testing.T) {
	for _, input := range []string{"0000-01-01T00:00:00+01:00", "9999-12-31T23:00:00-02:00"} {
		_, err := ParseSince(input)
		require.Error(t, err, "cutoff must remain RFC3339 after UTC normalization")
	}
}
