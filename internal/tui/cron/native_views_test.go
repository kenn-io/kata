package cron

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	native "go.kenn.io/kata/internal/cron"
	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func nativeFixture() Data {
	return Data{Projects: map[string]Data{"7": {Project: "example-project"}}, Jobs: []records.Job{{ProjectUID: "7", Project: "example-project", UID: "job-1", Name: "Daily summary", DefinitionEventUID: "guard", Definition: jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 7 * * *","timezone":"UTC"},"action":{"kind":"execute","workflow_uid":"flow-1"},"overlap":"forbid","catchup":"latest"}`)}}, Workflows: []records.Workflow{{ProjectUID: "7", Project: "example-project", UID: "flow-1", Name: "Summary flow", Definition: jsontext.Value(`{"version":1,"steps":[{"key":"read","kind":"command","command":"echo example"},{"key":"summarize","kind":"prompt","prompt":"Summarize the results","after":["read"],"retries":2}]}`)}}, Runs: []records.Run{{ProjectKey: "7", UID: "run-1", JobUID: new("job-1"), WorkflowUID: new("flow-1"), Status: "succeeded", Revision: 1, CreatedAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC), Summary: summary("Recorded summary")}}}
}

func TestCronMissingReferences(t *testing.T) {
	d := nativeFixture()
	d.Jobs = nil
	d.Workflows = nil
	m := newModel(Options{})
	m.SetData(d)
	m.SelectTab(1)
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "run run-1") {
		t.Fatal(text)
	}
	m.pushDetail(viewRun, "7", "run-1")
	text := ansi.Strip(m.View().Content)
	if !strings.Contains(text, "Recorded summary") || !strings.Contains(text, "job-1") {
		t.Fatal(text)
	}
	if strings.Contains(text, "steps") || strings.Contains(text, "model") {
		t.Fatal("invented runtime evidence", text)
	}
}

func TestCronNativeStatusAndRecordedStart(t *testing.T) {
	d := nativeFixture()
	r := d.Runs[0]
	r.Status = "cancelled"
	v := runView(r, d)
	if !v.When.IsZero() {
		t.Fatal("creation time displayed as execution start")
	}
	started := r.CreatedAt.Add(time.Minute)
	r.StartedAt = &started
	if got := runView(r, d).When; got != started {
		t.Fatalf("recorded start %v, want %v", got, started)
	}
	label, _ := runLabel(r)
	if label != "cancelled" {
		t.Fatal(label)
	}
}

func TestCronRunTriggerIsUnrecorded(t *testing.T) {
	for _, occurrence := range []*string{nil, new(""), new("occurrence-1")} {
		r := records.Run{UID: "run-1", OccurrenceKey: occurrence}
		if got := runView(r, Data{}).Trigger; got != "—" {
			t.Fatalf("optional occurrence key inferred trigger %q", got)
		}
	}
}

func TestCronJobHistoryDoesNotInferTrigger(t *testing.T) {
	for _, occurrence := range []*string{nil, new("occurrence-1")} {
		d := nativeFixture()
		d.Runs[0].OccurrenceKey = occurrence
		m := newModel(Options{})
		m.Resize(160, 80)
		m.SetData(d)
		m.pushDetail(viewJob, "7", "job-1")
		text := ansi.Strip(m.View().Content)
		history := strings.SplitN(text, "Runs · loaded history", 2)
		if len(history) != 2 || !strings.Contains(history[1], "Recorded summary") {
			t.Fatal("history observation missing", text)
		}
		if strings.Contains(history[1], "manual") || strings.Contains(history[1], "scheduled") {
			t.Fatal("history invented a trigger", history[1])
		}
	}
}

func summary(message string) native.Summary { return native.Summary{Version: 1, Message: message} }

func TestCronSourceMarqueeScheduling(t *testing.T) {
	m := newModel(Options{})
	d := nativeFixture()
	d.Jobs[0].Name = strings.Repeat("Long job title ", 20)
	m.SetData(d)
	_, cmd := m.Update(struct{}{})
	if cmd == nil || !m.marqueePending {
		t.Fatal("visible overflow did not schedule source marquee")
	}
	generation := m.marqueeGeneration
	before := m.frame
	m.Update(marqueeTickMsg{generation: generation + 1})
	if m.frame != before {
		t.Fatal("late animation advanced frame")
	}
	m.Update(marqueeTickMsg{generation: generation})
	if m.frame != before+1 {
		t.Fatal("source marquee did not advance")
	}
}

func TestCronTabsRestoreDetailFrames(t *testing.T) {
	m := newModel(Options{})
	m.SetData(nativeFixture())
	m.pushDetail(viewJob, "7", "job-1")
	m.scroll = 4
	m.SelectTab(1)
	if m.currentView() != viewList || m.tab != 1 {
		t.Fatal("Jobs detail trapped the Runs tab", m.tab, m.currentView())
	}
	m.SelectTab(0)
	if m.currentView() != viewJob || m.scroll != 4 {
		t.Fatal("tab switch lost detail state", m.currentView(), m.scroll)
	}
}

func TestCronHiddenViewsStopMarquee(t *testing.T) {
	m := newModel(Options{})
	d := nativeFixture()
	d.Jobs[0].Name = strings.Repeat("Long title ", 30)
	m.SetData(d)
	m.Update(struct{}{})
	generation := m.marqueeGeneration
	m.SetActive(false)
	m.Update(marqueeTickMsg{generation: generation})
	if m.marqueePending {
		t.Fatal("hidden cron view kept animation timer")
	}
}

func TestCronJobIssueLinkBeforeRun(t *testing.T) {
	d := nativeFixture()
	d.Runs = nil
	d.Jobs[0].Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summary"},"issue":{"kind":"existing","uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"},"overlap":"allow","catchup":"skip"}`)
	m := newModel(Options{})
	m.SetData(d)
	cmd := m.routeKey(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if cmd == nil {
		t.Fatal("job issue link disappeared without observations")
	}
	msg, ok := cmd().(IssueMsg)
	if !ok || msg.ProjectKey != "7" || msg.UID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatal(msg)
	}
}

func TestCronSchedulesIncludeTimezone(t *testing.T) {
	for _, kind := range []string{"cron", "interval", "once"} {
		trigger := defs.Trigger{Kind: kind, Cron: "0 7 * * *", IntervalSeconds: 3600, At: "2026-01-01T07:00:00Z", Timezone: "America/New_York"}
		if text := jobSchedule(trigger); !strings.Contains(text, "America/New_York") {
			t.Fatal("summary omitted configured timezone", kind, text)
		}
	}
}

func TestCronUnloadedHistoryDoesNotMeanNever(t *testing.T) {
	m := newModel(Options{})
	d := nativeFixture()
	d.Runs = nil
	m.SetData(d)
	text := ansi.Strip(m.View().Content)
	if strings.Contains(text, "never") {
		t.Fatal("unloaded observations claimed job never ran", text)
	}
	if !strings.Contains(text, "unloaded") {
		t.Fatal("missing explicit unloaded status", text)
	}
}

func TestCronWorkflowHistoryUsesRecordedCreation(t *testing.T) {
	d := nativeFixture()
	d.Runs = nil
	for i := range 21 {
		d.Runs = append(d.Runs, records.Run{UID: fmt.Sprintf("run-%02d", 20-i), WorkflowUID: new("flow-1"), ProjectKey: "7", CreatedAt: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)})
	}
	m := newModel(Options{})
	m.SetData(d)
	v := m.wfIndex[workflowKey{"7", "flow-1"}]
	if got := m.data.Runs[v.runs[0]].UID; got != "run-00" {
		t.Fatal("workflow latest observation ignored creation time", got)
	}
}

func TestCronRunDetailIncludesRecordedRevision(t *testing.T) {
	d := nativeFixture()
	d.Runs[0].Revision = 42
	m := newModel(Options{})
	m.SetData(d)
	m.pushDetail(viewRun, "7", "run-1")
	var lines []string
	for _, line := range m.renderRunDetail() {
		lines = append(lines, ansi.Strip(line.Text))
	}
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "revision") || !strings.Contains(text, "42") {
		t.Fatal("recorded revision missing", text)
	}
}

func TestCronModalMouseDoesNotNavigateUnderlyingRows(t *testing.T) {
	for _, modal := range []string{"confirm", "conflict", "form", "project"} {
		t.Run(modal, func(t *testing.T) {
			d := nativeFixture()
			j := d.Jobs[0]
			j.UID = "job-2"
			d.Jobs = append(d.Jobs, j)
			m := newModel(Options{Mouse: true})
			m.SetData(d)
			switch modal {
			case "confirm":
				m.confirm = d.Jobs
			case "conflict":
				m.conflict = &ConflictError{}
			case "form":
				m.editor = newJobForm(t.Context(), nil, nil, nil, editorContext{})
			case "project":
				m.projectChoices = []string{"7"}
			}
			m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
			if m.cursor != 0 || m.currentView() != viewList {
				t.Fatal("modal wheel moved underlying selection", m.cursor, m.currentView())
			}
		})
	}
}
