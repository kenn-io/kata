package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// An operator closes evidence under their own attribution while retaining the
// starter's immutable run identity. Failure paths leave both row and log intact.
func checkCronRunReap(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, p)
	id, err := uid.New()
	require.NoError(t, err)
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	in := db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Teammate: new("runner"), ExecutorLabel: new("runtime-a"), Status: "running", StartedAt: &start, Summary: cron.Summary{Version: 1, Message: "Previous progress", InputTokens: 23, OutputTokens: 7}}
	observed, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	request := db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: observed.Run.Revision, Now: observed.Run.UpdatedAt.Add(24 * time.Hour), Message: "Runtime stopped"}
	foreign, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	wrong := request
	wrong.ExpectedRevision++
	_, err = store.ReapCronRun(ctx, wrong)
	require.ErrorIs(t, err, db.ErrCronConflict)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.ReapCronRun(cancelled, request)
	require.ErrorIs(t, err, context.Canceled)
	early := request
	early.Now = observed.Run.UpdatedAt
	_, err = store.ReapCronRun(ctx, early)
	require.ErrorIs(t, err, db.ErrCronRunNotStale)
	wrong = request
	wrong.ProjectID = foreign.ID
	_, err = store.ReapCronRun(ctx, wrong)
	require.ErrorIs(t, err, db.ErrNotFound)
	current, err := store.CronRun(ctx, p.ID, id)
	require.NoError(t, err)
	require.Equal(t, observed.Run, current)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	// Rejected reap operations add no events.
	require.Equal(t, before, after)
	reaped, err := store.ReapCronRun(ctx, request)
	require.NoError(t, err)
	require.Len(t, reaped.Events, 1)
	require.Equal(t, "operator", reaped.Events[0].Actor)
	require.Equal(t, "cron.run.observed", reaped.Events[0].Type)
	require.Equal(t, "unknown", reaped.Run.Status)
	require.Equal(t, "starter", reaped.Run.Actor)
	require.Equal(t, observed.Run.Revision+1, reaped.Run.Revision)
	require.Equal(t, request.Now.UTC(), reaped.Run.UpdatedAt)
	require.Contains(t, reaped.Run.Summary.Message, "Marked unknown by operator")
	require.Contains(t, reaped.Run.Summary.Message, "Runtime stopped")
	require.Contains(t, reaped.Run.Summary.Message, "Previous: Previous progress")
	unchanged := reaped.Run
	unchanged.Status = observed.Run.Status
	unchanged.Summary.Message = observed.Run.Summary.Message
	unchanged.Revision = observed.Run.Revision
	unchanged.UpdatedAt = observed.Run.UpdatedAt
	require.Equal(t, observed.Run, unchanged)
	_, err = store.ReapCronRun(ctx, request)
	require.ErrorIs(t, err, db.ErrCronConflict, "an operator retry cannot emit another event")
	request.ExpectedRevision = reaped.Run.Revision
	request.Force = true
	_, err = store.ReapCronRun(ctx, request)
	require.ErrorIs(t, err, db.ErrCronRunNotRunning)
	// Late truth must refresh the revision after the operator's write.
	in.Status = "succeeded"
	in.ExpectedRevision = observed.Run.Revision
	_, err = store.ObserveCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrCronConflict)
	in.ExpectedRevision = reaped.Run.Revision
	_, err = store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	// Force waives only age, and summary trimming is measured after JSON escaping.
	in.UID, err = uid.New()
	require.NoError(t, err)
	in.ExpectedRevision = 0
	in.Status = "running"
	in.Summary.Message = strings.Repeat("\x01", 10900)
	in.EndedAt = new(start.Add(time.Hour))
	observed, err = store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	request.UID = in.UID
	request.ExpectedRevision = observed.Run.Revision
	request.Now = observed.Run.UpdatedAt
	reaped, err = store.ReapCronRun(ctx, request)
	require.NoError(t, err)
	raw, err := json.Marshal(reaped.Run.Summary)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), cron.SummaryLimit)
	require.Equal(t, int64(23), reaped.Run.Summary.InputTokens)
	require.Equal(t, in.EndedAt, reaped.Run.EndedAt)
	return nil
}

// Reap reads the current retained policy inside its write transaction, and
// cannot turn a failed job lookup or a read-only project into an unknown run.
func checkCronReapCurrentPolicy(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, p)
	job.Definition.TimeoutSeconds = 172800
	job, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: "starter", Definition: job.Definition})
	require.NoError(t, err)
	id, err := uid.New()
	require.NoError(t, err)
	observed, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: id, Actor: "starter", JobUID: new(job.UID), DefinitionEventUID: new(job.DefinitionEventUID), Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	in := db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: observed.Run.Revision, Now: observed.Run.UpdatedAt.Add(24 * time.Hour)}
	_, err = store.ReapCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrCronRunNotStale)
	var notStale *db.CronRunNotStaleError
	require.ErrorAs(t, err, &notStale)
	if notStale == nil {
		t.Fatal("not-stale error did not include its deadline")
	}
	require.Equal(t, observed.Run.UpdatedAt.Add(48*time.Hour+15*time.Minute), notStale.StaleAt)
	job.Definition.TimeoutSeconds = 1
	job, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: "starter", Definition: job.Definition, Deleted: true})
	require.NoError(t, err)
	in.Now = observed.Run.UpdatedAt.Add(901 * time.Second)
	reaped, err := store.ReapCronRun(ctx, in)
	require.NoError(t, err)
	require.Equal(t, observed.Run.DefinitionEventUID, reaped.Run.DefinitionEventUID)
	id, err = uid.New()
	require.NoError(t, err)
	observed, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: id, Actor: "starter", JobUID: new(job.UID), DefinitionEventUID: new(job.DefinitionEventUID), Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	in.UID = id
	in.ExpectedRevision = observed.Run.Revision
	in.Force = true
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, `{"version":1,"unexpected":true}`, job.UID)
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.ReapCronRun(ctx, in)
	require.Error(t, err)
	require.NotErrorIs(t, err, db.ErrCronRunNotStale)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	current, err := store.CronRun(ctx, p.ID, id)
	require.NoError(t, err)
	require.Equal(t, observed.Run, current)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: p.UID, Actor: "operator", Enabled: true, PushEnabled: false})
	require.NoError(t, err)
	_, err = store.ReapCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrFederatedReadOnly, "force never bypasses the project write gate")
	return nil
}

func checkCronReapRace(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, p)
	id, err := uid.New()
	require.NoError(t, err)
	input := db.ObserveCronRun{ProjectID: p.ID, UID: id, Actor: "starter", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}}
	run, err := store.ObserveCronRun(ctx, input)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := store.ReapCronRun(ctx, db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: run.Run.Revision, Force: true, Now: run.Run.UpdatedAt})
		results <- err
	}()
	go func() {
		<-start
		input.Status = "succeeded"
		input.ExpectedRevision = run.Run.Revision
		_, err := store.ObserveCronRun(ctx, input)
		results <- err
	}()
	close(start)
	a, b := <-results, <-results
	if a == nil {
		require.ErrorIs(t, b, db.ErrCronConflict)
	} else {
		require.ErrorIs(t, a, db.ErrCronConflict)
		require.NoError(t, b)
	}
	current, err := store.CronRun(ctx, p.ID, id)
	require.NoError(t, err)
	require.Equal(t, run.Run.Revision+1, current.Revision)
	require.Contains(t, []string{"unknown", "succeeded"}, current.Status)
	return nil
}

// Reap remains a standard observed event. Peers preserve portable evidence,
// and bound-actor ingest accepts the operator envelope for a known starter.
func checkCronReapFederation(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	p, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, hub, p)
	id, err := uid.New()
	require.NoError(t, err)
	initial, err := hub.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	binding, err := hub.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, err)
	baseline, err := hub.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100})
	require.NoError(t, err)
	peer := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	local, err := peer.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	adopted, err := peer.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: p.ID, HubProjectUID: p.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "operator", EmptyOnly: true})
	require.NoError(t, err)
	for _, event := range baseline {
		_, err := peer.InsertRemoteEvent(ctx, local.ID, portableNativeEvent(event))
		require.NoError(t, err)
	}
	require.NoError(t, peer.MaterializeFederatedProject(ctx, local.ID))
	reaped, err := hub.ReapCronRun(ctx, db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: initial.Run.Revision, Force: true, Now: initial.Run.UpdatedAt})
	require.NoError(t, err)
	_, err = peer.InsertRemoteEvent(ctx, local.ID, portableNativeEvent(reaped.Events[0]))
	require.NoError(t, err)
	require.NoError(t, peer.MaterializeFederatedProject(ctx, local.ID))
	current, err := peer.CronRun(ctx, local.ID, id)
	require.NoError(t, err)
	want, got := db.NewCronRunObservation(reaped.Run, p.UID), db.NewCronRunObservation(current, p.UID)
	want.Revision, got.Revision = 0, 0 // Revisions are local CAS tokens, not portable evidence equality.
	require.Equal(t, want, got)
	_, err = peer.EnableFederationPush(ctx, local.ID, adopted.Binding.PushCursorEventID)
	require.NoError(t, err)
	resumed, err := peer.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: id, Actor: "starter", JobUID: current.JobUID, DefinitionEventUID: current.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1, Message: "New evidence"}, ExpectedRevision: current.Revision})
	require.NoError(t, err)
	peerReap, err := peer.ReapCronRun(ctx, db.ReapCronRun{ProjectID: local.ID, UID: id, Actor: "operator", ExpectedRevision: resumed.Run.Revision, Force: true, Now: resumed.Run.UpdatedAt})
	require.NoError(t, err)
	batch := []db.FederationIngestEvent{}
	for _, event := range []db.Event{resumed.Events[0], peerReap.Events[0]} {
		batch = append(batch, db.FederationIngestEvent{SourceEventID: event.ID, Event: portableNativeEvent(event)})
	}
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: peer.InstanceUID(), BoundActor: "operator", EventFeatures: db.CronEventFeature, Events: batch})
	require.NoError(t, err)
	current, err = hub.CronRun(ctx, p.ID, id)
	require.NoError(t, err)
	require.Equal(t, "unknown", current.Status)
	require.Equal(t, "starter", current.Actor)
	// A later remote completion can have the same source revision. The fold
	// still advances local CAS, preventing an operator's old read overwriting it.
	late := current
	late.Status = "succeeded"
	late.Summary.Message = "Late truth"
	late.UpdatedAt = current.UpdatedAt.Add(time.Minute)
	payload, err := json.Marshal(db.NewCronRunObservation(late, p.UID))
	require.NoError(t, err)
	event := portableNativeEvent(peerReap.Events[0])
	event.EventUID, err = uid.New()
	require.NoError(t, err)
	event.Payload = payload
	event.HLCPhysicalMS += 60000
	event.CreatedAt = late.UpdatedAt
	resignNativeEvent(t, &event)
	_, err = hub.InsertRemoteEvent(ctx, p.ID, event)
	require.NoError(t, err)
	require.NoError(t, hub.MaterializeFederatedProject(ctx, p.ID))
	_, err = hub.ReapCronRun(ctx, db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: current.Revision, Force: true, Now: late.UpdatedAt})
	require.ErrorIs(t, err, db.ErrCronConflict)
	final, err := hub.CronRun(ctx, p.ID, id)
	require.NoError(t, err)
	require.Equal(t, "succeeded", final.Status)
	require.Greater(t, final.Revision, current.Revision)
	return nil
}

func checkCronReapClockPrecision(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, p)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		id, err := uid.New()
		require.NoError(t, err)
		observed, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Status: "running", Summary: cron.Summary{Version: 1}})
		require.NoError(t, err)
		// Federation writes RFC3339Nano updated_at values without truncating them.
		updated := observed.Run.UpdatedAt.Add(456789 * time.Nanosecond)
		_, err = q.ExecContext(ctx, `UPDATE cron_runs SET updated_at=$1 WHERE uid=$2`, updated.Format(time.RFC3339Nano), id)
		require.NoError(t, err)
		now := updated.Add(24 * time.Hour).Add(offset)
		result, err := store.ReapCronRun(ctx, db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: observed.Run.Revision, Now: now})
		if offset < 0 {
			require.ErrorIs(t, err, db.ErrCronRunNotStale)
		} else {
			require.NoError(t, err)
			require.Equal(t, now, result.Run.UpdatedAt)
		}
	}
	return nil
}

func checkCronReapRollback(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, p)
	id, err := uid.New()
	require.NoError(t, err)
	initial, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	install := `CREATE TRIGGER fail_reap_update BEFORE UPDATE ON cron_runs BEGIN SELECT RAISE(FAIL,'injected reap update failure'); END`
	drop := `DROP TRIGGER fail_reap_update`
	if backend.Name == "postgres" {
		install = `CREATE FUNCTION fail_reap_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected reap update failure'; END $$; CREATE TRIGGER fail_reap_update BEFORE UPDATE ON cron_runs FOR EACH ROW EXECUTE FUNCTION fail_reap_update()`
		drop = `DROP TRIGGER fail_reap_update ON cron_runs; DROP FUNCTION fail_reap_update()`
	}
	_, err = q.ExecContext(ctx, install)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := q.ExecContext(context.WithoutCancel(t.Context()), drop); require.NoError(t, err) })
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	result, err := store.ReapCronRun(ctx, db.ReapCronRun{ProjectID: p.ID, UID: id, Actor: "operator", ExpectedRevision: initial.Run.Revision, Force: true, Now: initial.Run.UpdatedAt})
	require.Error(t, err)
	require.Empty(t, result.Events)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	current, err := store.CronRun(ctx, p.ID, id)
	require.NoError(t, err)
	require.Equal(t, initial.Run, current)
	return nil
}
