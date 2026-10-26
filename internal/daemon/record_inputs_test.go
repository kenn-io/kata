package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/uid"
)

func TestRecordTeammateRawInputPolicy(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, signer, _ := dbtest.RecordTeammateRoot(t, store)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "worker"})
		require.NoError(t, err)
		definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
		require.NoError(t, err)
		job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}, RootSigning: daemon.NewRootSigning(signer)})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		for _, kind := range []string{"comment", "cron"} {
			for _, raw := range []string{`null`, `true`, `42`, `[]`, `{}`, `"bad/handle"`} {
				t.Run(kind+"/"+raw, func(t *testing.T) {
					method, path, body := http.MethodPost, issueURLRef(project.ID, issue.ShortID, "comments"), fmt.Sprintf(`{"actor":"worker","body":"Finding","teammate":%s}`, raw)
					if kind == "cron" {
						id, err := uid.New()
						require.NoError(t, err)
						method = http.MethodPut
						path = fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, id)
						body = fmt.Sprintf(`{"actor":"worker","job_uid":%q,"definition_event_uid":%q,"status":"running","summary":{"version":1},"expected_revision":0,"teammate":%s}`, job.UID, job.DefinitionEventUID, raw)
					}
					before, err := store.MaxEventID(t.Context())
					require.NoError(t, err)
					receipts, err := store.AttributionReceiptsAfter(t.Context(), project.UID, 1, 0, 1000)
					require.NoError(t, err)
					req := httptest.NewRequest(method, path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer test-token")
					req.Header.Set("X-Kata-Teammate", "codex")
					out := httptest.NewRecorder()
					server.Handler().ServeHTTP(out, req)
					if raw == "null" {
						require.Equal(t, 200, out.Code, out.Body.String())
						if kind == "comment" {
							comments, err := store.CommentsByIssue(t.Context(), issue.ID)
							require.NoError(t, err)
							require.Equal(t, "codex", comments[len(comments)-1].Teammate)
						} else {
							runs, err := store.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
							require.NoError(t, err)
							require.Len(t, runs, 1)
							require.Nil(t, runs[0].Teammate)
						}
						return
					}
					require.Equal(t, 400, out.Code, out.Body.String())
					after, err := store.MaxEventID(t.Context())
					require.NoError(t, err)
					require.Equal(t, before, after)
					afterReceipts, err := store.AttributionReceiptsAfter(t.Context(), project.UID, 1, 0, 1000)
					require.NoError(t, err)
					require.Equal(t, receipts, afterReceipts)
				})
			}
		}
	})
}

func TestTeammateHeaderIsSingleValued(t *testing.T) {
	d := openTestDB(t)
	project, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: d.db, Auth: config.AuthConfig{Token: "test-token"}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, tc := range []struct {
		name    string
		headers []string
		auth    bool
		status  int
	}{
		{"conflicting", []string{"codex", "teammate-1"}, true, 400},
		{"identical", []string{"codex", "codex"}, true, 400},
		{"empty then valid", []string{"", "codex"}, true, 400},
		{"auth before repeats", []string{"codex", "teammate-1"}, false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := d.db.MaxEventID(t.Context())
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), strings.NewReader(`{"actor":"worker","title":"Task","force_new":true}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.auth {
				req.Header.Set("Authorization", "Bearer test-token")
			}
			for _, value := range tc.headers {
				req.Header.Add("X-Kata-Teammate", value)
			}
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, req)
			require.Equal(t, tc.status, out.Code, out.Body.String())
			after, err := d.db.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}
