package dbtest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// A stored definition that a later release's rules would reject must stay
// readable, exportable and deletable. Validation guards writes, not reads.
func checkCronReadsTolerateTightenedRules(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)
	// An execute action with neither prompt nor workflow fails today's rules
	// but is a well-formed document, as an older release's row would be.
	stale := `{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, stale, job.UID)
	require.NoError(t, err)

	read, err := store.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Error(t, read.Definition.Validate(), "the fixture must violate current rules")
	listed, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	_, err = CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)

	deleted, _, err := store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: read.Name, Definition: read.Definition, ExpectedEventUID: read.DefinitionEventUID, Actor: "worker", Deleted: true})
	require.NoError(t, err, "a stale document must not block deletion")
	require.NotNil(t, deleted.DeletedAt)
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: read.Name, Definition: read.Definition, ExpectedEventUID: deleted.DefinitionEventUID, Actor: "worker"})
	require.ErrorIs(t, err, cron.ErrInvalid, "restoring still requires a document that passes current rules")
	return nil
}
