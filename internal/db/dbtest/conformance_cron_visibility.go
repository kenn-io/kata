package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

var cronVisibilityStatuses = []string{"running", "succeeded", "failed", "cancelled", "unknown"}

func cronVisibilityUID(n int) string { return fmt.Sprintf("01J%023d", n) }

// cronVisibilityDefinition is a committed job or workflow that runs reference.
type cronVisibilityDefinition struct {
	projectID     int64
	uid, eventUID string
	workflow      bool
}

func cronVisibilityJob(t *testing.T, store db.Storage, projectID int64) cronVisibilityDefinition {
	t.Helper()
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: projectID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	return cronVisibilityDefinition{projectID: projectID, uid: job.UID, eventUID: job.DefinitionEventUID}
}

func cronVisibilityWorkflow(t *testing.T, store db.Storage, projectID int64) cronVisibilityDefinition {
	t.Helper()
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "command", Command: "git status"}}}
	workflow, _, err := store.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: projectID, Name: "Review workflow", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	return cronVisibilityDefinition{projectID: projectID, uid: workflow.UID, eventUID: workflow.DefinitionEventUID, workflow: true}
}

// Seed evidence directly to control creation instants without changing the
// production clock. Each run references a committed definition in its own
// project, as producers require.
func seedCronVisibilityRun(t *testing.T, store db.Storage, runID int, definition cronVisibilityDefinition, status string, at time.Time) db.CronRun {
	t.Helper()
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	var jobUID, definitionUID, workflowUID, workflowDefinitionUID any
	if definition.workflow {
		workflowUID, workflowDefinitionUID = definition.uid, definition.eventUID
	} else {
		jobUID, definitionUID = definition.uid, definition.eventUID
	}
	id := cronVisibilityUID(runID)
	_, err := q.ExecContext(t.Context(), `INSERT INTO cron_runs
		(uid,project_id,job_uid,definition_event_uid,workflow_uid,workflow_definition_event_uid,actor,status,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,'worker',$7,$8,$8)`, id, definition.projectID, jobUID, definitionUID, workflowUID, workflowDefinitionUID, status, at.UTC().Format(db.CronRunCreatedAtFormat))
	require.NoError(t, err)
	run, err := store.CronRun(t.Context(), definition.projectID, id)
	require.NoError(t, err)
	return run
}

func checkCronVisibilityLatest(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	oldDef, busyDef, neverDef := cronVisibilityJob(t, store, project.ID), cronVisibilityJob(t, store, project.ID), cronVisibilityJob(t, store, project.ID)
	otherDef, otherNewerDef := cronVisibilityJob(t, store, other.ID), cronVisibilityJob(t, store, other.ID)
	oldJob, busyJob, neverJob, otherJob := oldDef.uid, busyDef.uid, neverDef.uid, otherDef.uid
	old := seedCronVisibilityRun(t, store, 1, oldDef, "failed", at.Add(-72*time.Hour))
	seedCronVisibilityRun(t, store, 2, oldDef, "failed", at.Add(-48*time.Hour))
	winner := seedCronVisibilityRun(t, store, 3, oldDef, "succeeded", at.Add(-48*time.Hour))
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `UPDATE cron_runs SET updated_at=$1 WHERE uid=$2`, at.Add(365*24*time.Hour).Format(time.RFC3339Nano), old.UID)
	require.NoError(t, err)
	var busy db.CronRun
	for i := range 130 {
		busy = seedCronVisibilityRun(t, store, 10+i, busyDef, "running", at.Add(time.Duration(i)*time.Second))
	}
	seedCronVisibilityRun(t, store, 200, otherNewerDef, "failed", at.Add(24*time.Hour))
	foreign := seedCronVisibilityRun(t, store, 201, otherDef, "failed", at)
	seedCronVisibilityRun(t, store, 202, cronVisibilityWorkflow(t, store, project.ID), "failed", at)

	latest, err := store.LatestCronRuns(ctx, project.ID, []string{busyJob, strings.ToLower(oldJob), neverJob, otherJob, oldJob})
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.Equal(t, winner.UID, latest[oldJob].UID, "creation order and UID, not update time, select the last run")
	require.Equal(t, busy.UID, latest[busyJob].UID)
	_, exists := latest[neverJob]
	require.False(t, exists, "a missing key means this job never ran")
	latest, err = store.LatestCronRuns(ctx, other.ID, []string{otherJob})
	require.NoError(t, err)
	require.Equal(t, foreign.UID, latest[otherJob].UID)
	latest, err = store.LatestCronRuns(ctx, project.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Empty(t, latest)
	_, err = store.LatestCronRuns(ctx, project.ID, []string{oldJob, "not-a-job-uid"})
	require.ErrorIs(t, err, cron.ErrInvalid)
	return nil
}

func checkCronVisibilityStatusPages(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	job := cronVisibilityJob(t, store, project.ID)
	failed := seedCronVisibilityRun(t, store, 1, job, "failed", at)
	cancelled := seedCronVisibilityRun(t, store, 2, job, "cancelled", at)
	newer := seedCronVisibilityRun(t, store, 3, job, "failed", at)
	seedCronVisibilityRun(t, store, 4, job, "succeeded", at.Add(time.Second))
	seedCronVisibilityRun(t, store, 5, cronVisibilityJob(t, store, project.ID), "failed", at.Add(time.Second))
	foreign := seedCronVisibilityRun(t, store, 6, cronVisibilityJob(t, store, other.ID), "failed", at)

	query := db.CronRunList{ProjectID: project.ID, JobUID: job.uid, Limit: 2}
	query.Statuses = []string{"failed", "cancelled", "failed"}
	page, err := store.ListCronRuns(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []string{newer.UID, cancelled.UID}, cronVisibilityRunUIDs(page), "filter before limiting; tied instants page by UID")
	query.BeforeUID = page[len(page)-1].UID
	page, err = store.ListCronRuns(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []string{failed.UID}, cronVisibilityRunUIDs(page))
	query.BeforeUID = cronVisibilityUID(4)
	query.Statuses = []string{"failed"}
	page, err = store.ListCronRuns(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []string{newer.UID, failed.UID}, cronVisibilityRunUIDs(page), "the cursor need not itself match the selected statuses")
	query.BeforeUID = foreign.UID
	_, err = store.ListCronRuns(ctx, query)
	require.ErrorIs(t, err, db.ErrNotFound)
	query.BeforeUID = ""
	for _, invalid := range []string{"", "FAILED", "never_ran", "failed') OR 1=1 --"} {
		query.Statuses = []string{"failed", invalid}
		_, err = store.ListCronRuns(ctx, query)
		require.ErrorIs(t, err, cron.ErrInvalid, invalid)
	}
	return nil
}

func checkCronVisibilityFailedCount(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	until := time.Date(2026, 10, 25, 12, 0, 0, 123456789, time.UTC)
	since := until.Add(-24 * time.Hour)
	job := cronVisibilityJob(t, store, project.ID)
	for i, at := range []time.Time{since.Add(-time.Nanosecond), since, since.Add(time.Nanosecond), until, until.Add(time.Nanosecond)} {
		seedCronVisibilityRun(t, store, i+1, job, "failed", at)
	}
	for i, status := range []string{"running", "succeeded", "cancelled", "unknown"} {
		seedCronVisibilityRun(t, store, i+10, job, status, until)
	}
	seedCronVisibilityRun(t, store, 20, cronVisibilityWorkflow(t, store, project.ID), "failed", until)
	seedCronVisibilityRun(t, store, 21, cronVisibilityJob(t, store, other.ID), "failed", until)
	count, err := store.CountFailedCronRuns(ctx, project.ID, since.In(time.FixedZone("offset", -7*60*60)), until)
	require.NoError(t, err)
	require.EqualValues(t, 4, count, "include both boundaries, including workflow-only observations")
	count, err = store.CountFailedCronRuns(ctx, project.ID, until, until)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	count, err = store.CountFailedCronRuns(ctx, other.ID, since, until)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	count, err = store.CountFailedCronRuns(ctx, project.ID, until.Add(time.Hour), until.Add(2*time.Hour))
	require.NoError(t, err)
	require.Zero(t, count)
	count, err = store.CountFailedCronRuns(ctx, project.ID,
		time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC))
	require.NoError(t, err)
	require.EqualValues(t, 6, count, "the full supported timestamp range remains readable")
	_, err = store.CountFailedCronRuns(ctx, project.ID, until, since)
	require.ErrorIs(t, err, cron.ErrInvalid)
	for _, window := range [][2]time.Time{
		{time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), until},
		{since, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		_, err = store.CountFailedCronRuns(ctx, project.ID, window[0], window[1])
		require.ErrorIs(t, err, cron.ErrInvalid, "out-of-range timestamps cannot use the fixed-width text ordering")
	}
	return nil
}

func checkCronVisibilityCancellation(t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.LatestCronRuns(ctx, project.ID, []string{cronVisibilityUID(1000)})
	require.ErrorIs(t, err, context.Canceled)
	query := db.CronRunList{ProjectID: project.ID}
	query.Statuses = []string{"failed"}
	_, err = store.ListCronRuns(ctx, query)
	require.ErrorIs(t, err, context.Canceled)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	_, err = store.CountFailedCronRuns(ctx, project.ID, now.Add(-24*time.Hour), now)
	require.ErrorIs(t, err, context.Canceled)
	return nil
}

type cronVisibilityQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type cronVisibilityQueries struct {
	cronVisibilityQuery
	arguments []int
}

func (q *cronVisibilityQueries) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.arguments = append(q.arguments, len(args))
	return q.cronVisibilityQuery.QueryContext(ctx, statement, args...)
}

func checkCronVisibilityBatching(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	definitions := make([]cronVisibilityDefinition, 513)
	jobs := make([]string, len(definitions))
	for i := range definitions {
		definitions[i] = cronVisibilityJob(t, store, project.ID)
		jobs[i] = definitions[i].uid
	}
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	want := map[string]string{}
	for i, index := range []int{0, 256, 512} {
		run := seedCronVisibilityRun(t, store, i+1, definitions[index], "succeeded", at)
		want[jobs[index]] = run.UID
	}
	requested := append(slices.Clone(jobs), jobs[0], strings.ToLower(jobs[256]))
	before := slices.Clone(requested)
	observed := &cronVisibilityQueries{cronVisibilityQuery: store.(cronVisibilityQuery)}
	adapter := db.CronSQL{Query: observed}
	got, err := adapter.LatestRuns(ctx, project.ID, requested)
	require.NoError(t, err)
	require.Equal(t, want, cronVisibilityLatestUIDs(got))
	require.Equal(t, []int{257, 257, 2}, observed.arguments, "one project parameter plus at most 256 distinct job IDs per query")
	require.Equal(t, before, requested, "normalizing and batching must not mutate caller input")
	observed.arguments = nil
	_, err = adapter.LatestRuns(ctx, project.ID, nil)
	require.NoError(t, err)
	require.Empty(t, observed.arguments, "empty lookup must not issue a query")
	_, err = adapter.LatestRuns(ctx, project.ID, append(jobs, "invalid"))
	require.ErrorIs(t, err, cron.ErrInvalid)
	require.Empty(t, observed.arguments, "validate all IDs before querying the first batch")
	return nil
}

func checkCronVisibilityProperties(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	projects := []int64{project.ID, other.ID}
	// Jobs 0-2 belong to the first project and 3-5 to the second.
	definitions := make([]cronVisibilityDefinition, 0, 2*3)
	jobs := make([]string, 0, 2*3)
	for _, projectID := range projects {
		for range 3 {
			definition := cronVisibilityJob(t, store, projectID)
			definitions = append(definitions, definition)
			jobs = append(jobs, definition.uid)
		}
	}
	base := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var runs []db.CronRun
	for i := range 30 {
		// Nonmonotonic/tied creation times and interleaved projects make a
		// first-row, globally keyed, or UID-only oracle observably wrong.
		runs = append(runs, seedCronVisibilityRun(t, store, i+1, definitions[(i%2)*3+i%3], cronVisibilityStatuses[i%5], base.Add(time.Duration((i*7)%9-4)*time.Hour)))
	}
	t.Run("latest lookup matches independent history", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			projectID := hegel.Draw(ht, hegel.SampledFrom(projects))
			requested := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(jobs)))
			candidate := ulid.ULID(hegel.Draw(ht, hegel.Binary(16, 16))).String()
			if hegel.Draw(ht, hegel.Booleans()) {
				requested = append(requested, candidate)
			}
			if hegel.Draw(ht, hegel.Booleans()) {
				for i := range requested {
					requested[i] = strings.ToLower(requested[i])
				}
			}
			want := map[string]string{}
			for _, job := range requested {
				job = strings.ToUpper(job)
				candidates := []db.CronRun{}
				for _, run := range runs {
					if run.ProjectID == projectID && *run.JobUID == job {
						candidates = append(candidates, run)
					}
				}
				slices.SortFunc(candidates, cronVisibilityNewestFirst)
				if len(candidates) > 0 {
					want[job] = candidates[0].UID
				}
			}
			got, err := store.LatestCronRuns(ctx, projectID, requested)
			require.NoError(ht, err)
			require.Equal(ht, want, cronVisibilityLatestUIDs(got))
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})
	t.Run("filtered page matches independent history", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			projectID := hegel.Draw(ht, hegel.SampledFrom(projects))
			statuses := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(cronVisibilityStatuses)))
			limit := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
			job := hegel.Draw(ht, hegel.SampledFrom(append(slices.Clone(jobs), "")))
			var cursor *db.CronRun
			if hegel.Draw(ht, hegel.Booleans()) {
				index := hegel.Draw(ht, hegel.Integers(0, len(runs)/2-1))
				candidate := runs[index*2]
				if candidate.ProjectID != projectID {
					candidate = runs[index*2+1]
				}
				cursor = &candidate
			}
			query := db.CronRunList{ProjectID: projectID, Limit: limit, JobUID: job}
			query.Statuses = statuses
			if cursor != nil {
				query.BeforeUID = cursor.UID
			}
			want := []db.CronRun{}
			for _, run := range runs {
				if run.ProjectID != projectID || job != "" && *run.JobUID != job || len(statuses) > 0 && !slices.Contains(statuses, run.Status) {
					continue
				}
				if cursor == nil || cronVisibilityNewestFirst(run, *cursor) > 0 {
					want = append(want, run)
				}
			}
			slices.SortFunc(want, cronVisibilityNewestFirst)
			if limit == 0 || limit > 100 {
				limit = 100
			}
			want = want[:min(limit, len(want))]
			got, err := store.ListCronRuns(ctx, query)
			require.NoError(ht, err)
			require.Equal(ht, cronVisibilityRunUIDs(want), cronVisibilityRunUIDs(got))
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})
	t.Run("failed window matches independent history", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			projectID := hegel.Draw(ht, hegel.SampledFrom(projects))
			// This structured subset crosses every fixture instant, including
			// windows narrower than a second; explicit cases pin the 24h edges.
			a := hegel.Draw(ht, hegel.Integers[int64](-48*int64(time.Hour), 48*int64(time.Hour)))
			b := hegel.Draw(ht, hegel.Integers[int64](-48*int64(time.Hour), 48*int64(time.Hour)))
			since, until := base.Add(time.Duration(min(a, b))), base.Add(time.Duration(max(a, b)))
			if hegel.Draw(ht, hegel.Booleans()) {
				// Also draw the full supported UTC timestamp domain; the nearby
				// subset above deliberately concentrates cases around fixture rows.
				low := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
				high := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).Unix()
				x := time.Unix(hegel.Draw(ht, hegel.Integers(low, high)), hegel.Draw(ht, hegel.Integers[int64](0, 999999999)))
				y := time.Unix(hegel.Draw(ht, hegel.Integers(low, high)), hegel.Draw(ht, hegel.Integers[int64](0, 999999999)))
				if x.After(y) {
					x, y = y, x
				}
				since, until = x, y
			}
			offset := hegel.Draw(ht, hegel.Integers(-24*60*60, 24*60*60))
			since = since.In(time.FixedZone("offset", offset))
			var want int64
			for _, run := range runs {
				if run.ProjectID == projectID && run.Status == "failed" && !run.CreatedAt.Before(since) && !run.CreatedAt.After(until) {
					want++
				}
			}
			got, err := store.CountFailedCronRuns(ctx, projectID, since, until)
			require.NoError(ht, err)
			require.Equal(ht, want, got)
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})

	t.Run("arbitrary status validation", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			status := hegel.Draw(ht, hegel.Text())
			query := db.CronRunList{ProjectID: project.ID}
			query.Statuses = []string{status}
			_, err := store.ListCronRuns(ctx, query)
			if slices.Contains(cronVisibilityStatuses, status) {
				require.NoError(ht, err)
			} else {
				require.ErrorIs(ht, err, cron.ErrInvalid)
			}
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})
	return nil
}

func cronVisibilityNewestFirst(a, b db.CronRun) int {
	if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
		return order
	}
	return strings.Compare(b.UID, a.UID)
}

func cronVisibilityRunUIDs(runs []db.CronRun) []string {
	out := make([]string, len(runs))
	for i := range runs {
		out[i] = runs[i].UID
	}
	return out
}

func cronVisibilityLatestUIDs(runs map[string]db.CronRun) map[string]string {
	out := make(map[string]string, len(runs))
	for job, run := range runs {
		out[job] = run.UID
	}
	return out
}

func init() {
	storageScenarios = append(storageScenarios,
		scenario{name: "cron visibility latest", methods: []string{"LatestCronRuns"}, run: checkCronVisibilityLatest},
		scenario{name: "cron visibility status pages", methods: []string{"ListCronRuns"}, run: checkCronVisibilityStatusPages},
		scenario{name: "cron visibility batching", run: checkCronVisibilityBatching},
		scenario{name: "cron visibility failed count", methods: []string{"CountFailedCronRuns"}, run: checkCronVisibilityFailedCount},
		scenario{name: "cron visibility cancellation", run: checkCronVisibilityCancellation},
		scenario{name: "cron visibility properties", run: checkCronVisibilityProperties},
	)
}
