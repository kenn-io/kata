package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCronObservationSchemaIndependentRuns(t *testing.T) {
	d, ctx, project, issue := setupTestIssue(t)
	const job = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const event = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	for _, id := range []string{"01ARZ3NDEKTSV4RRFFQ69G5FAX", "01ARZ3NDEKTSV4RRFFQ69G5FAY"} {
		_, err := d.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,occurrence_key,issue_uid,actor,status) VALUES(?, ?, ?, ?, 'same-occurrence', ?, 'worker', 'running')`, id, project.ID, job, event, issue.UID)
		require.NoError(t, err, "separate run UIDs may reference the same occurrence and issue")
	}
	var count int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_runs WHERE project_id=? AND occurrence_key='same-occurrence'`, project.ID).Scan(&count))
	require.Equal(t, 2, count)
	_, err := d.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status) VALUES(?, ?, ?, ?, 'worker', 'running')`, "01ARZ3NDEKTSV4RRFFQ69G5FAX", project.ID, job, event)
	require.Error(t, err, "run UID is still unique")
}
