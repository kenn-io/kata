package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func TestSyncFederationCronRunReferencesMovedIssue(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "run-source", "run-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := hub.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: projects[0].ID, Name: "Review job", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Run issue", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = spoke.env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: spoke.projects[0].ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, IssueUID: &issue.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0), "a run identity must not conflict with its referenced issue; joined current-project proof is automatic")
	retained, err := hub.DB.CronRun(t.Context(), projects[0].ID, runUID)
	require.NoError(t, err)
	require.Equal(t, &issue.UID, retained.IssueUID)
	require.Equal(t, "running", retained.Status)
	_, err = spoke.env.DB.ActiveFederationQuarantine(t.Context(), spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound, "valid cron evidence must not become a poisoned push")
}
