package tui

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/tui/cron/records"
	"go.kenn.io/kata/internal/uid"
)

// The smoke fixture is an isolated daemon seeded only through existing APIs.
func TestCronNativeDaemonSmoke(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := context.Background()
	p, err := c.EnsureProject(ctx, "example-project", "example-actor")
	if err != nil {
		t.Fatal(err)
	}
	flowID, err := uid.New()
	if err != nil {
		t.Fatal(err)
	}
	flow, err := c.SaveCronWorkflow(ctx, p.ID, "example-actor", records.Workflow{UID: flowID, Name: "Summary workflow", Definition: jsontext.Value(`{"version":1,"steps":[{"key":"summarize","kind":"prompt","prompt":"Summarize the report"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := uid.New()
	if err != nil {
		t.Fatal(err)
	}
	definition := jsontext.Value(fmt.Sprintf(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600,"timezone":"UTC"},"action":{"kind":"execute","workflow_uid":%q},"issue":{"kind":"per-run","title":"Report"},"overlap":"forbid","catchup":"latest","options":{"opaque":9007199254740993}}`, flow.UID))
	job, err := c.SaveCronJob(ctx, p.ID, "example-actor", records.Job{UID: jobID, Name: "Hourly report", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := uid.New()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"actor":"example-actor","job_uid":%q,"definition_event_uid":%q,"workflow_uid":%q,"workflow_definition_event_uid":%q,"executor_label":"example-executor","status":"succeeded","occurrence_key":"2026-01-01T07:00:00Z","started_at":"2026-01-01T07:00:00Z","ended_at":"2026-01-01T07:01:00Z","summary":{"version":1,"message":"Report recorded","input_tokens":100,"output_tokens":20},"expected_revision":0}`, job.UID, job.DefinitionEventUID, flow.UID, flow.DefinitionEventUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/runs/%s", env.URL, p.ID, runID), bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status, string(raw))
	}
	m := initialModel(Options{})
	m.api = newUndoClient(c)
	m.scope = scope{projectID: p.ID, projectName: p.Name}
	m.list.actor = "example-actor"
	h := newCronHost(m)
	h.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	cmd := h.selectCronTab(1)
	drainCronSmoke(t, h, cmd)
	if text := ansi.Strip(h.View().Content); !strings.Contains(text, "Hourly report") {
		t.Fatal(text)
	}
	// Editing is native and uses the original event guard, including pause.
	svc := cronProjectClient{client: c, projectID: p.ID, project: p.Name, actor: "example-actor"}
	paused, err := svc.Pause(job)
	if err != nil {
		t.Fatal(err)
	}
	if paused.DefinitionEventUID == job.DefinitionEventUID {
		t.Fatal("pause did not advance definition guard")
	}
	if _, err := svc.Pause(job); records.Status(err) != 409 {
		t.Fatal("stale guard was accepted", err)
	}
	drainCronSmoke(t, h, h.selectCronTab(2))
	if text := ansi.Strip(h.View().Content); !strings.Contains(text, "Report recorded") {
		t.Fatal(text)
	}
	drainCronSmoke(t, h, h.selectCronTab(3))
	if text := ansi.Strip(h.View().Content); !strings.Contains(text, "Summary workflow") {
		t.Fatal(text)
	}
}
func drainCronSmoke(t *testing.T, h *cronHost, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, cmd := range batch {
			drainCronSmoke(t, h, cmd)
		}
		return
	}
	// Animation is verified separately; no wall-clock waits in this API smoke.
	if _, ok := msg.(cronChildMsg); ok {
		return
	}
	_, next := h.Update(msg)
	drainCronSmoke(t, h, next)
}

func TestCronNativeReadbackMatchesOptionalDefaults(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := t.Context()
	p, err := c.EnsureProject(ctx, "example-project", "example-actor")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			id, err := uid.New()
			if err != nil {
				t.Fatal(err)
			}
			pending := records.Job{UID: id, Name: "Example"}
			var saved records.Job
			if kind == "job" {
				pending.Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual","lead_seconds":0},"action":{"kind":"execute","prompt":"Summarize"},"issue":{"kind":"per-run","title":"Report","body":""},"overlap":"allow","catchup":"skip","grace_seconds":0,"options":{"opaque":9007199254740993}}`)
				saved, err = c.SaveCronJob(ctx, p.ID, "example-actor", pending)
			} else {
				pending.Definition = jsontext.Value(`{"version":1,"about":"","input":"","steps":[{"key":"summarize","kind":"prompt","prompt":"Summarize","after":[],"retries":0,"options":{"opaque":9007199254740993}}]}`)
				saved, err = c.SaveCronWorkflow(ctx, p.ID, "example-actor", pending)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !records.SameDefinition(pending, saved) {
				t.Fatalf("native canonical readback did not recognize committed payload:\npending %s\nstored %s", pending.Definition, saved.Definition)
			}
		})
	}
}

// The daemon's last_run is authoritative for a job whose only run is outside
// the first history page and for a job that never ran.
func TestCronDaemonLastRunBeyondLoadedHistory(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := context.Background()
	p, err := c.EnsureProject(ctx, "example-project", "example-actor")
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]records.Job{}
	for _, name := range []string{"older-job", "busy-job", "unrun-job"} {
		id, err := uid.New()
		if err != nil {
			t.Fatal(err)
		}
		definition := jsontext.Value(`{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600,"timezone":"UTC"},"action":{"kind":"execute","prompt":"Summarize the report"},"issue":{"kind":"per-run","title":"Report"},"overlap":"forbid","catchup":"latest"}`)
		if jobs[name], err = c.SaveCronJob(ctx, p.ID, "example-actor", records.Job{UID: id, Name: name, Definition: definition}); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(job records.Job, status string) string {
		t.Helper()
		id, err := uid.New()
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"actor":"example-actor","job_uid":%q,"definition_event_uid":%q,"status":%q,"summary":{"version":1,"message":"%s result"},"expected_revision":0}`, job.UID, job.DefinitionEventUID, status, job.Name)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/runs/%s", env.URL, p.ID, id), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := env.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status, string(raw))
		}
		return id
	}
	olderRun := observe(jobs["older-job"], "failed")
	for range 100 {
		observe(jobs["busy-job"], "succeeded")
	}
	page, err := c.ListCronRuns(ctx, p.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Runs {
		if r.UID == olderRun {
			t.Fatal("the older job's run is inside the first history page")
		}
	}

	listed, err := c.ListCronJobs(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]records.Job{}
	for _, j := range listed {
		byName[j.Name] = j
	}
	if j := byName["older-job"]; !j.LastRunKnown || j.LastRun == nil || j.LastRun.UID != olderRun || j.LastRun.Status != "failed" || j.LastRun.ProjectKey != fmt.Sprint(p.ID) {
		t.Fatalf("job outside the history page lacks its last run: %+v", j.LastRun)
	}
	if j := byName["unrun-job"]; !j.LastRunKnown || j.LastRun != nil {
		t.Fatalf("unrun job is not never-ran: known=%t last=%+v", j.LastRunKnown, j.LastRun)
	}

	m := initialModel(Options{})
	m.api = newUndoClient(c)
	m.scope = scope{projectID: p.ID, projectName: p.Name}
	h := newCronHost(m)
	h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drainCronSmoke(t, h, h.selectCronTab(1))
	want := map[string]string{"older-job": "failed", "busy-job": "succeeded", "unrun-job": "never ran"}
	text := ansi.Strip(h.View().Content)
	for name, status := range want {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, name) && strings.Contains(line, status) && !strings.Contains(line, "unloaded") {
				found = true
			}
		}
		if !found {
			t.Fatalf("jobs tab lacks %s %s:\n%s", status, name, text)
		}
	}
}
