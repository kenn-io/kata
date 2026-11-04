package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// An incomplete namespace rename breaks real CLI requests or the HTTP routes
// used by clients, even when both components still compile.
func TestCronNamespaceExecutesAgainstDaemon(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, command := range [][]string{
		{"capabilities"},
		{"job", "list"},
		{"workflow", "list"},
		{"run", "list", "--limit", "100"},
	} {
		t.Run(command[0]+fmt.Sprint(command[1:]), func(t *testing.T) {
			resetFlags(t)
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetContext(contextWithBaseURL(t.Context(), env.URL))
			root.SetArgs(append([]string{"--project", "spoke-project", "--json", "cron"}, command...))
			require.NoError(t, root.Execute(), out.String())
			var response map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &response))
			if command[0] == "capabilities" {
				require.Contains(t, response["event_features"], "cron_v1")
			} else {
				require.Contains(t, response, command[0]+"s")
			}
		})
	}
	t.Run("HTTP capabilities", func(t *testing.T) {
		status, body := env.Get(t, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID))
		require.Equal(t, 200, status, string(body))
		var response map[string]any
		require.NoError(t, json.Unmarshal(body, &response))
		require.Contains(t, response["event_features"], "cron_v1")
	})
}

// A configured native job creates ordinary children and exact inbox requests through the daemon.
func TestCronRecurringChildrenContract(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	project, e := env.DB.CreateProject(t.Context(), "recurring-project")
	require.NoError(t, e)
	parent, _, e := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Ongoing review", Author: "operator"})
	require.NoError(t, e)
	execute := func(args []string, input string) []byte {
		t.Helper()
		resetFlags(t)
		root := newRootCmd()
		var out, diagnostics bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&diagnostics)
		root.SetIn(strings.NewReader(input))
		root.SetContext(contextWithBaseURL(t.Context(), env.URL))
		root.SetArgs(append([]string{"--project", "recurring-project", "--json"}, args...))
		require.NoError(t, root.Execute(), diagnostics.String())
		return out.Bytes()
	}
	request := fmt.Sprintf(`{"name":"Review every two hours","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":7200},"action":{"kind":"notify","recipient":"operator/consumer"},"issue":{"kind":"per-run","parent_uid":%q,"title":"Review service"},"overlap":"allow","catchup":"latest"}}`, parent.UID)
	raw := execute([]string{"cron", "job", "create", "--file", "-"}, request)
	var response struct {
		Job db.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.NotEmpty(t, response.Job.UID)
	parent, e = env.DB.IssueByID(t.Context(), parent.ID)
	require.NoError(t, e)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &values))
	var progress cron.RecurringProgress
	require.NoError(t, json.Unmarshal(values["cron.progress."+response.Job.UID], &progress))
	sweep := daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), nil), "UTC")
	now := progress.Anchor.Add(2 * time.Hour)
	require.NoError(t, sweep.RunOnce(t.Context(), now))
	children, e := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, e)
	require.Len(t, children, 2)
	var child db.Issue
	for _, issue := range children {
		if issue.ID != parent.ID {
			child = issue
		}
	}
	inboxRefs := func() []string {
		t.Helper()
		var inbox struct {
			Requests []struct {
				Ref string `json:"ref"`
			} `json:"requests"`
		}
		require.NoError(t, json.Unmarshal(execute([]string{"inbox", "--for", "operator/consumer"}, ""), &inbox))
		refs := make([]string, 0, len(inbox.Requests))
		for _, request := range inbox.Requests {
			refs = append(refs, request.Ref)
		}
		return refs
	}
	require.Equal(t, []string{child.ShortID}, inboxRefs())
	execute([]string{"notify", child.ShortID, "--to", "operator/consumer", "--clear"}, "")
	require.NoError(t, sweep.RunOnce(t.Context(), now))
	require.Empty(t, inboxRefs())
	require.NoError(t, sweep.RunOnce(t.Context(), now.Add(2*time.Hour)))
	children, e = env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, e)
	require.Len(t, children, 3)
	nextRefs := inboxRefs()
	require.Len(t, nextRefs, 1)
	require.NotEqual(t, child.ShortID, nextRefs[0])
	parent, e = env.DB.IssueByID(t.Context(), parent.ID)
	require.NoError(t, e)
	require.Equal(t, "open", parent.Status)
}

// A compact, attributed beat admitted by HTTP must also work through the CLI.
func TestCronRuntimeCLIAdmittedBodyLimit(t *testing.T) {
	for _, char := range []string{"a", "&", "<", ">"} {
		for _, choices := range []bool{false, true} {
			t.Run(fmt.Sprintf("actor_%x/choices_%t", char, choices), func(t *testing.T) {
				env := testenv.New(t)
				project, err := env.DB.CreateProject(t.Context(), "spoke-project")
				require.NoError(t, err)
				fields := map[string]any{"actor": "", "label": "worker", "adapter": "example"}
				if choices {
					fields["choices"] = map[string][]string{"options.agent": {"review"}}
				}
				empty, err := json.Marshal(fields)
				require.NoError(t, err)
				actor := strings.Repeat(char, cron.RuntimeBeatLimit-len(empty))
				fields["actor"] = actor
				body, err := json.Marshal(fields)
				require.NoError(t, err)
				require.Len(t, body, cron.RuntimeBeatLimit)
				id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/runtimes/%s", env.URL, project.ID, id), bytes.NewReader(body))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				response, err := env.HTTP.Do(request)
				require.NoError(t, err)
				raw, err := io.ReadAll(response.Body)
				require.NoError(t, response.Body.Close())
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				out, diagnostics, err := executeCronCLI(t, env.URL, bytes.NewReader(body), "runtime", "beat", id, "--json-input", "-")
				require.NoError(t, err, diagnostics)
				var beat struct {
					Runtime cron.Runtime `json:"runtime"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &beat))
				require.Equal(t, actor, beat.Runtime.Actor)
				require.Equal(t, "live", beat.Runtime.Status)
				require.Equal(t, "active", beat.Runtime.State)
				require.Equal(t, 60, beat.Runtime.IntervalSeconds)
				if choices {
					require.Equal(t, []string{"review"}, beat.Runtime.Choices["options.agent"])
				}
			})
		}
	}
}

// Execute the displayed heredoc body and command, including its retained sample UID.
func TestCronRuntimeHelpExampleExecutes(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	help := string(executeRoot(t, newRootCmd(), "cron", "runtime", "beat", "--help"))
	_, examples, ok := strings.Cut(help, "\nExamples:\n")
	require.True(t, ok, "rendered help must include examples")
	lines := strings.Split(examples, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(line), "kata cron runtime beat ")
	})
	require.NotEqual(t, -1, start, "rendered help must include the beat command")
	lines = lines[start:]
	args := strings.Fields(lines[0])
	last := args[len(args)-1]
	require.True(t, strings.HasPrefix(last, "<<'"))
	delimiter := strings.TrimSuffix(strings.TrimPrefix(last, "<<'"), "'")
	end := 1
	for end < len(lines) && strings.TrimSpace(lines[end]) != delimiter {
		end++
	}
	require.Less(t, end, len(lines), "unterminated heredoc in runtime help")
	body := strings.Join(lines[1:end], "\n")
	var expected map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(body), &expected))
	expected["actor"] = jsontext.Value(`"operator"`)
	expectedRaw, err := json.Marshal(expected)
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	// Verify every displayed field survives the complete CLI request path.
	proxy := &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) {
		request.SetURL(target)
	}}
	wire := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/cron/runtimes/") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			wire <- raw
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	out, _, err := executeCronCLI(t, server.URL, strings.NewReader(body), args[2:len(args)-1]...)
	require.NoError(t, err)
	require.Contains(t, out, `"status":"live"`)
	select {
	case raw := <-wire:
		require.JSONEq(t, string(expectedRaw), string(raw))
	default:
		t.Fatal("help example did not send a runtime beat")
	}
	out, _, err = executeCronCLI(t, env.URL, nil, "runtime", "list")
	require.NoError(t, err)
	require.Contains(t, out, `"actor":"operator"`)
	require.Contains(t, out, "example-worker")
}
