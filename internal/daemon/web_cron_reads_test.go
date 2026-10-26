package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The web Cron page reads summaries, definitions and runs; every cron write
// and the capability probe stay outside local browser authority.
func TestWebLocalCronReads(t *testing.T) {
	for _, tail := range []string{"summary", "jobs", "jobs/job-uid", "runs", "runs/run-uid", "workflows", "workflows/workflow-uid"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			r := httptest.NewRequest(method, "/api/v1/projects/1/cron/"+tail, nil)
			if got := webLocalSPARequestAllowed(r); got != (method == http.MethodGet) {
				t.Errorf("%s %s allowed=%v", method, tail, got)
			}
		}
	}
	for _, path := range []string{
		"/api/v1/projects/1/cron/capabilities",
		"/api/v1/projects/no/cron/runs",
		"/api/v1/projects/1/cron/runs/id/observe",
		"/api/v1/projects/1/cron/summary/extra",
		"/api/v1/projects/1/cron/runs/",
		"/api/v1/projects/1/cron/jobs/id/extra",
	} {
		if webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("allowed %s", path)
		}
	}
	if !webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/projects/1/issues/issue-ref", nil)) {
		t.Error("linked issue read denied")
	}
	if webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/projects/1/issues/issue-ref?include_deleted=true", nil)) {
		t.Error("deleted issue read allowed")
	}
}
