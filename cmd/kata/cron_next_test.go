package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronNextRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{{"bad"}, {"01ARZ3NDEKTSV4RRFFQ69G5FAV", "--count", "0"}, {"01ARZ3NDEKTSV4RRFFQ69G5FAV", "--from", "tomorrow"}} {
		out, _, err := executeCronCLI(t, "http://daemon.example", nil, append([]string{"job", "next"}, args...)...)
		require.Error(t, err)
		require.Empty(t, out)
	}
}

func TestCronNextSavedSchedules(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, tc := range []struct{ name, definition, parent, want string }{
		{"local once", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"once","at":"2026-06-01T08:00:00Z","timezone":"Local"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}`, "", "2026-06-01T08:00:00Z"},
		{"native interval", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":7200,"timezone":"Local"},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FB9","title":"Inspection"},"overlap":"allow","catchup":"all"}`, `{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FB9","project_id":1,"status":"open","revision":7,"metadata":{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{"version":1,"anchor":"2026-06-01T00:00:00Z","through":"2026-06-01T06:00:00Z"}}}`, "2026-06-01T08:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/projects":
					fmt.Fprint(w, `{"projects":[{"id":1,"name":"spoke-project"}]}`)
				case r.URL.Path == "/api/v1/projects/1":
					fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"},"aliases":[]}`)
				case strings.Contains(r.URL.Path, "/cron/jobs/"):
					fmt.Fprintf(w, `{"job":{"uid":"%s","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","definition":%s}}`, id, tc.definition)
				case strings.Contains(r.URL.Path, "/issues/"):
					fmt.Fprintf(w, `{"issue":%s}`, tc.parent)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			t.Cleanup(server.Close)
			out, _, err := executeCronCLI(t, server.URL, nil, "job", "next", id, "--count", "1", "--from", "2026-06-01T01:00:00Z")
			require.NoError(t, err)
			require.Contains(t, out, tc.want)
		})
	}
}

func TestCronNextLeavesProgressUnchanged(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "tester"})
	require.NoError(t, err)
	def := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "all"}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "tester", Definition: def})
	require.NoError(t, err)
	before, err := env.DB.IssueByUID(t.Context(), parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	out, _, err := executeCronCLI(t, env.URL, nil, "job", "next", job.UID)
	require.NoError(t, err)
	require.Contains(t, out, `"status":"ready"`)
	after, err := env.DB.IssueByUID(t.Context(), parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before.Metadata, after.Metadata)
}

func TestCronNextParentReadErrors(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, code := range []string{"issue_not_found", "project_not_found", "forbidden"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/projects":
					fmt.Fprint(w, `{"projects":[{"id":1,"name":"spoke-project"}]}`)
				case r.URL.Path == "/api/v1/projects/1":
					fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"},"aliases":[]}`)
				case strings.Contains(r.URL.Path, "/cron/jobs/"):
					fmt.Fprintf(w, `{"job":{"uid":"%s","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":7200},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FB9","title":"Review"},"overlap":"allow","catchup":"all"}}}`, id)
				case strings.Contains(r.URL.Path, "/issues/"):
					require.Equal(t, "true", r.URL.Query().Get("include_deleted"))
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprintf(w, `{"error":{"code":%q,"message":"failed"}}`, code)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			t.Cleanup(server.Close)
			out, _, err := executeCronCLI(t, server.URL, nil, "job", "next", id)
			require.Empty(t, out)
			var got *cliError
			require.ErrorAs(t, err, &got)
			if code == "issue_not_found" {
				require.Equal(t, "cron_preview_unavailable", got.Code)
				require.Contains(t, string(got.Data), `"status":"unavailable"`)
			} else {
				require.Equal(t, code, got.Code)
			}
		})
	}
}

func TestCronNextOutputModes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  outputMode
		quiet bool
		want  string
	}{
		{"human", outputHuman, false, "Adapter trigger times (UTC, exhausted):\n  exhausted"},
		{"json", outputJSON, false, `"status":"exhausted"`},
		{"agent", outputAgent, false, `"status":"exhausted"`},
		{"quiet", outputHuman, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			flags.Mode, flags.Quiet = tc.mode, tc.quiet
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			err := emitCronNext(cmd, cronNextResult{Delivery: "adapter", Timezone: "UTC", Status: "exhausted", Occurrences: []cronNextOccurrence{}, Warnings: []string{}})
			require.NoError(t, err)
			if tc.quiet {
				require.Empty(t, out.String())
			} else {
				require.Contains(t, out.String(), tc.want)
			}
		})
	}
}

func TestCronNextRootErrorEnvelope(t *testing.T) {
	out, diagnostic, err := executeRootCapture(t, t.Context(), "--json", "cron", "job", "next", "bad")
	require.Error(t, err)
	require.Empty(t, out)
	envelope := parseErrorEnvelope(t, []byte(diagnostic))
	require.Equal(t, "validation", envelope.Error.Kind)
}
