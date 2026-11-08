package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func requestJobFixture(t *testing.T, s db.Storage, name string) (db.Project, db.CronJob, db.CronWorkflow) {
	t.Helper()
	p, e := s.CreateProject(t.Context(), name)
	require.NoError(t, e)
	job, wf := nativeFederationDefinitions(t, s, p)
	job.Definition.Enabled = true
	job, _, e = s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
	require.NoError(t, e)
	return p, job, wf
}
func requestUIDAt(t *testing.T, now time.Time) string {
	t.Helper()
	id, e := uid.FromTime(now)
	require.NoError(t, e)
	return id
}

// Contract: retained exact replay precedes freshness, eligibility and capacity,
// but changed caller identity never changes the pinned request body.
func checkCronRequests(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t.Run("admission and replay", func(t *testing.T) {
		p, job, _ := requestJobFixture(t, s, "request-project")
		in := db.RequestCronIn{ProjectID: p.ID, UID: requestUIDAt(t, now), Kind: "run", TargetUID: job.UID, Actor: "operator", Now: now}
		first, e := s.RequestCron(ctx, in)
		require.NoError(t, e)
		require.False(t, first.Replayed)
		require.NotNil(t, first.Event)
		require.Equal(t, "project.metadata_updated", first.Event.Type)
		require.Equal(t, "operator", first.Event.Actor)
		require.Equal(t, job.DefinitionEventUID, first.Request.DefinitionEventUID)
		require.Equal(t, job.UID, first.Request.JobUID)
		require.Equal(t, 10*time.Minute, first.Request.ExpiresAt.Sub(first.Request.RequestedAt))
		require.True(t, now.Equal(first.Request.RequestedAt))
		require.Contains(t, string(first.Event.Payload), "cron.request."+in.UID)
		original := first.Request
		job.Definition.Enabled = false
		job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, e)
		fresh := in
		fresh.UID = requestUIDAt(t, now)
		_, e = s.RequestCron(ctx, fresh)
		require.ErrorIs(t, e, db.ErrCronRequestUnavailable, "disabled job alone refuses admission")
		job.Definition.Enabled = true
		job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, e)
		_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, e)
		before, e := s.MaxEventID(ctx)
		require.NoError(t, e)
		in.Now = now.Add(48 * time.Hour)
		again, e := s.RequestCron(ctx, in)
		require.NoError(t, e)
		require.True(t, again.Replayed)
		require.Nil(t, again.Event)
		require.Equal(t, original, again.Request)
		after, e := s.MaxEventID(ctx)
		require.NoError(t, e)
		require.Equal(t, before, after)
		for _, change := range []func(*db.RequestCronIn){func(v *db.RequestCronIn) { v.Actor = "other" }, func(v *db.RequestCronIn) { v.TTLSeconds = 60 }, func(v *db.RequestCronIn) { v.TargetUID = requestUIDAt(t, now) }, func(v *db.RequestCronIn) { v.Kind = "cancel" }} {
			changed := in
			change(&changed)
			_, e = s.RequestCron(ctx, changed)
			require.ErrorIs(t, e, db.ErrCronRequestConflict)
		}
		fresh.Now = in.Now
		fresh.UID = requestUIDAt(t, in.Now)
		_, e = s.RequestCron(ctx, fresh)
		require.ErrorIs(t, e, db.ErrCronRequestUnavailable, "project pause alone refuses admission")
		_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
		require.NoError(t, e)
		_, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator", Deleted: true})
		require.NoError(t, e)
		_, e = s.RequestCron(ctx, fresh)
		require.ErrorIs(t, e, db.ErrCronRequestUnavailable, "tombstoned job alone refuses admission")
		// Model expiry collection by removing the retained key. An old identity
		// must never be recreated even though the exact replay previously worked.
		q := s.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, e = q.ExecContext(ctx, `UPDATE projects SET metadata='{}' WHERE id=$1`, p.ID)
		require.NoError(t, e)
		_, e = s.RequestCron(ctx, in)
		require.ErrorIs(t, e, db.ErrCronRequestExpired)
	})
	t.Run("cancel while paused", func(t *testing.T) {
		p, job, wf := requestJobFixture(t, s, "cancel-project")
		_, e := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, e)
		for _, workflowOnly := range []bool{false, true} {
			observation := db.ObserveCronRun{ProjectID: p.ID, UID: requestUIDAt(t, now), Actor: "operator", Status: "running", Summary: cron.Summary{Version: 1}}
			if workflowOnly {
				observation.WorkflowUID = &wf.UID
				observation.WorkflowDefinitionEventUID = &wf.DefinitionEventUID
			} else {
				observation.JobUID = &job.UID
				observation.DefinitionEventUID = &job.DefinitionEventUID
			}
			running, e := s.ObserveCronRun(ctx, observation)
			require.NoError(t, e)
			in := db.RequestCronIn{ProjectID: p.ID, UID: requestUIDAt(t, now), Kind: "cancel", TargetUID: running.Run.UID, Actor: "operator", TTLSeconds: 60, Now: now}
			first, e := s.RequestCron(ctx, in)
			require.NoError(t, e)
			require.NotNil(t, first.Request.RunUID)
			require.Equal(t, running.Run.UID, *first.Request.RunUID)
			if workflowOnly {
				require.Empty(t, first.Request.JobUID)
			} else {
				require.Equal(t, job.UID, first.Request.JobUID)
				require.Equal(t, job.DefinitionEventUID, first.Request.DefinitionEventUID)
			}
			observation.Status = "succeeded"
			observation.ExpectedRevision = running.Run.Revision
			_, e = s.ObserveCronRun(ctx, observation)
			require.NoError(t, e)
			replay, e := s.RequestCron(ctx, in)
			require.NoError(t, e)
			require.True(t, replay.Replayed)
			in.UID = requestUIDAt(t, now)
			_, e = s.RequestCron(ctx, in)
			require.ErrorIs(t, e, db.ErrCronRequestUnavailable)
		}
	})
	t.Run("atomic capacity", func(t *testing.T) {
		store := b.Open(t)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		p, job, _ := requestJobFixture(t, store, "capacity-project")
		running, e := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: requestUIDAt(t, now), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: "running", Summary: cron.Summary{Version: 1}})
		require.NoError(t, e)
		values := map[string]any{}
		for i := range 99 {
			id, e := uid.FromStableSeed([]byte(fmt.Sprint(i)), now)
			require.NoError(t, e)
			value := map[string]any{"kind": "run", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "requested_by": "operator", "requested_at": now, "expires_at": now.Add(600 * time.Second)}
			if i%2 == 1 {
				value["kind"] = "cancel"
				value["run_uid"] = running.Run.UID
			}
			values["cron.request."+id] = value
		}
		raw, e := json.Marshal(values)
		require.NoError(t, e)
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, e = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), p.ID)
		require.NoError(t, e)
		other := b.OpenSame(t, store)
		t.Cleanup(func() { require.NoError(t, other.Close()) })
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, database := range []db.Storage{store, other} {
			id := requestUIDAt(t, now)
			wg.Go(func() {
				_, e := database.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: id, Kind: "run", TargetUID: job.UID, Actor: "operator", TTLSeconds: 60, Now: now})
				results <- e
			})
		}
		wg.Wait()
		close(results)
		success, full := 0, 0
		for e := range results {
			if e == nil {
				success++
			} else if errors.Is(e, db.ErrCronRequestCapacity) {
				full++
			} else {
				require.NoError(t, e)
			}
		}
		require.Equal(t, 1, success)
		require.Equal(t, 1, full)
		p, e = store.ProjectByID(ctx, p.ID)
		require.NoError(t, e)
		var retained map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(p.Metadata), &retained))
		require.Len(t, retained, 100)
		// Expired retained records do not consume live admission capacity.
		later := now.Add(601 * time.Second)
		_, e = store.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: requestUIDAt(t, later), Kind: "run", TargetUID: job.UID, Actor: "operator", Now: later})
		require.NoError(t, e)
	})
	t.Run("target eligibility and isolation", func(t *testing.T) {
		p, job, _ := requestJobFixture(t, s, "eligible-project")
		other, e := s.CreateProject(ctx, "other-request-project")
		require.NoError(t, e)
		_, e = s.RequestCron(ctx, db.RequestCronIn{ProjectID: other.ID, UID: requestUIDAt(t, now), Kind: "run", TargetUID: job.UID, Actor: "operator", Now: now})
		require.ErrorIs(t, e, db.ErrNotFound)
		issue, _, e := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Notification target", Author: "operator"})
		require.NoError(t, e)
		job.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: issue.UID}
		job.Definition.Action = cron.Action{Kind: "notify", Recipient: "operator", Message: "Inspect"}
		job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, e)
		_, e = s.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: requestUIDAt(t, now), Kind: "run", TargetUID: job.UID, Actor: "operator", Now: now})
		require.ErrorIs(t, e, cron.ErrInvalid)
		_, e = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: p.UID, Actor: "operator", Enabled: false})
		require.NoError(t, e)
		_, e = s.RequestCron(ctx, db.RequestCronIn{ProjectID: p.ID, UID: requestUIDAt(t, now), Kind: "run", TargetUID: job.UID, Actor: "operator", Now: now})
		require.ErrorIs(t, e, db.ErrFederatedSpokeUnsupported)
	})
	return nil
}

// Request values use exactly the same4 authenticated ingress boundaries.
func checkCronRequestsIngress(t *testing.T, store db.Storage) error {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := requestUIDAt(t, now)
	value, e := json.Marshal(map[string]any{"kind": "run", "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "requested_by": "operator", "requested_at": now, "expires_at": now.Add(time.Minute)})
	require.NoError(t, e)
	return checkCronControlIngressValue(t, store, "cron.request."+id, string(value))
}
