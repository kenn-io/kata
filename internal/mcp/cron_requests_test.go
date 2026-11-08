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
	args := map[string]any{"uid": job["uid"], "request_uid": id, "ttl_seconds": 60}
	first := cronOK(t, f.session, "cron_job_request_run", args)
	request := first["request"].(map[string]any)
	require.Equal(t, id, request["uid"])
	require.Equal(t, "example-agent", request["requested_by"])
	require.Equal(t, job["definition_event_uid"], request["definition_event_uid"])
	replay := cronOK(t, f.session, "cron_job_request_run", args)
	require.Equal(t, true, replay["replayed"])
	require.Equal(t, request, replay["request"])
	require.True(t, cronCall(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"]}).IsError)
	require.True(t, cronCall(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"], "request_uid": cronNewUID(t, f.session), "ttl_seconds": 59}).IsError)
	runID := cronNewUID(t, f.session)
	jobID := job["uid"].(string)
	definitionID := job["definition_event_uid"].(string)
	_, err := f.store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: f.project.ID, UID: runID, Actor: "example-agent", JobUID: &jobID, DefinitionEventUID: &definitionID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	cancel := cronOK(t, f.session, "cron_run_request_cancel", map[string]any{"uid": runID, "request_uid": cronNewUID(t, f.session)})["request"].(map[string]any)
	require.Equal(t, "cancel", cancel["kind"])
	require.Equal(t, runID, cancel["run_uid"])
	list := cronOK(t, f.session, "cron_request_list", map[string]any{"state": "pending"})
	require.Len(t, list["requests"], 2)
	require.True(t, cronCall(t, f.session, "cron_run_request_cancel", map[string]any{"uid": runID}).IsError)
}
