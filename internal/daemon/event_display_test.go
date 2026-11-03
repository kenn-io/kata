package daemon

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestEventDisplayContribution(t *testing.T) {
	for _, tc := range []struct{ kind, payload, want string }{
		{"issue.updated", `{"attributed_teammate":"writer"}`, "writer"},
		{"issue.commented", `{"teammate":"legacy"}`, "legacy"},
		{"issue.created", `{"metadata":{"teammate":"legacy"}}`, "legacy"},
		{"issue.updated", `{"teammate":"route"}`, ""},
		{"issue.commented", `{"attributed_teammate":"bad/name","teammate":"route"}`, ""},
	} {
		t.Run(tc.kind+tc.payload, func(t *testing.T) {
			event := db.Event{Type: tc.kind, Payload: tc.payload, Actor: "actor", ContentHash: "unchanged"}
			envelope := eventToEnvelope(event)
			data, err := json.Marshal(envelope)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(data, &fields))
			if tc.want == "" {
				require.NotContains(t, fields, "attributed_teammate")
			} else {
				require.Equal(t, tc.want, fields["attributed_teammate"])
			}
			require.Equal(t, tc.payload, string(envelope.Payload))
			require.Equal(t, "unchanged", envelope.ContentHash)
		})
	}
}
