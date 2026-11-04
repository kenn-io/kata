package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCronDispatchNullableObservations(t *testing.T) {
	raw, err := json.Marshal(CronDispatchExplanation{})
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(raw, &value))
	for _, field := range []string{"last", "last_non_idle"} {
		require.Contains(t, value, field)
		require.Nil(t, value[field])
	}
}
