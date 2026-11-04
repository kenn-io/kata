package client_test

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Explicit empty steps are v2 evidence; typed clients must preserve that
// presence on writes and on response re-encoding without adding it to v1.
func TestCronSummaryClientPreservesEmptySteps(t *testing.T) {
	for _, raw := range []string{`{"version":1}`, `{"version":1,"steps":[]}`} {
		for _, response := range []bool{false, true} {
			var value any = &generated.Summary{}
			if response {
				value = &generated.SummaryResponse{}
			}
			require.NoError(t, json.Unmarshal([]byte(raw), value))
			for _, marshal := range []func(any) ([]byte, error){jsonv1.Marshal, func(value any) ([]byte, error) { return json.Marshal(value) }} {
				encoded, err := marshal(value)
				require.NoError(t, err)
				var want, got map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(raw), &want))
				require.NoError(t, json.Unmarshal(encoded, &got))
				require.Equal(t, want, got)
			}
		}
	}
}
