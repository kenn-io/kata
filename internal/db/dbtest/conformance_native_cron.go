package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Dormant installations contain storage for portable history without
// creating jobs, workflows, or runs, and an ordinary restore keeps it empty.
func checkNativeCronDormancy(t *testing.T, store db.Storage) error {
	t.Helper()
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	requireEmpty := func(message string) {
		for _, table := range []string{"cron_jobs", "cron_workflows", "cron_runs"} {
			var count int
			require.NoError(t, q.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
			require.Zero(t, count, message+": "+table)
		}
	}
	requireEmpty("fresh store")
	records, err := CollectImportRecords(t.Context(), store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	require.NoError(t, store.ImportReplay(t.Context(), records, db.ImportOptions{}))
	requireEmpty("ordinary restore without cron records")
	return nil
}

func checkNativeCronConstraints(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	project, err := store.CreateProject(ctx, "example-cron")
	require.NoError(t, err)
	const jobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const eventUID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	const runUID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	definition := `{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`
	hlc := `{"version":1,"physical_ms":1,"counter":0,"origin_instance_uid":"` + store.InstanceUID() + `"}`
	_, err = q.ExecContext(ctx, `INSERT INTO cron_jobs(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author) VALUES($1,$2,'Review',$3,$4,$5,'worker')`, jobUID, project.ID, definition, eventUID, hlc)
	require.NoError(t, err)
	for _, change := range []struct {
		column string
		value  any
	}{
		{"revision", -1}, {"definition_json", `{"version":"1"}`}, {"definition_hlc_json", `{"version":1,"physical_ms":-1,"counter":-1}`}, {"definition_json", `{"version":2}`}, {"definition_json", `{}`},
		{"definition_json", `{"version":1,"value":"` + strings.Repeat("x", 256*1024) + `"}`},
		{"uid", "invalid"}, {"uid", strings.ToLower(jobUID)},
	} {
		_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET `+change.column+`=$1 WHERE uid=$2`, change.value, jobUID)
		require.Error(t, err, change.column)
	}
	_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,occurrence_key,actor,status) VALUES($1,$2,$3,$4,'same-occurrence','worker','running')`, runUID, project.ID, jobUID, eventUID)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,occurrence_key,actor,status) VALUES($1,$2,$3,$4,'same-occurrence','worker','running')`, "01ARZ3NDEKTSV4RRFFQ69G5FAY", project.ID, jobUID, eventUID)
	require.NoError(t, err)
	for _, statement := range []string{`UPDATE cron_runs SET status='invented' WHERE uid=$1`, `UPDATE cron_runs SET actor='' WHERE uid=$1`, `UPDATE cron_runs SET summary_json='{"version":1,"input_tokens":-1}' WHERE uid=$1`, `UPDATE cron_runs SET revision=-1 WHERE uid=$1`, `UPDATE cron_runs SET definition_event_uid=NULL WHERE uid=$1`, `UPDATE cron_runs SET workflow_uid='invalid' WHERE uid=$1`} {
		_, err = q.ExecContext(ctx, statement, runUID)
		require.Error(t, err)
	}
	return nil
}
