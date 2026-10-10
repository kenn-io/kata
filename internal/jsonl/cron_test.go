package jsonl_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

type nativeJSONLFixture struct {
	data       []byte
	beforeCron []byte
	records    []db.ImportRecord
	projectID  int64
}

func buildNativeCronJSONLFixture(t *testing.T) nativeJSONLFixture {
	t.Helper()
	ctx := context.Background()
	source := openExportTestDB(t)
	project, err := source.CreateProject(ctx, "example-cron")
	require.NoError(t, err)
	var prior bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &prior, jsonl.ExportOptions{IncludeDeleted: true}))
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip","secret_refs":{"provider":"review-provider"},"checkout_key":"main"}`))
	require.NoError(t, err)
	workflow, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review workflow", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
	require.NoError(t, err)
	job, _, err := source.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)

	occurrence := "manual:review"
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	runRecords := []db.ImportRecord{}
	for range 2 {
		runUID, err := uid.New()
		require.NoError(t, err)
		result, err := source.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, IssueUID: &issue.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1, Message: "Review started", InputTokens: 123}})
		require.NoError(t, err)
		run := db.CronRunExport(result.Run)
		runRecords = append(runRecords, &run)
	}
	var exported bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &exported, jsonl.ExportOptions{IncludeDeleted: true}))
	jobRecord := db.CronJobExport(job)
	workflowRecord := db.CronWorkflowExport(workflow)
	return nativeJSONLFixture{data: exported.Bytes(), beforeCron: prior.Bytes(), records: append([]db.ImportRecord{&jobRecord, &workflowRecord}, runRecords...), projectID: project.ID}
}

func assertNativeCronRecords(t *testing.T, store db.Storage, fixture nativeJSONLFixture) {
	t.Helper()
	all, err := dbtest.CollectImportRecords(t.Context(), store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	var actual []db.ImportRecord
	for _, record := range all {
		if strings.HasPrefix(record.ImportKind(), "cron_") {
			actual = append(actual, record)
		}
	}
	require.Equal(t, fixture.records, actual)
}

func TestNativeCronJSONLRoundTrip(t *testing.T) {
	fixture := buildNativeCronJSONLFixture(t)
	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(fixture.data), restored))
	assertNativeCronRecords(t, restored, fixture)
	require.NotContains(t, string(fixture.data), `"checkout_path"`)
	require.NotContains(t, string(fixture.data), `"api_token"`)
}

func TestNativeCronJSONLRoundTripAcrossPostgres(t *testing.T) {
	fixture := buildNativeCronJSONLFixture(t)
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgres.Close() })
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(fixture.data), postgres))
	assertNativeCronRecords(t, postgres, fixture)
	var exported bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, postgres, &exported, jsonl.ExportOptions{IncludeDeleted: true}))
	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(exported.Bytes()), restored))
	assertNativeCronRecords(t, restored, fixture)
	// Explicit IDs restored across engines must advance each identity sequence.
	original := db.CronJob(*fixture.records[0].(*db.CronJobExport))
	next, _, err := postgres.PutCronJob(ctx, db.PutCronJob{ProjectID: fixture.projectID, Name: "Next review", Definition: original.Definition, Actor: "worker"})
	require.NoError(t, err)
	require.Greater(t, next.ID, original.ID)
}

func TestNativeCronJSONLRunningEvidenceDoesNotGateRestore(t *testing.T) {
	fixture := buildNativeCronJSONLFixture(t)
	target := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(fixture.data), target))
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(fixture.beforeCron), target))
	runs, err := target.ListCronRuns(t.Context(), db.CronRunList{ProjectID: fixture.projectID})
	require.NoError(t, err)
	require.Empty(t, runs)
}
