package tui

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: this optional chip describes pulls for the selected project, never
// recent push traffic, and an unavailable status read does not hide cron data.
func TestCronReplicaClient(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/7/federation/status", r.URL.Path)
		respondJSON(t, w, map[string]any{"statuses": []map[string]any{{"project_id": 7, "role": "spoke", "last_pull_success_at": "2026-05-01T11:00:00Z", "last_successful_sync_at": "2026-05-01T11:59:00Z", "last_error_at": "2026-05-01T11:30:00Z", "last_error": "pull failed"}}})
	})
	status, err := c.readCronReplica(t.Context(), 7)
	require.NoError(t, err)
	require.Equal(t, "spoke", status.role)
	require.Equal(t, snapshotFixedNow.Add(-time.Hour), *status.pulledAt)
	require.Equal(t, "pull failed", status.lastError)
}

func TestCronReplicaChip(t *testing.T) {
	defer snapshotInit(t)()
	for _, tc := range []struct {
		name  string
		value cronReplica
		want  string
	}{
		{"fresh", cronReplica{role: "spoke", pulledAt: new(snapshotFixedNow.Add(-3 * time.Minute))}, "replica · pulled 3m ago"},
		{"never", cronReplica{role: "spoke"}, "replica · never pulled"},
		{"error", cronReplica{role: "spoke", pulledAt: new(snapshotFixedNow.Add(-time.Hour)), errorAt: new(snapshotFixedNow.Add(-2 * time.Minute)), lastError: "offline"}, "replica · sync error 2m ago"},
		{"old error", cronReplica{role: "spoke", pulledAt: new(snapshotFixedNow.Add(-3 * time.Minute)), errorAt: new(snapshotFixedNow.Add(-time.Hour)), lastError: "old"}, "replica · pulled 3m ago"},
		{"hub", cronReplica{role: "hub"}, ""},
		{"standalone", cronReplica{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, stripANSI(cronReplicaChip(tc.value)))
		})
	}
}

type cronReplicaTestAPI struct {
	*cronReadTestAPI
	replica cronReplica
	err     error
	project int64
}

func (a *cronReplicaTestAPI) readCronReplica(ctx context.Context, projectID int64) (cronReplica, error) {
	a.project = projectID
	if err := ctx.Err(); err != nil {
		return cronReplica{}, err
	}
	return a.replica, a.err
}
func TestCronReplicaRefresh(t *testing.T) {
	m := cronFixture(t)
	a := &cronReplicaTestAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, replica: cronReplica{role: "spoke", pulledAt: new(snapshotFixedNow.Add(-time.Hour))}}
	p := cronData{}
	cronFetch(t.Context(), a, 7, &p)
	require.Equal(t, int64(7), a.project)
	require.Equal(t, a.replica, p.replica)
	a.err = errors.New("older daemon")
	p = cronData{}
	cronFetch(t.Context(), a, 7, &p)
	require.Empty(t, p.replica)
	require.NotEmpty(t, p.jobs)
	require.NoError(t, p.jobsErr)
	// A failed status refresh must clear an old chip instead of retaining it.
	m.cron.data.replica = a.replica
	m = m.cronLoaded(cronLoadedMsg(p))
	require.Empty(t, m.cron.data.replica)
}
func TestSnapshot_CronReplica(t *testing.T) {
	defer snapshotInit(t)()
	m := cronFixture(t)
	m.width = 120
	m.cron.data.replica = cronReplica{role: "spoke", pulledAt: new(snapshotFixedNow.Add(-3 * time.Minute))}
	assertGolden(t, "cron-jobs-replica", m.viewBody())
	m.cron.data.replica.errorAt = new(snapshotFixedNow.Add(-2 * time.Minute))
	m.cron.data.replica.lastError = "sync failed"
	assertGolden(t, "cron-jobs-replica-error", m.viewBody())
}
