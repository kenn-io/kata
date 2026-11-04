package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronValidateCollisionOutput(t *testing.T) {
	f := newCronFixture(t)
	definition := cronDefinition()
	definition["trigger"] = map[string]any{"kind": "cron", "cron": "0 9 * * *"}
	var peerDefinition cron.JobDefinition
	require.NoError(t, json.Unmarshal(mustJSON(t, definition), &peerDefinition))
	peerDefinition.Enabled = true
	peer, _, err := f.store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.project.ID, Name: "Daily peer", Definition: peerDefinition, Actor: "operator"})
	require.NoError(t, err)
	zeroPeer := peerDefinition
	zeroPeer.Trigger = cron.Trigger{Kind: "once", At: "0001-01-01T00:00:00Z"}
	_, _, err = f.store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.project.ID, Name: "Zero peer", Definition: zeroPeer, Actor: "operator"})
	require.NoError(t, err)
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	result := cronOK(t, f.session, "cron_job_validate", map[string]any{"name": "Candidate", "definition": definition, "count": 1, "from": "2026-11-03T00:00:00Z"})
	require.Equal(t, true, result["valid"])
	schedule := result["schedule"].(map[string]any)
	report, ok := schedule["collisions"].(map[string]any)
	require.True(t, ok, "MCP must retain collision output from the daemon")
	items := report["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	require.EqualValues(t, 7, item["occurrences"])
	require.Equal(t, peer.UID, item["peers"].([]any)[0].(map[string]any)["uid"])
	require.Equal(t, "paused", result["warnings"].([]any)[0].(map[string]any)["code"])
	require.Len(t, schedule["next"], 1)
	// Present zero timestamps must survive the generated client and MCP wire.
	definition["trigger"] = map[string]any{"kind": "once", "at": "0001-01-01T00:00:00Z"}
	for _, from := range []string{"0000-12-25T00:00:00Z", "0001-01-01T00:00:00Z"} {
		zero := cronOK(t, f.session, "cron_job_validate", map[string]any{"name": "Candidate", "definition": definition, "from": from})
		require.Equal(t, true, zero["valid"])
		report := zero["schedule"].(map[string]any)["collisions"].(map[string]any)
		if from == "0000-12-25T00:00:00Z" {
			require.Equal(t, "0001-01-01T00:00:00Z", report["until"])
			require.Empty(t, report["items"])
		} else {
			require.Equal(t, "0001-01-01T00:00:00Z", report["items"].([]any)[0].(map[string]any)["at"])
		}
	}
	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	tools, err := f.session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name != "kata.cron_job_validate" {
			continue
		}
		var schema any
		require.NoError(t, json.Unmarshal(mustJSON(t, tool.OutputSchema), &schema))
		properties := map[string]bool{}
		var visit func(any)
		visit = func(value any) {
			switch v := value.(type) {
			case map[string]any:
				if fields, ok := v["properties"].(map[string]any); ok {
					for name := range fields {
						properties[name] = true
					}
				}
				for _, child := range v {
					visit(child)
				}
			case []any:
				for _, child := range v {
					visit(child)
				}
			}
		}
		visit(schema)
		for _, field := range []string{"collisions", "until", "items", "occurrences", "peer_count", "peers", "skipped", "truncated"} {
			require.Contains(t, properties, field)
		}
		return
	}
	t.Fatal("validation tool absent from discovery")
}
