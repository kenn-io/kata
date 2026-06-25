package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func purgeProjectPath(projectID int64) string {
	return fmt.Sprintf("/api/v1/projects/%d/actions/purge", projectID)
}

// TestPurgeProjectHandler_PurgesArchived exercises the purgeProject route
// across the happy path and every gated failure. Each subtest bootstraps its
// own server so project state never leaks between cases.
func TestPurgeProjectHandler_PurgesArchived(t *testing.T) {
	t.Run("archived project purges and frees the name", func(t *testing.T) {
		h, pid := bootstrapProject(t)
		ts := h.ts.(*httptest.Server)
		archiveProject(t, h, pid, false)

		resp := postWithHeader(t, ts, purgeProjectPath(pid),
			map[string]string{"X-Kata-Confirm": "PURGE kata"},
			map[string]any{"actor": "tester"})
		require.Equalf(t, 200, resp.status, "purge status: %s", string(resp.body))

		var body struct {
			ProjectPurgeLog struct {
				ProjectName string `json:"project_name"`
			} `json:"project_purge_log"`
		}
		require.NoError(t, json.Unmarshal(resp.body, &body), string(resp.body))
		assert.Equal(t, "kata", body.ProjectPurgeLog.ProjectName)

		_, err := h.DB().ProjectByNameIncludingArchived(context.Background(), "kata")
		assert.True(t, errors.Is(err, db.ErrNotFound),
			"name should be freed after purge, got err=%v", err)
	})

	t.Run("re-purging a gone project returns 404", func(t *testing.T) {
		h, pid := bootstrapProject(t)
		ts := h.ts.(*httptest.Server)
		archiveProject(t, h, pid, false)

		first := postWithHeader(t, ts, purgeProjectPath(pid),
			map[string]string{"X-Kata-Confirm": "PURGE kata"},
			map[string]any{"actor": "tester"})
		require.Equalf(t, 200, first.status, "first purge: %s", string(first.body))

		second := postWithHeader(t, ts, purgeProjectPath(pid),
			map[string]string{"X-Kata-Confirm": "PURGE kata"},
			map[string]any{"actor": "tester"})
		assertAPIError(t, second.status, second.body, 404, "project_not_found")
	})

	t.Run("active project with correct confirm returns 409", func(t *testing.T) {
		h, pid := bootstrapProject(t)
		ts := h.ts.(*httptest.Server)

		resp := postWithHeader(t, ts, purgeProjectPath(pid),
			map[string]string{"X-Kata-Confirm": "PURGE kata"},
			map[string]any{"actor": "tester"})
		assertAPIError(t, resp.status, resp.body, 409, "project_not_archived")
	})

	t.Run("missing confirm header returns 412", func(t *testing.T) {
		h, pid := bootstrapProject(t)
		ts := h.ts.(*httptest.Server)
		archiveProject(t, h, pid, false)

		resp := postWithHeader(t, ts, purgeProjectPath(pid),
			nil, map[string]any{"actor": "tester"})
		assertAPIError(t, resp.status, resp.body, 412, "confirm_required")
	})

	t.Run("wrong confirm header returns 412", func(t *testing.T) {
		h, pid := bootstrapProject(t)
		ts := h.ts.(*httptest.Server)
		archiveProject(t, h, pid, false)

		resp := postWithHeader(t, ts, purgeProjectPath(pid),
			map[string]string{"X-Kata-Confirm": "PURGE wrong-name"},
			map[string]any{"actor": "tester"})
		assertAPIError(t, resp.status, resp.body, 412, "confirm_mismatch")
	})

	t.Run("federated project returns 409 with role detail", func(t *testing.T) {
		h, pid := bootstrapProject(t)
		ts := h.ts.(*httptest.Server)
		archiveProject(t, h, pid, false)
		// A federation binding blocks purge with a role-aware error.
		_, err := h.DB().ExecContext(context.Background(),
			`INSERT INTO federation_bindings(project_id, role, hub_project_uid, enabled, push_enabled)
			 VALUES(?, 'hub', '0000000000000000000000000A', 1, 0)`, pid)
		require.NoError(t, err)

		resp := postWithHeader(t, ts, purgeProjectPath(pid),
			map[string]string{"X-Kata-Confirm": "PURGE kata"},
			map[string]any{"actor": "tester"})
		assertAPIError(t, resp.status, resp.body, 409, "project_federated")

		var envelope struct {
			Error struct {
				Data map[string]any `json:"data"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(resp.body, &envelope), string(resp.body))
		assert.Equal(t, "hub", envelope.Error.Data["role"])
	})
}

// TestPurgeProjectHandler_BroadcastsResetToScopedSubscribers locks in the
// SubFilter scoping the purge depends on: a global subscriber and a subscriber
// scoped to the purged project both receive the reset frame, while a subscriber
// scoped to an unrelated project receives nothing.
func TestPurgeProjectHandler_BroadcastsResetToScopedSubscribers(t *testing.T) {
	bcast := daemon.NewEventBroadcaster()
	h, pid := bootstrapProject(t, withBroadcaster(bcast))
	ts := h.ts.(*httptest.Server)

	// Seed an event so the purge reserves a reset cursor.
	resp, bs := postJSON(t, ts, fmt.Sprintf("/api/v1/projects/%d/issues", pid),
		map[string]any{"actor": "tester", "title": "doomed"})
	require.Equalf(t, 200, resp.StatusCode, "seed issue: %s", string(bs))

	archiveProject(t, h, pid, true)

	globalSub := bcast.Subscribe(daemon.SubFilter{})
	defer globalSub.Unsub()
	purgedSub := bcast.Subscribe(daemon.SubFilter{ProjectID: pid})
	defer purgedSub.Unsub()
	otherSub := bcast.Subscribe(daemon.SubFilter{ProjectID: pid + 1000})
	defer otherSub.Unsub()

	purge := postWithHeader(t, ts, purgeProjectPath(pid),
		map[string]string{"X-Kata-Confirm": "PURGE kata"},
		map[string]any{"actor": "tester"})
	require.Equalf(t, 200, purge.status, "purge status: %s", string(purge.body))

	globalMsg := receiveMsg(t, globalSub.Ch, time.Second, "global reset")
	assert.Equal(t, "reset", globalMsg.Kind)
	assert.Equal(t, pid, globalMsg.ProjectID)
	assert.NotZero(t, globalMsg.ResetID)

	purgedMsg := receiveMsg(t, purgedSub.Ch, time.Second, "purged-project reset")
	assert.Equal(t, "reset", purgedMsg.Kind)
	assert.Equal(t, pid, purgedMsg.ProjectID)
	assert.NotZero(t, purgedMsg.ResetID)

	assertNoReceive(t, otherSub.Ch, 100*time.Millisecond, "unrelated-project reset")
}
