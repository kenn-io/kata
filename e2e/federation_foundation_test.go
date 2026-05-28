package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// TestSmoke_FederationFoundationV3 covers spec §8.9: full pipeline from a
// freshly-initialized daemon through mutations, SSE tail, purge, and export.
// Every event observed over the wire must carry event_uid + origin_instance_uid;
// the purge_log row must carry uid + origin_instance_uid; the JSONL export
// must contain the meta.instance_uid record and v3 event/purge fields.
func TestSmoke_FederationFoundationV3(t *testing.T) {
	env := testenv.New(t)
	dir := initRepo(t, "https://github.com/wesm/federation-smoke.git")

	// 1. /api/v1/instance returns the daemon's stable identity.
	resp, err := env.HTTP.Get(env.URL + "/api/v1/instance") //nolint:gosec // test loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var inst struct {
		InstanceUID string `json:"instance_uid"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&inst))
	require.True(t, uid.Valid(inst.InstanceUID), "instance_uid %q invalid", inst.InstanceUID)

	// 2. Init project; resolve project id.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects",
		map[string]any{"start_path": dir}))
	pid := resolvePID(t, env.HTTP, env.URL, dir)
	pidStr := strconv.FormatInt(pid, 10)

	// 3. Baseline cursor so the project-creation events don't pollute the SSE
	//    capture.
	baseline := pollNextAfterID(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/events?after_id=0")

	// 4. Open SSE consumer at baseline.
	sseResp := openSSEAt(t, env.HTTP, env.URL+"/api/v1/events/stream?project_id="+pidStr+
		"&after_id="+strconv.FormatInt(baseline, 10))
	defer func() { _ = sseResp.Body.Close() }()
	framer := newSmokeFramer(sseResp.Body)

	// 5. Create two issues + a comment. The first issue is purged in step
	//    7; the second survives so step 8's JSONL export still has events
	//    to validate (a vacuous "no events left" export would pass
	//    otherwise).
	target := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "v3-smoke"})
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+target.ShortID+"/comments",
		map[string]any{"actor": "agent", "body": "smoke comment"}))
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "v3-survivor"}))

	// 6. Capture the three SSE frames; every event_uid is valid and
	//    origin_instance_uid matches the daemon's identity.
	frames := framer.NextN(t, 3, 2*time.Second)
	require.Len(t, frames, 3)
	for _, f := range frames {
		var data struct {
			EventUID          string `json:"event_uid"`
			OriginInstanceUID string `json:"origin_instance_uid"`
			HLCPhysicalMS     int64  `json:"hlc_physical_ms"`
			HLCCounter        int64  `json:"hlc_counter"`
			ContentHash       string `json:"content_hash"`
		}
		require.NoErrorf(t, json.Unmarshal([]byte(f.data), &data), "frame data: %s", f.data)
		assert.True(t, uid.Valid(data.EventUID), "event_uid %q invalid", data.EventUID)
		assert.Equal(t, inst.InstanceUID, data.OriginInstanceUID, f.event)
		assert.Greater(t, data.HLCPhysicalMS, int64(0), f.event)
		assert.GreaterOrEqual(t, data.HLCCounter, int64(0), f.event)
		assert.Regexp(t, `^[a-f0-9]{64}$`, data.ContentHash, f.event)
	}

	// 7. Purge the issue and capture the purge_log row from the response.
	purgeURL := env.URL + "/api/v1/projects/" + pidStr + "/issues/" + target.ShortID + "/actions/purge"
	pReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, purgeURL,
		strings.NewReader(`{"actor":"agent"}`))
	require.NoError(t, err)
	pReq.Header.Set("Content-Type", "application/json")
	pReq.Header.Set("X-Kata-Confirm", "PURGE federation-smoke#"+target.ShortID)
	pResp, err := env.HTTP.Do(pReq) //nolint:gosec // test loopback
	require.NoError(t, err)
	defer func() { _ = pResp.Body.Close() }()
	require.Equal(t, 200, pResp.StatusCode)

	var purgeBody struct {
		PurgeLog struct {
			UID               string `json:"uid"`
			OriginInstanceUID string `json:"origin_instance_uid"`
		} `json:"purge_log"`
	}
	require.NoError(t, json.NewDecoder(pResp.Body).Decode(&purgeBody))
	assert.True(t, uid.Valid(purgeBody.PurgeLog.UID), "purge_log.uid %q invalid", purgeBody.PurgeLog.UID)
	assert.Equal(t, inst.InstanceUID, purgeBody.PurgeLog.OriginInstanceUID)

	// 8. Export via jsonl.Export against the live daemon's DB. The JSONL must
	//    contain the meta.instance_uid record matching the daemon's identity,
	//    and every event/purge_log envelope must carry uid + origin.
	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(context.Background(), env.DB, &buf,
		jsonl.ExportOptions{IncludeDeleted: true}))

	var sawInstanceMeta, sawEvent, sawPurgeLog bool
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var env struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &env))
		switch env.Kind {
		case "meta":
			var rec struct {
				Key, Value string
			}
			require.NoError(t, json.Unmarshal(env.Data, &rec))
			if rec.Key == "instance_uid" {
				assert.Equal(t, inst.InstanceUID, rec.Value)
				sawInstanceMeta = true
			}
		case "event":
			var rec struct {
				UID               string `json:"uid"`
				OriginInstanceUID string `json:"origin_instance_uid"`
				HLCPhysicalMS     int64  `json:"hlc_physical_ms"`
				HLCCounter        int64  `json:"hlc_counter"`
				ContentHash       string `json:"content_hash"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &rec))
			assert.True(t, uid.Valid(rec.UID), "event uid %q invalid", rec.UID)
			assert.Equal(t, inst.InstanceUID, rec.OriginInstanceUID)
			assert.Greater(t, rec.HLCPhysicalMS, int64(0))
			assert.GreaterOrEqual(t, rec.HLCCounter, int64(0))
			assert.Regexp(t, `^[a-f0-9]{64}$`, rec.ContentHash)
			sawEvent = true
		case "purge_log":
			var rec struct {
				UID               string `json:"uid"`
				OriginInstanceUID string `json:"origin_instance_uid"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &rec))
			assert.True(t, uid.Valid(rec.UID), "purge_log uid %q invalid", rec.UID)
			assert.Equal(t, inst.InstanceUID, rec.OriginInstanceUID)
			sawPurgeLog = true
		}
	}
	assert.True(t, sawInstanceMeta, "JSONL export must contain a meta.instance_uid record")
	assert.True(t, sawEvent, "JSONL export must contain at least one event envelope "+
		"(otherwise the per-event uid/origin assertions are vacuous)")
	assert.True(t, sawPurgeLog, "JSONL export must contain at least one purge_log envelope "+
		"(otherwise the per-purge_log uid/origin assertions are vacuous)")
}
