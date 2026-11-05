package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

type pruneFixtureSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func pruneEventFixture(t *testing.T, s db.Storage) (db.Project, pruneFixtureSQL) {
	t.Helper()
	p, err := s.CreateProject(t.Context(), "event-model-project")
	require.NoError(t, err)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: fmt.Sprintf("%026d", 100), Name: "Review", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	return p, s.(pruneFixtureSQL)
}

func seedPruneEventRows(ctx context.Context, t require.TestingT, q pruneFixtureSQL, p db.Project, count int) {
	job, err := q.(db.Storage).CronJob(ctx, p.ID, fmt.Sprintf("%026d", 100))
	require.NoError(t, err)
	definition, err := json.Marshal(job.Definition)
	require.NoError(t, err)
	jobSnapshot, err := json.Marshal(db.CronDefinitionEvent{UID: job.UID, ProjectUID: p.UID, Name: job.Name, Definition: definition, Author: job.Author, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt, DefinitionEventUID: job.DefinitionEventUID, DefinitionHLC: &job.DefinitionHLC})
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `DELETE FROM cron_runs WHERE project_id=$1`, p.ID)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND type IN ('cron.run.observed','cron.run.snapshot','cron.job.snapshot')`, p.ID)
	require.NoError(t, err)
	for i := range 2 {
		_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status,created_at,updated_at) VALUES($1,$2,$3,$4,'operator','succeeded',$5,$6)`, fmt.Sprintf("%026d", i+100), p.ID, job.UID, job.DefinitionEventUID, time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC).Format(db.CronRunCreatedAtFormat), time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
		require.NoError(t, err)
	}
	tx, err := q.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := range count {
		kind := []string{"cron.run.observed", "cron.run.snapshot", "cron.job.snapshot", "cron.run.observed"}[i%4]
		id := 100
		if i%4 == 3 {
			id = 101
		}
		at := time.Date(2026, 1, id-99, 0, 0, 0, 0, time.UTC)
		observation := db.CronRunObservation{ProjectUID: p.UID, UID: fmt.Sprintf("%026d", id), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: "succeeded", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
		if kind == "cron.run.snapshot" {
			observation.ObservationEventUID = observation.UID
			observation.ObservationHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: at.UnixMilli(), OriginInstanceUID: q.(db.Storage).InstanceUID()}
		}
		payload, err := json.Marshal(observation)
		require.NoError(t, err)
		if kind == "cron.job.snapshot" {
			// Job and run UIDs occupy separate tables and may coincide.
			payload = jobSnapshot
		}
		event := db.RemoteEvent{EventUID: fmt.Sprintf("%026d", i+1000), OriginInstanceUID: q.(db.Storage).InstanceUID(), ProjectUID: p.UID, ProjectName: p.Name, Type: kind, Actor: "operator", Payload: payload, HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,'operator',$6,$7,0,$8,$9)`, event.EventUID, event.OriginInstanceUID, p.ID, p.Name, kind, string(payload), event.HLCPhysicalMS, event.ContentHash, at.Format(db.EventTimestampFormat))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func pruneEvents(ctx context.Context, t require.TestingT, s db.Storage, p db.Project) map[string]db.EventExport {
	out := map[string]db.EventExport{}
	for e, err := range s.ExportEvents(ctx, db.ExportFilter{ProjectID: &p.ID}) {
		require.NoError(t, err)
		out[e.UID] = e
	}
	return out
}

// Contract D6/D7: only selected runs' top-level observed/snapshot event bodies
// disappear, including across event pages; every surviving row is identical.
func checkCronPruneEventModel(t *testing.T, s db.Storage) error {
	p, q := pruneEventFixture(t, s)
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, math.MaxInt32))
		count = min(count, 1100) // Bound fixture materialization, not the drawn domain.
		if hegel.Draw(ht, hegel.Booleans()) {
			count = []int{0, 1, 499, 500, 501, 999, 1000, 1001}[hegel.Draw(ht, hegel.Integers(0, 7))]
		}
		seedPruneEventRows(t.Context(), ht, q, p, count)
		want := pruneEvents(t.Context(), ht, s, p)
		eventCount := len(want)
		for id, e := range want {
			var body struct {
				UID string `json:"uid"`
			}
			require.NoError(ht, json.Unmarshal(e.Payload, &body))
			if (e.Type == "cron.run.observed" || e.Type == "cron.run.snapshot") && body.UID == fmt.Sprintf("%026d", 100) {
				delete(want, id)
			}
		}
		in := db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1, DryRun: true}
		preview, err := s.PruneCronRuns(t.Context(), in)
		require.NoError(ht, err)
		require.Equal(ht, eventCount-len(want), preview.EventsDeleted)
		in.DryRun = false
		result, err := s.PruneCronRuns(t.Context(), in)
		require.Equal(ht, eventCount-len(want), result.EventsDeleted)
		require.NoError(ht, err)
		got := pruneEvents(t.Context(), ht, s, p)
		metadataEvent, ok := got[result.MetadataEvent.UID]
		require.True(ht, ok, "prune metadata event is exported")
		e := result.MetadataEvent
		require.JSONEq(ht, e.Payload, string(metadataEvent.Payload))
		require.Equal(ht, db.EventExport{
			ID: e.ID, UID: e.UID, OriginInstanceUID: e.OriginInstanceUID,
			ProjectID: e.ProjectID, ProjectUID: e.ProjectUID, ProjectName: e.ProjectName,
			IssueID: e.IssueID, IssueUID: e.IssueUID, RelatedIssueID: e.RelatedIssueID, RelatedIssueUID: e.RelatedIssueUID,
			Type: e.Type, Actor: e.Actor, Payload: metadataEvent.Payload,
			HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, ContentHash: e.ContentHash,
			CreatedAt: e.CreatedAt.UTC().Format(db.EventTimestampFormat),
		}, metadataEvent)
		delete(got, result.MetadataEvent.UID)
		require.Equal(ht, want, got)
	}, hegel.WithTestCases(30))
	return nil
}

// Match only exact top-level run UIDs, independently of JSON spelling and
// another project's matching event bodies.
func checkCronPruneEventIdentity(t *testing.T, s db.Storage) error {
	p, q := pruneEventFixture(t, s)
	seedPruneEventRows(t.Context(), t, q, p, 13)
	selected := fmt.Sprintf("%026d", 100)
	for i, payload := range map[int]string{
		1:  `{"uid":null}`,
		4:  ` { "uid" : "\u0030` + selected[1:] + `" } `,
		5:  `{}`,
		8:  `{"nested":{"uid":"` + selected + `"}}`,
		12: `{"uid":"prefix-` + selected + `-suffix"}`,
	} {
		_, err := q.ExecContext(t.Context(), `UPDATE events SET payload=$1 WHERE project_id=$2 AND uid=$3`, payload, p.ID, fmt.Sprintf("%026d", 1000+i))
		require.NoError(t, err)
	}
	other := newCronTallyFixture(t.Context(), t, s)
	other.add(t.Context(), t, "job", "succeeded", time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), "")
	other.add(t.Context(), t, "job", "running", other.before, "")
	_, err := q.ExecContext(t.Context(), `UPDATE events SET payload=$1 WHERE project_id=$2 AND type='cron.run.observed'`, `{"uid":"`+selected+`"}`, other.project.ID)
	require.NoError(t, err)
	otherEvents := pruneEvents(t.Context(), t, s, other.project)
	otherRuns, err := s.ListCronRuns(t.Context(), db.CronRunList{ProjectID: other.project.ID})
	require.NoError(t, err)
	original := pruneEvents(t.Context(), t, s, p)
	want := maps.Clone(original)
	otherProject, err := s.ProjectByID(t.Context(), other.project.ID)
	require.NoError(t, err)
	// Only these bodies match. Null/missing UIDs immediately follow matches
	// so a reused decoder target must not carry a UID into the next row.
	for _, i := range []int{0, 4, 9} {
		delete(want, fmt.Sprintf("%026d", 1000+i))
	}
	for _, dry := range []bool{true, false} {
		result, err := s.PruneCronRuns(t.Context(), db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: other.before, Keep: 1, DryRun: dry})
		require.NoError(t, err)
		require.Equal(t, 1, result.Pruned)
		require.Equal(t, 3, result.EventsDeleted)
		require.False(t, result.More)
		got := pruneEvents(t.Context(), t, s, p)
		if dry {
			require.Equal(t, original, got)
		} else {
			delete(got, result.MetadataEvent.UID)
			require.Equal(t, want, got)
		}
		require.Equal(t, otherEvents, pruneEvents(t.Context(), t, s, other.project))
		gotRuns, err := s.ListCronRuns(t.Context(), db.CronRunList{ProjectID: other.project.ID})
		require.NoError(t, err)
		require.Equal(t, otherRuns, gotRuns)
		gotProject, err := s.ProjectByID(t.Context(), other.project.ID)
		require.NoError(t, err)
		require.Equal(t, otherProject, gotProject)
	}
	return nil
}

// The existing strict decoder rejects corruption even on an unselected run.
// Preview and deletion must return that error and preserve the whole batch.
func checkCronPruneMalformedEvents(t *testing.T, s db.Storage) error {
	p, q := pruneEventFixture(t, s)
	seedPruneEventRows(t.Context(), t, q, p, 1001)
	// ExportEvents intentionally refuses duplicate JSON members. Compare raw
	// persisted bodies so that export validation cannot mask the prune result.
	snapshot := func(t *testing.T) map[int64]string {
		t.Helper()
		rows, err := q.QueryContext(t.Context(), `SELECT id,payload FROM events WHERE project_id=$1`, p.ID)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		out := map[int64]string{}
		for rows.Next() {
			var id int64
			var body string
			require.NoError(t, rows.Scan(&id, &body))
			out[id] = body
		}
		require.NoError(t, rows.Err())
		return out
	}
	for _, payload := range []string{
		`{"uid":123}`,
		`{"uid":"unselected","uid":"unselected"}`,
		`{"uid":"unselected","extra":{"x":1,"x":2}}`,
		`{"uid":"unselected","\u0075id":"unselected"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			_, err := q.ExecContext(t.Context(), `UPDATE events SET payload=$1 WHERE project_id=$2 AND uid=$3`, payload, p.ID, fmt.Sprintf("%026d", 1999))
			require.NoError(t, err)
			wantEvents := snapshot(t)
			wantRuns, err := s.ListCronRuns(t.Context(), db.CronRunList{ProjectID: p.ID})
			require.NoError(t, err)
			wantProject, err := s.ProjectByID(t.Context(), p.ID)
			require.NoError(t, err)
			var body struct {
				UID string `json:"uid"`
			}
			decodeErr := json.Unmarshal([]byte(payload), &body)
			require.Error(t, decodeErr)
			for _, dry := range []bool{true, false} {
				_, err := s.PruneCronRuns(t.Context(), db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1, DryRun: dry})
				require.EqualError(t, err, decodeErr.Error())
				require.Equal(t, wantEvents, snapshot(t))
				gotRuns, err := s.ListCronRuns(t.Context(), db.CronRunList{ProjectID: p.ID})
				require.NoError(t, err)
				require.Equal(t, wantRuns, gotRuns)
				gotProject, err := s.ProjectByID(t.Context(), p.ID)
				require.NoError(t, err)
				require.Equal(t, wantProject, gotProject)
			}
		})
	}
	return nil
}

// Contract A9/A10: a failure after event and run deletion rolls back the entire
// batch. The reset hook injects cancellation or a failure at the last stage.
func checkCronPruneRollback(t *testing.T, s db.Storage, backend Backend) error {
	p, q := pruneEventFixture(t, s)
	seedPruneEventRows(t.Context(), t, q, p, 1001)
	wantEvents := pruneEvents(t.Context(), t, s, p)
	wantRuns, err := s.ListCronRuns(t.Context(), db.CronRunList{ProjectID: p.ID})
	require.NoError(t, err)
	var wantMetadata string
	var wantRevision int64
	require.NoError(t, q.QueryRowContext(t.Context(), `SELECT metadata,revision FROM projects WHERE id=$1`, p.ID).Scan(&wantMetadata, &wantRevision))
	injectedResetFailure := errors.New("injected reset failure")
	for _, cancelAtReset := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelAtReset), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			adapter := backend.CronSQLForTest(s)
			reachedReset := false
			adapter.ReserveResetCursor = func(context.Context, *sql.Tx) (int64, error) {
				reachedReset = true
				if cancelAtReset {
					cancel()
					return 0, ctx.Err()
				}
				return 0, injectedResetFailure
			}
			_, err := adapter.PruneRuns(ctx, db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1})
			require.True(t, reachedReset, "failure occurs after real metadata and deletion writes")
			if cancelAtReset {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, injectedResetFailure)
			}
			gotRuns, err := s.ListCronRuns(t.Context(), db.CronRunList{ProjectID: p.ID})
			require.NoError(t, err)
			require.Equal(t, wantRuns, gotRuns)
			require.Equal(t, wantEvents, pruneEvents(t.Context(), t, s, p))
			var gotMetadata string
			var gotRevision int64
			require.NoError(t, q.QueryRowContext(t.Context(), `SELECT metadata,revision FROM projects WHERE id=$1`, p.ID).Scan(&gotMetadata, &gotRevision))
			require.Equal(t, wantMetadata, gotMetadata)
			require.Equal(t, wantRevision, gotRevision)
		})
	}
	return nil
}

// Contract R2/R3: deleting deduplication evidence permits one previously
// accepted direct-spoke observation to return; its next identical retry is a
// duplicate. This slice deliberately has no tombstone to prevent resurrection.
func checkCronPruneAcceptedReplay(t *testing.T, s db.Storage) error {
	p, err := s.CreateProject(t.Context(), "replay-project")
	require.NoError(t, err)
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Actor: "operator", Name: "Inspect", Definition: def})
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(t.Context(), p.ID, "operator")
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	newestUID, err := uid.New()
	require.NoError(t, err)
	_, err = s.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: newestUID, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "succeeded", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	// Distinct source events advance the spoke cursor; retries retain their ID.
	sourceIDs := map[string]int64{}
	var nextSourceID int64
	hegel.Test(t, func(ht *hegel.T) {
		status := []string{"running", "succeeded", "failed", "cancelled", "unknown"}[hegel.Draw(ht, hegel.Integers(0, 4))]
		id, err := uid.New()
		require.NoError(ht, err)
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		observation := db.CronRunObservation{ProjectUID: p.UID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: status, Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
		body, err := json.Marshal(observation)
		require.NoError(ht, err)
		original := newRemoteEvent(t, p, nil, "cron.run.observed", "operator", spoke, at.UnixMilli(), jsontext.Value(body))
		ingest := func(event db.RemoteEvent) (db.FederationIngestResult, error) {
			sourceID, ok := sourceIDs[event.EventUID]
			if !ok {
				nextSourceID++
				sourceID = nextSourceID
				sourceIDs[event.EventUID] = sourceID
			}
			return s.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: spoke, BoundActor: "operator", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: sourceID, Event: event}}})
		}
		_, err = ingest(original)
		require.NoError(ht, err)
		if status == "running" {
			observation.Status, observation.Revision, observation.UpdatedAt = "succeeded", 2, at.Add(time.Hour)
			body, err = json.Marshal(observation)
			require.NoError(ht, err)
			_, err = ingest(newRemoteEvent(t, p, nil, "cron.run.observed", "operator", spoke, at.Add(time.Hour).UnixMilli(), body))
			require.NoError(ht, err)
		}
		_, err = s.PruneCronRuns(t.Context(), db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: at.Add(24 * time.Hour), Keep: 1})
		require.NoError(ht, err)
		_, err = s.CronRun(t.Context(), p.ID, id)
		require.ErrorIs(ht, err, db.ErrNotFound)
		_, err = ingest(original)
		require.NoError(ht, err)
		got, err := s.CronRun(t.Context(), p.ID, id)
		require.NoError(ht, err)
		require.Equal(ht, status, got.Status)
		retry, err := ingest(original)
		require.NoError(ht, err)
		require.Equal(ht, 1, retry.Duplicates)
	}, hegel.WithTestCases(20))
	return nil
}
