package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Contract: answers use all retained evidence with exact project/job/revision
// identity, and state is derived from current observations on every read.
func checkCronRequestReads(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p, job, _ := requestJobFixture(t, s, "request-read-project")
	other, e := s.CreateProject(ctx, "other-read-project")
	require.NoError(t, e)
	id := requestUIDAt(t, now)
	admitted, e := s.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: id, Kind: "run", TargetUID: job.UID, Actor: "operator", Now: now})
	require.NoError(t, e)
	state := func(at time.Time, want string) {
		t.Helper()
		rows, e := s.ListCronRequests(ctx, db.CronRequestList{ProjectID: p.ID, Now: at})
		require.NoError(t, e)
		require.Len(t, rows, 1)
		require.Equal(t, want, rows[0].State)
		require.Equal(t, id, rows[0].UID)
	}
	state(now, "pending")
	state(admitted.Request.ExpiresAt, "expired")
	// A current-revision answer does not answer the pinned older revision.
	pinned := job.DefinitionEventUID
	job.Name = "Updated definition"
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: pinned, Actor: "operator"})
	require.NoError(t, e)
	occurrence := "request:" + id
	run, e := s.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: requestUIDAt(t, now), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "operator", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, e)
	state(now, "pending")
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, e = q.ExecContext(ctx, `UPDATE cron_runs SET definition_event_uid=$1,occurrence_key=$2 WHERE uid=$3`, pinned, "request:"+requestUIDAt(t, now), run.Run.UID)
	require.NoError(t, e)
	state(now, "pending")
	_, e = q.ExecContext(ctx, `UPDATE cron_runs SET occurrence_key=$1 WHERE uid=$2`, occurrence, run.Run.UID)
	require.NoError(t, e)
	// More than the recent-run page must not hide an older exact answer.
	for range 105 {
		_, e = s.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: requestUIDAt(t, now), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: "running", Summary: cron.Summary{Version: 1}})
		require.NoError(t, e)
	}
	state(now, "answered")
	state(now.Add(time.Hour), "answered")
	for _, filter := range []string{"answered", "pending", "expired"} {
		rows, e := s.ListCronRequests(ctx, db.CronRequestList{ProjectID: p.ID, State: filter, Now: now.Add(time.Hour)})
		require.NoError(t, e)
		if filter == "answered" {
			require.Len(t, rows, 1)
		} else {
			require.Empty(t, rows)
		}
	}
	_, e = s.ListCronRequests(ctx, db.CronRequestList{ProjectID: p.ID, State: "unknown", Now: now})
	require.ErrorIs(t, e, cron.ErrInvalid)
	rows, e := s.ListCronRequests(ctx, db.CronRequestList{ProjectID: other.ID, Now: now})
	require.NoError(t, e)
	require.Empty(t, rows)
	// Cancel answers can return to pending when a later observation is running.
	cancelID := requestUIDAt(t, now)
	_, e = s.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: cancelID, Kind: "cancel", TargetUID: run.Run.UID, Actor: "operator", Now: now})
	require.NoError(t, e)
	for _, status := range []string{"running", "succeeded", "running"} {
		_, e = q.ExecContext(ctx, `UPDATE cron_runs SET status=$1 WHERE uid=$2`, status, run.Run.UID)
		require.NoError(t, e)
		rows, e = s.ListCronRequests(ctx, db.CronRequestList{ProjectID: p.ID, Now: now})
		require.NoError(t, e)
		for _, row := range rows {
			if row.UID == cancelID {
				want := "pending"
				if status != "running" {
					want = "answered"
				}
				require.Equal(t, want, row.State)
			}
		}
	}
	// Replicas read their local retained values, even while federation is disabled.
	_, e = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: p.UID, Actor: "operator", Enabled: false})
	require.NoError(t, e)
	rows, e = s.ListCronRequests(ctx, db.CronRequestList{ProjectID: p.ID, Now: now})
	require.NoError(t, e)
	require.Len(t, rows, 2)
	return nil
}

func checkCronRequestSweep(t *testing.T, s db.Storage) error {
	t.Run("adopt after root collection", func(t *testing.T) {
		p, job, _ := requestJobFixture(t, s, "request-collected-project")
		now := time.Now().UTC()
		id := requestUIDAt(t, now)
		out, e := s.RequestCron(t.Context(), db.RequestCronIn{ProjectID: p.ID, UID: id, Kind: "run", TargetUID: job.UID, Actor: "operator", TTLSeconds: 60, Now: now})
		require.NoError(t, e)
		hub, e := uid.New()
		require.NoError(t, e)
		in := db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: hub, ReplayHorizonEventID: 1, Actor: "operator"}
		_, e = s.AdoptProjectIntoFederation(t.Context(), in)
		require.ErrorIs(t, e, db.ErrFederatedSpokeUnsupported)
		event, e := s.SweepCronRequests(t.Context(), p.ID, out.Request.ExpiresAt)
		require.NoError(t, e)
		require.NotNil(t, event)
		_, e = s.AdoptProjectIntoFederation(t.Context(), in)
		require.NoError(t, e)
	})
	ctx := t.Context()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p, job, _ := requestJobFixture(t, s, "request-sweep-project")
	expired := requestUIDAt(t, now)
	first, e := s.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: expired, Kind: "run", TargetUID: job.UID, Actor: "operator", TTLSeconds: 60, Now: now})
	require.NoError(t, e)
	live := requestUIDAt(t, now)
	_, e = s.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: live, Kind: "run", TargetUID: job.UID, Actor: "operator", TTLSeconds: 600, Now: now})
	require.NoError(t, e)
	// Before the deadline cleanup is silent.
	early, e := s.SweepCronRequests(ctx, p.ID, now)
	require.NoError(t, e)
	require.Nil(t, early)
	// Legacy malformed data is retained and reported while valid expiry progresses.
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	p, e = s.ProjectByID(ctx, p.ID)
	require.NoError(t, e)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(p.Metadata), &values))
	if values == nil {
		values = make(map[string]jsontext.Value)
	}
	values["cron.request.bad"] = jsontext.Value(`{"kind":"run"}`)
	values["ordinary"] = jsontext.Value(`12345678901234567890`)
	raw, e := json.Marshal(values)
	require.NoError(t, e)
	_, e = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), p.ID)
	require.NoError(t, e)
	_, e = s.ListCronRequests(ctx, db.CronRequestList{ProjectID: p.ID, Now: now})
	require.ErrorIs(t, e, cron.ErrInvalid)
	ids, e := s.ListCronRequestProjects(ctx)
	require.NoError(t, e)
	require.Contains(t, ids, p.ID)
	event, e := s.SweepCronRequests(ctx, p.ID, first.Request.ExpiresAt)
	require.Error(t, e)
	require.NotNil(t, event)
	require.Equal(t, "project.metadata_updated", event.Type)
	require.Equal(t, "system", event.Actor)
	require.Contains(t, string(event.Payload), "cron.request."+expired)
	require.NotContains(t, string(event.Payload), "cron.request."+live)
	p, e = s.ProjectByID(ctx, p.ID)
	require.NoError(t, e)
	values = nil
	require.NoError(t, json.Unmarshal([]byte(p.Metadata), &values))
	require.NotContains(t, values, "cron.request."+expired)
	require.Contains(t, values, "cron.request."+live)
	require.Contains(t, values, "cron.request.bad")
	require.Equal(t, "12345678901234567890", string(values["ordinary"]))
	event, e = s.SweepCronRequests(ctx, p.ID, first.Request.ExpiresAt)
	require.Error(t, e)
	require.Nil(t, event)
	// A stale expiry candidate must recheck authority under the project lock.
	_, e = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: p.UID, Actor: "operator", Enabled: false})
	require.NoError(t, e)
	event, e = s.SweepCronRequests(ctx, p.ID, now.Add(time.Hour))
	require.NoError(t, e)
	require.Nil(t, event)
	p, e = s.ProjectByID(ctx, p.ID)
	require.NoError(t, e)
	require.Contains(t, string(p.Metadata), "cron.request."+live)
	return nil
}
