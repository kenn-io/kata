package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronRequestTools(t *testing.T) {
	f := newCronFixture(t)
	definition := cronDefinition()
	definition["enabled"] = true
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Execute review", "definition": definition}, "job")
	id := cronNewUID(t, f.session)
	request := cronRecord(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"], "request_uid": id, "ttl_seconds": 60}, "request")
	require.Equal(t, id, request["uid"])
	require.Equal(t, "example-agent", request["requested_by"])
	require.Equal(t, job["definition_event_uid"], request["definition_event_uid"])
	require.True(t, cronCall(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"]}).IsError)
	runID := cronNewUID(t, f.session)
	jobID, definitionID := job["uid"].(string), job["definition_event_uid"].(string)
	_, err := f.store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: f.project.ID, UID: runID, Actor: "example-agent", JobUID: &jobID, DefinitionEventUID: &definitionID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	cancel := cronRecord(t, f.session, "cron_run_request_cancel", map[string]any{"uid": runID, "request_uid": cronNewUID(t, f.session)}, "request")
	require.Equal(t, "cancel", cancel["kind"])
	require.Equal(t, runID, cancel["run_uid"])
	require.Len(t, cronOK(t, f.session, "cron_request_list", map[string]any{"state": "pending"})["requests"], 2)
}
