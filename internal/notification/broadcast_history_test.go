package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func FuzzBroadcastHistoryOneEvent(f *testing.F) {
	f.Add(uint8(5), "message", true)
	f.Fuzz(func(t *testing.T, n uint8, message string, broadcast bool) {
		if len(message) > 1024 {
			return
		}
		diff := map[string]any{}
		for i := range int(n) {
			diff[fmt.Sprintf("notify.recipient-%d", i)] = map[string]any{"from": nil, "to": map[string]any{"from": "worker", "teammate": "builder", "message": message, "broadcast": broadcast}}
		}
		raw, e := json.Marshal(map[string]any{"diff": diff})
		if e != nil {
			return
		}
		got := BroadcastsFromPayload(raw, time.Unix(123, 0))
		if n > 0 && broadcast {
			require.Len(t, got, 1)
			require.Equal(t, "worker/builder", got[0].Sender)
			require.Equal(t, message, got[0].Message)
		} else {
			require.Empty(t, got)
		}
	})
}
func TestBroadcastHistoryDoesNotCountRemovalOrOrdinaryMetadata(t *testing.T) {
	for _, raw := range []string{`{"diff":{"notify.recipient":{"from":{"from":"worker","message":"old","broadcast":true},"to":null}}}`, `{"diff":{"other":{"to":{"from":"worker","message":"old","broadcast":true}}}}`, `{"diff":{"notify.recipient":{"to":{"from":"worker","message":"human"}}}}`} {
		require.Empty(t, BroadcastsFromPayload(jsontext.Value(raw), time.Now()))
	}
}
