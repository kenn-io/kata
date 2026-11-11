package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronNextRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"bad"}, "cron identity must be a 26-character ULID"},
		{[]string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "--count", "0"}, "count must be between 1 and 100"},
		{[]string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "--count", "101"}, "count must be between 1 and 100"},
		{[]string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "--from", "tomorrow"}, "from must be RFC3339"},
	} {
		out, _, err := executeCronCLI(t, "http://daemon.example", nil, append([]string{"job", "next"}, tc.args...)...)
		var cli *cliError
		require.ErrorAs(t, err, &cli)
		require.Equal(t, kindValidation, cli.Kind)
		require.Equal(t, tc.message, cli.Message)
		require.Empty(t, out)
	}
}

func TestCronNextSavedSchedules(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	native := `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":7200,"timezone":"Local"},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FB9","title":"Inspection"},"overlap":"allow","catchup":"all"}`
	legacy := strings.Replace(native, `"kind":"interval","interval_seconds":7200,"timezone":"Local"`, `"kind":"cron","cron":"0 0 30 2 *"`, 1)
	parent := `{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FB9","project_id":1,"status":"open","revision":7,"metadata":{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{"version":1,"anchor":"2026-06-01T00:00:00Z","through":"2026-06-01T06:00:00Z"}}}`
	for _, tc := range []struct {
		name, definition, parent, want string
		invalid, deleted               bool
	}{
		{"local once", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"once","at":"2026-06-01T08:00:00Z","timezone":"Local"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}`, "", "2026-06-01T08:00:00Z", false, false},
		{"adapter implicit zone", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 9 * * *"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}`, "", "cron_preview_unavailable", true, false},
		{"native interval", native, parent, "2026-06-01T08:00:00Z", false, false},
		{"native local cron", strings.Replace(native, `"kind":"interval","interval_seconds":7200`, `"kind":"cron","cron":"0 9 * * *"`, 1), parent, "2026-06-01T09:00:00Z", false, false},
		{"archived parent", native, strings.Replace(parent, `"status":"open"`, `"status":"open","deleted_at":"2026-01-01T00:00:00Z"`, 1), `"status":"parent_closed"`, false, false},
		{"closed parent without progress", native, strings.Replace(parent, `"status":"open","revision":7,"metadata":{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{"version":1,"anchor":"2026-06-01T00:00:00Z","through":"2026-06-01T06:00:00Z"}}`, `"status":"closed","metadata":{}`, 1), "cron_preview_unavailable", true, false},
		{"missing parent", native, "error:issue_not_found", "cron_preview_unavailable", true, false},
		{"missing project", native, "error:project_not_found", "project_not_found", true, false},
		{"forbidden parent", native, "error:forbidden", "forbidden", true, false},
		{"future definition", strings.Replace(native, `"version":1`, `"version":2`, 1), parent, "", true, false},
		{"paused legacy", strings.Replace(legacy, `"enabled":true`, `"enabled":false`, 1), "", `"status":"paused"`, false, false},
		{"deleted legacy", legacy, "", `"status":"deleted"`, false, true},
		{"enabled legacy", legacy, parent, "", true, false},
		{"paused unknown field", strings.Replace(strings.Replace(native, `"enabled":true`, `"enabled":false`, 1), `"version":1`, `"version":1,"future":true`, 1), "", "", true, false},
		{"deleted unknown field", strings.Replace(native, `"version":1`, `"version":1,"future":true`, 1), "", "", true, true},
		{"deleted future definition", strings.Replace(native, `"version":1`, `"version":2`, 1), parent, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "adapter implicit zone" {
				previous := time.Local
				time.Local = time.FixedZone("adapter", -4*3600)
				t.Cleanup(func() { time.Local = previous })
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/projects":
					_, _ = fmt.Fprint(w, `{"projects":[{"id":1,"name":"spoke-project"}]}`)
				case r.URL.Path == "/api/v1/projects/1":
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"},"aliases":[]}`)
				case strings.HasSuffix(r.URL.Path, "/cron/capabilities"):
					w.Header().Set(db.EventFeaturesHeader, db.CronEventFeature)
					_, _ = fmt.Fprint(w, `{}`)
				case strings.Contains(r.URL.Path, "/cron/jobs/"):
					deleted := ""
					if tc.deleted {
						deleted = `,"deleted_at":"2026-01-01T00:00:00Z"`
					}
					_, _ = fmt.Fprintf(w, `{"job":{"uid":"%s","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","definition":%s%s}}`, id, tc.definition, deleted)
				case strings.Contains(r.URL.Path, "/issues/"):
					if r.URL.Query().Get("include_deleted") != "true" {
						t.Error("parent read must include deleted issues")
					}
					if code, ok := strings.CutPrefix(tc.parent, "error:"); ok {
						status := http.StatusNotFound
						if code == "forbidden" {
							status = http.StatusForbidden
						}
						w.WriteHeader(status)
						_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"failed"}}`, code)
						return
					}
					if tc.parent == "" {
						t.Error("unexpected parent read")
					}
					_, _ = fmt.Fprintf(w, `{"issue":%s}`, tc.parent)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			t.Cleanup(server.Close)
			out, _, err := executeCronCLI(t, server.URL, nil, "job", "next", id, "--count", "1", "--from", "2026-06-01T01:00:00Z")
			if tc.invalid {
				require.Error(t, err)
				require.Empty(t, out)
				if tc.want != "" {
					var cli *cliError
					require.ErrorAs(t, err, &cli)
					require.Equal(t, tc.want, cli.Code)
					if tc.want == "cron_preview_unavailable" {
						require.Equal(t, "cron preview unavailable", cli.Message)
					}
					return
				}
				require.ErrorContains(t, err, "cannot preview saved job with this kata")
				return
			}
			require.NoError(t, err)
			require.Contains(t, out, tc.want)
			if tc.name == "local once" || tc.name == "native interval" || tc.name == "native local cron" {
				previous := time.Local
				t.Cleanup(func() { time.Local = previous })
				for _, offset := range []int{-5 * 3600, 2 * 3600} {
					time.Local = time.FixedZone("client", offset)
					out, _, err := executeCronCLI(t, server.URL, nil, "job", "next", id, "--count", "1", "--from", "2026-06-01T01:00:00Z")
					require.NoError(t, err)
					require.Contains(t, out, `"timezone":"UTC"`)
					require.Contains(t, out, `"local":"`+tc.want+`"`)
				}
			}
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

	def.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "Local"}
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Local cron", Actor: "tester", Definition: def})
	require.NoError(t, err)
	before, err = env.DB.IssueByUID(t.Context(), parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var metadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(before.Metadata), &metadata))
	progress, err := cron.DecodeRecurringProgress(metadata["cron.progress."+job.UID])
	require.NoError(t, err)
	start := progress.Through.UTC()
	first := time.Date(start.Year(), start.Month(), start.Day(), 9, 0, 0, 0, time.UTC)
	if !first.After(start) {
		first = first.AddDate(0, 0, 1)
	}
	want := []time.Time{first, first.AddDate(0, 0, 1), first.AddDate(0, 0, 2)}
	previous := time.Local
	t.Cleanup(func() { time.Local = previous })
	for _, offset := range []int{-5 * 3600, 2 * 3600} {
		time.Local = time.FixedZone("client", offset)
		batch, err := cron.PlanRecurring(def, job.DefinitionEventUID, progress, want[2])
		require.NoError(t, err)
		require.Equal(t, want, batch.Due)
		out, _, err := executeCronCLI(t, env.URL, nil, "job", "next", job.UID, "--from", start.Format(time.RFC3339Nano))
		require.NoError(t, err)
		require.Contains(t, out, `"timezone":"UTC"`)
		for _, instant := range want {
			require.Contains(t, out, `"local":"`+instant.Format(time.RFC3339)+`"`)
		}
	}
	after, err = env.DB.IssueByUID(t.Context(), parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before.Metadata, after.Metadata)
}

func TestCronNextRootErrorEnvelope(t *testing.T) {
	out, diagnostic, err := executeRootCapture(t, t.Context(), "--json", "cron", "job", "next", "bad")
	require.Error(t, err)
	require.Empty(t, out)
	envelope := parseErrorEnvelope(t, []byte(diagnostic))
	require.Equal(t, "validation", envelope.Error.Kind)
}

func TestCronNextModes(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	def := cronCLIJob(t)
	def.Definition.Enabled = true
	def.Definition.Trigger = cron.Trigger{Kind: "once", At: "2090-01-01T12:00:00Z", Timezone: "UTC"}
	jobID := cronCLIUID(t)
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", jobID, "--file", cronCLIFile(t, def))
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--project", project.Name, "cron", "job", "next", jobID, "--from", "2091-01-01T00:00:00Z")
	require.NoError(t, err)
	require.Equal(t, "Adapter trigger times (UTC, exhausted)\n", out)
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"human", nil, "Adapter trigger times"},
		{"json", []string{"--json"}, `"status":"ready"`},
		{"agent", []string{"--agent"}, `"status":"ready"`},
		{"quiet", []string{"--quiet"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.flags, "--project", project.Name, "cron", "job", "next", jobID, "--from", "2089-01-01T00:00:00Z")
			out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), args...)
			require.NoError(t, err)
			if tc.want == "" {
				require.Empty(t, out)
			} else {
				require.Contains(t, out, tc.want)
			}
			if tc.name == "json" || tc.name == "agent" {
				require.Contains(t, out, `"basis":"adapter_trigger"`)
				require.Contains(t, out, `"definition_event_uid":`)
			}
		})
	}
}

func TestCronNextRequiresCapability(t *testing.T) {
	for _, tc := range []struct {
		name, feature, code string
		status              int
	}{
		{"absent", "", "cron_preview_unsupported", 200},
		{"unknown", "future_v2", "cron_preview_unsupported", 200},
		{"denied", "", "forbidden", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					_, _ = fmt.Fprint(w, `{"projects":[{"id":1,"name":"spoke-project"}]}`)
				case "/api/v1/projects/1":
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"},"aliases":[]}`)
				case "/api/v1/projects/1/cron/capabilities":
					w.Header().Set(db.EventFeaturesHeader, tc.feature)
					w.WriteHeader(tc.status)
					if tc.status == 200 {
						_, _ = fmt.Fprint(w, `{}`)
					} else {
						_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"denied"}}`, tc.code)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			t.Cleanup(server.Close)
			out, _, err := executeCronCLI(t, server.URL, nil, "job", "next", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
			require.Empty(t, out)
			var cli *cliError
			require.ErrorAs(t, err, &cli)
			require.Equal(t, tc.code, cli.Code)
		})
	}
}
