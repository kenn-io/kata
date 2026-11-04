package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronDefinitionVersionsRoutes(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { cfg.EmbeddingProfile = daemon.EmbeddingProfile("restricted") })
	project := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Definition: cronHTTPDefinition(t), Actor: "operator"})
	require.NoError(t, err)
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect workflow", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}, Actor: "operator"})
	require.NoError(t, err)
	for _, tc := range []struct{ kind, uid string }{{"jobs", job.UID}, {"workflows", workflow.UID}} {
		path := fmt.Sprintf("/api/v1/projects/%d/cron/%s/%s/versions", project.ID, tc.kind, tc.uid)
		status, raw := cronHTTP(t, env, http.MethodGet, path, nil)
		require.Equalf(t, http.StatusOK, status, "%s", raw)
		var page struct {
			Versions []struct {
				EventUID  string `json:"event_uid"`
				Operation string `json:"operation"`
				Actor     string `json:"actor"`
				Current   bool   `json:"current"`
			} `json:"versions"`
			Next string `json:"next_before_event_uid"`
		}
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Versions, 1)
		require.True(t, page.Versions[0].Current)
		require.Equal(t, "created", page.Versions[0].Operation)
		require.Equal(t, "operator", page.Versions[0].Actor)
		require.Empty(t, page.Next)
		for _, suffix := range []string{"?limit=-1", "?limit=101", "?limit=abc", "?before_event_uid=invalid", "?before_event_uid=" + cronHTTPUID(t)} {
			status, _ := cronHTTP(t, env, http.MethodGet, path+suffix, nil)
			require.Equal(t, http.StatusBadRequest, status, suffix)
		}
		status, raw = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/%s/%s/versions", other.ID, tc.kind, tc.uid), nil)
		require.Equalf(t, http.StatusNotFound, status, "%s", raw)
		status, _ = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/%s/%s/versions", project.ID, tc.kind, cronHTTPUID(t)), nil)
		require.Equal(t, http.StatusNotFound, status)
	}
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, Actor: "archiver", Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Deleted: true})
	require.NoError(t, err)
	workflow, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: workflow.UID, Name: workflow.Name, Actor: "archiver", Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Deleted: true})
	require.NoError(t, err)
	for _, tc := range []struct{ kind, uid string }{{"jobs", job.UID}, {"workflows", workflow.UID}} {
		status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/%s/%s/versions", project.ID, tc.kind, tc.uid), nil)
		require.Equal(t, http.StatusOK, status)
		var page db.CronVersionPage
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Versions, 2)
		require.Equal(t, "deleted", page.Versions[0].Operation)
		require.True(t, page.Versions[0].Current)
		require.NotNil(t, page.Versions[0].DeletedAt)
	}
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: project.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	for _, tc := range []struct{ kind, uid string }{{"jobs", job.UID}, {"workflows", workflow.UID}} {
		status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/%s/%s/versions", project.ID, tc.kind, tc.uid), nil)
		require.Equal(t, http.StatusNotFound, status)
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(raw, &envelope))
		require.Equal(t, "project_not_found", envelope.Error.Code)
	}
}
