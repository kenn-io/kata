package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The web Cron page reads summaries, definitions and runs; the capability
// probe stays outside local browser authority. TestWebLocalCronDefinitionWrites
// covers the definition writes.
func TestWebLocalCronReads(t *testing.T) {
	for _, tail := range []string{"summary", "jobs", "runs", "runs/run-uid", "workflows"} {
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
	for _, path := range []string{"/api/v1/projects/1/cron/jobs/job-uid", "/api/v1/projects/1/cron/workflows/workflow-uid"} {
		if !webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("denied %s", path)
		}
	}
	if !webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/projects/1/issues/issue-ref", nil)) {
		t.Error("linked issue read denied")
	}
	if webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/projects/1/issues/issue-ref?include_deleted=true", nil)) {
		t.Error("deleted issue read allowed")
	}
	// A linked issue keeps its UID when it moves to another project, so the
	// drawer may read it by UID alone.
	if !webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/issues/issue-uid", nil)) {
		t.Error("linked issue read by UID denied")
	}
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/issues/issue-uid?include_deleted=true", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/issues/", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/issues/issue-uid/extra", nil),
		httptest.NewRequest(http.MethodPatch, "/api/v1/issues/issue-uid", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/issues/issue-uid", nil),
	} {
		if webLocalSPARequestAllowed(r) {
			t.Errorf("allowed %s %s", r.Method, r.URL)
		}
	}
}
