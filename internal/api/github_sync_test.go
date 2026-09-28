package api_test

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

func TestIssueSyncIntervalPresence(t *testing.T) {
	for _, decode := range []struct {
		name string
		fn   func([]byte, any) error
	}{
		{"v1", jsonv1.Unmarshal}, {"v2", func(b []byte, v any) error { return json.Unmarshal(b, v) }},
	} {
		t.Run(decode.name, func(t *testing.T) {
			for _, tc := range []struct {
				raw     string
				present bool
				seconds int
			}{
				{`{}`, false, 0}, {`{"interval_seconds":0}`, true, 0}, {`{"interval_seconds":-1}`, true, -1}, {`{"interval_seconds":120}`, true, 120},
			} {
				var got api.EnableIssueSyncRequestBody
				require.NoError(t, decode.fn([]byte(tc.raw), &got))
				require.Equal(t, tc.present, got.IntervalSecondsPresent)
				require.Equal(t, tc.seconds, got.IntervalSeconds)
			}
		})
	}
}
