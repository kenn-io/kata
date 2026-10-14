package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Creation status comes from the daemon's verified receipt projection. Source
// labels on pending or legacy records must never become accountable actors.
func TestShowCreationAttribution(t *testing.T) {
	for _, state := range []string{"verified", "pending", "legacy"} {
		for _, mode := range []string{"human", "agent"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				wire := []byte(`{"issue":{"short_id":"abc1","title":"Example issue","status":"open","author":"source-agent","source_actor":"source-agent","teammate":"example-worker","accountable_actor":"member-one","verification":"` + state + `"},"comments":[{"uid":"comment-one","author":"comment-agent","teammate":"comment-worker","source_actor":"comment-agent","accountable_actor":"member-two","verification":"` + state + `","body":"Example comment"}]}`)
				var response showResponseForCLI
				require.NoError(t, json.Unmarshal(wire, &response))
				var output bytes.Buffer
				if mode == "human" {
					require.NoError(t, printShowHuman(&output, response, "example-project", nil))
				} else {
					require.NoError(t, printShowAgent(&output, response, "example-project", "show"))
				}
				require.Contains(t, output.String(), "attribution="+state)
				require.Contains(t, output.String(), "source-agent / example-worker")
				require.Contains(t, output.String(), "comment-agent / comment-worker")
				if state == "verified" {
					require.Contains(t, output.String(), "accountable=member-one")
					require.Contains(t, output.String(), "accountable=member-two")
				} else {
					require.NotContains(t, output.String(), "member-one")
					require.NotContains(t, output.String(), "member-two")
				}
			})
		}
	}
}

func TestShowCreationAttributionLabelsRemainData(t *testing.T) {
	view := db.AttributionView{Verification: "unknown", SourceActor: "source-agent", AccountableActor: "untrusted-account"}
	require.Equal(t, "attribution=legacy source=source-agent", showCreationAttribution(view, "author"))
	view = db.AttributionView{Verification: "verified", SourceActor: "\x1b[31magent\x1b[0m\nlabel", Teammate: "worker\rlabel", AccountableActor: "member\tlabel"}
	value := showCreationAttribution(view, "author")
	for _, control := range []string{"\x1b", "\n", "\r", "\t"} {
		require.NotContains(t, value, control)
	}
	require.Contains(t, value, "attribution=verified")
	require.Equal(t, "", showCreationAttribution(db.AttributionView{}, "author"), "older daemons without a projection must not acquire a verified label")
}
