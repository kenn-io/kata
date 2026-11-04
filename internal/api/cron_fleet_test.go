package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/require"
)

func TestCronFleetMalformedQueryRejected(t *testing.T) {
	for _, raw := range []string{"project_ids=%ZZ", "project_ids=1&project_ids=%ZZ", "status=failed%ZZ"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cron/runs", nil)
		req.URL.RawQuery = raw
		ctx := humatest.NewContext(&huma.Operation{}, req, httptest.NewRecorder())
		var runs AllCronRunsRequest
		require.NotEmpty(t, runs.Resolve(ctx), raw)
		if raw[:3] == "pro" {
			var jobs AllCronJobsRequest
			require.NotEmpty(t, jobs.Resolve(ctx), raw)
		}
	}
}

// Hegel shrank a silently dropped restriction to a lone semicolon.
func TestCronFleetSemicolonCannotDropFilter(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cron/runs?project_ids=1&status=;", nil)
	ctx := humatest.NewContext(&huma.Operation{}, req, httptest.NewRecorder())
	var in AllCronRunsRequest
	require.NotEmpty(t, in.Resolve(ctx))
}

// Existing CLI clients send empty legacy cursor/job parameters on unfiltered reads.
func TestCronFleetLegacyEmptyPaginationAllowed(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/projects/1/cron/runs?job_uid=&before_uid=", nil)
	ctx := humatest.NewContext(&huma.Operation{}, req, httptest.NewRecorder())
	var in CronRunsRequest
	require.Empty(t, in.Resolve(ctx))
	native, err := in.Native(1, nil, false)
	require.NoError(t, err)
	require.Empty(t, native.JobUID)
	require.Empty(t, native.BeforeUID)
}
