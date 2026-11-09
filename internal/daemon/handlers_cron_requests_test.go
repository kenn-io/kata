package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// Contract: admission publishes exactly its committed event, an exact retry
// replays without one, and store refusals map to their documented codes.
func TestCronRequestsHTTP(t *testing.T) {
	sink := &recordingSink{}
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(c *daemon.ServerConfig) { c.Hooks = sink })
	p := seedProject(t, env, "request-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Actor: "operator", Name: "Execute review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	base := fmt.Sprintf("/api/v1/projects/%d/cron", p.ID)
	path := base + "/jobs/" + job.UID + "/requests"
	status, raw := cronHTTP(t, env, http.MethodGet, base+"/requests", nil)
	require.Equal(t, 200, status, string(raw))
	require.JSONEq(t, `{"requests":[]}`, string(raw))
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	body := map[string]any{"uid": cronHTTPUID(t), "actor": "operator", "ttl_seconds": 60}
	for _, replayed := range []bool{false, true} {
		status, raw = cronHTTP(t, env, http.MethodPost, path, body)
		require.Equal(t, 200, status, string(raw))
		var out db.RequestCronOut
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Equal(t, replayed, out.Replayed)
		require.Equal(t, body["uid"], out.Request.UID)
		events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: p.ID, AfterID: before, Limit: 10})
		require.NoError(t, err)
		require.Len(t, events, 1)
		if replayed {
			require.Nil(t, out.Event)
			continue
		}
		require.Equal(t, events[0], *out.Event)
		require.Equal(t, events[0], *receiveMsg(t, sub.Ch, time.Second, "run request event").Event)
		captured := sink.snapshot()
		require.Equal(t, events[0], captured[len(captured)-1])
	}
	// A changed TTL proves ttl_seconds reaches the stored request identity.
	body["ttl_seconds"] = 61
	status, raw = cronHTTP(t, env, http.MethodPost, path, body)
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "request_conflict")
	expired, err := uid.FromTime(time.Now().Add(-time.Minute))
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"uid": expired, "actor": "operator"})
	require.Equal(t, 410, status, string(raw))
	require.Contains(t, string(raw), "request_expired")
	status, raw = cronHTTP(t, env, http.MethodGet, base+"/requests?state=pending", nil)
	require.Equal(t, 200, status, string(raw))
	var list struct {
		Requests []db.CronRequest `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Requests, 1)
	require.Equal(t, "pending", list.Requests[0].State)
	status, raw = cronHTTP(t, env, http.MethodGet, base+"/requests?state=unknown", nil)
	require.Equal(t, 400, status, string(raw))
	// Pause refuses new run requests but leaves cancel admission available.
	_, err = env.DB.SetCronProjectPause(t.Context(), db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"uid": cronHTTPUID(t), "actor": "operator"})
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "request_unavailable")
	run, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: cronHTTPUID(t), Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPost, base+"/runs/"+run.Run.UID+"/cancel-request", map[string]any{"uid": cronHTTPUID(t), "actor": "operator"})
	require.Equal(t, 200, status, string(raw))
	var cancel db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &cancel))
	require.Equal(t, "cancel", cancel.Request.Kind)
	require.Equal(t, run.Run.UID, *cancel.Request.RunUID)
}
