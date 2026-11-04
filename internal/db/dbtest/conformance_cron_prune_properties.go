package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
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
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func pruneEventFixture(t *testing.T, s db.Storage) (db.Project, pruneFixtureSQL) {
	t.Helper()
	p, err := s.CreateProject(t.Context(), "event-model-project")
	require.NoError(t, err)
	return p, s.(pruneFixtureSQL)
}

func seedPruneEventRows(ctx context.Context, t require.TestingT, q pruneFixtureSQL, p db.Project, count int) {
	_, err := q.ExecContext(ctx, `DELETE FROM cron_runs WHERE project_id=$1`, p.ID)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND type LIKE 'cron.%'`, p.ID)
	require.NoError(t, err)
	for i := range 2 {
		_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status,created_at,updated_at) VALUES($1,$2,$3,$4,'operator','succeeded',$5,$5)`, fmt.Sprintf("%026d", i+100), p.ID, fmt.Sprintf("%026d", 1), fmt.Sprintf("%026d", 2), time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC).Format(db.CronRunCreatedAtFormat))
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
		_, err := tx.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,'operator',$6,1,0,$7,$8)`, fmt.Sprintf("%026d", i+1000), fmt.Sprintf("%026d", 3), p.ID, p.Name, kind, fmt.Sprintf(`{"uid":"%026d","other":"%026d"}`, id, 100), strings.Repeat("0", 64), "2026-01-01T00:00:00.000Z")
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
		require.Equal(ht, result.MetadataEvent.ID, metadataEvent.ID)
		require.Equal(ht, result.MetadataEvent.OriginInstanceUID, metadataEvent.OriginInstanceUID)
		require.Equal(ht, result.MetadataEvent.ProjectID, metadataEvent.ProjectID)
		require.Equal(ht, result.MetadataEvent.Type, metadataEvent.Type)
		require.Equal(ht, result.MetadataEvent.ProjectUID, metadataEvent.ProjectUID)
		require.Equal(ht, result.MetadataEvent.ProjectName, metadataEvent.ProjectName)
		require.Equal(ht, result.MetadataEvent.IssueID, metadataEvent.IssueID)
		require.Equal(ht, result.MetadataEvent.IssueUID, metadataEvent.IssueUID)
		require.Equal(ht, result.MetadataEvent.RelatedIssueID, metadataEvent.RelatedIssueID)
		require.Equal(ht, result.MetadataEvent.RelatedIssueUID, metadataEvent.RelatedIssueUID)
		require.Equal(ht, result.MetadataEvent.Actor, metadataEvent.Actor)
		require.JSONEq(ht, result.MetadataEvent.Payload, string(metadataEvent.Payload))
		require.Equal(ht, result.MetadataEvent.HLCPhysicalMS, metadataEvent.HLCPhysicalMS)
		require.Equal(ht, result.MetadataEvent.HLCCounter, metadataEvent.HLCCounter)
		require.Equal(ht, result.MetadataEvent.ContentHash, metadataEvent.ContentHash)
		require.Equal(ht, result.MetadataEvent.CreatedAt.UTC().Format(db.EventTimestampFormat), metadataEvent.CreatedAt)
		delete(got, result.MetadataEvent.UID)
		require.Equal(ht, want, got)
	}, hegel.WithTestCases(30))
	return nil
}

// Contract A9/A10: a failure after event and run deletion rolls back the entire
// batch. The reset hook injects cancellation or a failure at the last stage.
func checkCronPruneRollback(t *testing.T, s db.Storage) error {
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
			adapter := db.CronSQL{
				PruneGate: func(context.Context, *sql.Tx, db.Project) error { return nil },
				WriteGate: func(context.Context, *sql.Tx, int64) error { return nil },
				ReserveResetCursor: func(context.Context, *sql.Tx) (int64, error) {
					if cancelAtReset {
						cancel()
						return 0, context.Canceled
					}
					return 0, injectedResetFailure
				},
				PatchProjectMetadataTx: func(ctx context.Context, tx *sql.Tx, in db.PatchProjectMetadataIn) (db.PatchProjectMetadataOut, error) {
					metadata, err := json.Marshal(in.Patch)
					if err != nil {
						return db.PatchProjectMetadataOut{}, err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE projects SET metadata=$1,revision=revision+1 WHERE id=$2`, string(metadata), in.ProjectID); err != nil {
						return db.PatchProjectMetadataOut{}, err
					}
					eventUID, err := uid.New()
					if err != nil {
						return db.PatchProjectMetadataOut{}, err
					}
					_, err = tx.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,'project.metadata_updated',$5,'{"diff":{}}',1,0,$6,$7)`, eventUID, s.InstanceUID(), p.ID, p.Name, in.Actor, strings.Repeat("0", 64), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(db.CronRunCreatedAtFormat))
					if err != nil {
						return db.PatchProjectMetadataOut{}, err
					}
					return db.PatchProjectMetadataOut{}, nil
				},
				Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
					tx, err := q.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if err := run(tx); err != nil {
						return err
					}
					return tx.Commit()
				},
			}
			_, err := adapter.PruneRuns(ctx, db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 1})
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
			return s.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: spoke, BoundActor: "operator", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
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
