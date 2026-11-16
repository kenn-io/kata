package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerdictEvidenceWire(t *testing.T) {
	for _, kind := range []string{"confirmed", "refuted"} {
		t.Run(kind, func(t *testing.T) {
			var e Evidence
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+kind+`","comment":"c:abc123"}`), &e))
			encoded, err := json.Marshal(e)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"comment":"c:abc123"`)
		})
	}
}
