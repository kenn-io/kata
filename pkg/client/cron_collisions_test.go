package client_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronCollisionSDKPresence(t *testing.T) {
	for _, name := range []string{"zero-until", "zero-at", "missing-until", "null-until", "missing-at", "null-at", "invalid-peer"} {
		t.Run(name, func(t *testing.T) {
			item := map[string]any{"at": "0001-01-01T00:00:00Z", "local": "0001-01-01T00:00:00Z", "occurrences": 1, "peer_count": 1, "peers": []any{map[string]any{"uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA", "name": "Peer"}}}
			report := map[string]any{"until": "0001-01-08T00:00:00Z", "items": []any{item}, "skipped": 0, "truncated": false}
			from := "0001-01-01T00:00:00Z"
			switch name {
			case "zero-until":
				from = "0000-12-25T00:00:00Z"
				report["until"], report["items"] = "0001-01-01T00:00:00Z", []any{}
			case "missing-until":
				delete(report, "until")
			case "null-until":
				report["until"] = nil
			case "missing-at":
				delete(item, "at")
			case "null-at":
				item["at"] = nil
			case "invalid-peer":
				item["peers"] = []any{map[string]any{}}
			}
			raw, err := json.Marshal(map[string]any{"valid": true, "name": "Inspect", "definition": map[string]any{}, "errors": []any{}, "warnings": []any{}, "schedule": map[string]any{"delivery": "adapter", "basis": "adapter_trigger", "zone": "UTC", "from": from, "next": []any{}, "collisions": report}})
			require.NoError(t, err)
			var result generated.CronValidationResult
			require.NoError(t, json.Unmarshal(raw, &result))
			err = result.Validate()
			if name == "zero-until" || name == "zero-at" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "collision presence and nested validation are required")
			}
		})
	}
}
