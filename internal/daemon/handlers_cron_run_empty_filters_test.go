package daemon_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCronRunHTTPRejectsEmptyFilters(t *testing.T) {
	for _, operation := range []string{"prune", "counts"} {
		for _, field := range []string{"job_uid", "workflow_uid"} {
			for _, value := range []string{"", " ", "\t"} {
				t.Run(fmt.Sprintf("%s/%s/%q", operation, field, value), func(t *testing.T) {
					env, project, prunePath := cronPruneHTTPFixture(t)
					before, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
					require.NoError(t, err)
					method, path := http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/counts?%s=%s", project.ID, field, url.QueryEscape(value))
					var body any
					var headers map[string]string
					if operation == "prune" {
						method, path = http.MethodPost, prunePath
						body = map[string]any{"actor": "operator", "before": time.Now().Add(-48 * time.Hour), "keep": 1, "dry_run": false, field: value}
						headers = map[string]string{"X-Kata-Confirm": "PRUNE example-project cron runs"}
					}
					response, raw := envDoRaw(t, env, method, path, body, headers)
					require.Equal(t, http.StatusBadRequest, response.StatusCode, string(raw))
					after, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
					require.NoError(t, err)
					require.Equal(t, before, after)
				})
			}
		}
	}
}

func TestCronRunPruneRejectsUTCOutOfDomainCutoff(t *testing.T) {
	env, _, path := cronPruneHTTPFixture(t)
	response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "operator", "before": "0001-01-01T00:00:00+01:00", "keep": 1, "dry_run": true}, nil)
	require.Equal(t, http.StatusBadRequest, response.StatusCode, string(raw))
}
