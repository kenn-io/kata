package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronValidateCollisionHTTP(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	definition := cronHTTPDefinition(t)
	definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}
	_, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Daily peer", Definition: definition, Actor: "operator"})
	require.NoError(t, err)
	definition.Enabled = false
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate?count=1&from=2026-11-03T00:00:00Z", project.ID)
	status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "Candidate", "definition": definition})
	require.Equal(t, http.StatusOK, status, string(raw))
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, true, out["valid"])
	require.Empty(t, out["errors"])
	require.Len(t, out["warnings"].([]any), 1)
	schedule := out["schedule"].(map[string]any)
	report, ok := schedule["collisions"].(map[string]any)
	require.True(t, ok, "previewable schedules need the collision report: %s", raw)
	items := report["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	require.IsType(t, float64(0), item["occurrences"])
	require.IsType(t, float64(0), item["peer_count"])
	require.IsType(t, "", item["local"])
	require.Len(t, schedule["next"], 1, "count does not shorten the collision horizon")
	require.Equal(t, "paused", out["warnings"].([]any)[0].(map[string]any)["code"])
}
