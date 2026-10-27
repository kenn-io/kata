package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// The fixture contains current definitions and an observation of an older
// revision, which must remain distinguishable when following its links.
func cronFixture(t *testing.T) Model {
	t.Helper()
	m := newTestModel()
	m.width, m.height = 80, 24
	m.scope.projectName = "example-project"
	p := cronData{}
	clock := generated.HLC{
		Version: 1, PhysicalMs: snapshotFixedNow.UnixMilli(),
		OriginInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FB4",
	}
	p.jobs = []generated.CronJob{{
		UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "Daily summary", ProjectID: 7,
		ID: 1, Revision: 2, Author: "worker", CreatedAt: snapshotFixedNow, UpdatedAt: snapshotFixedNow,
		DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB2", DefinitionHlc: clock,
		Definition: generated.CronJobDefinitionResponse{
			Version: 1, Kind: "job", Enabled: true,
			Trigger: generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")},
			Action:  generated.ActionResponse{Kind: "execute", WorkflowUID: new("01ARZ3NDEKTSV4RRFFQ69G5FAW")},
			Issue:   &generated.IssuePolicyResponse{Kind: "per-run", Title: new("Summary result")},
			Overlap: "forbid", Catchup: "skip",
		},
	}}
	p.workflows = []generated.CronWorkflow{{
		UID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Name: "Summary flow", ProjectID: 7,
		ID: 2, Revision: 1, Author: "worker", CreatedAt: snapshotFixedNow, UpdatedAt: snapshotFixedNow,
		DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB3", DefinitionHlc: clock,
		Definition: generated.CronWorkflowDefinitionResponse{
			Version: 1, About: new("A daily report"), Input: new("Changes since yesterday"),
			Steps: []generated.CronWorkflowStepResponse{{
				Key: "collect", Kind: "command", Command: new("echo first\necho second"), Retries: new(int64(2)),
				Options: map[string]any{"shell": "sh"},
			}},
		},
	}}
	p.runs = []generated.CronRun{{
		UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", ProjectID: 7, JobUID: new("01ARZ3NDEKTSV4RRFFQ69G5FAV"), WorkflowUID: new("01ARZ3NDEKTSV4RRFFQ69G5FAW"),
		DefinitionEventUID: new("01ARZ3NDEKTSV4RRFFQ69G5FB1"), WorkflowDefinitionEventUID: new("01ARZ3NDEKTSV4RRFFQ69G5FB3"),
		Status: "succeeded", Actor: "worker", ExecutorLabel: new("executor-a"),
		ID: 1, Revision: 1, UpdatedAt: snapshotFixedNow.Add(-time.Hour),
		CreatedAt: snapshotFixedNow.Add(-time.Hour), StartedAt: new(snapshotFixedNow.Add(-2 * time.Hour)), EndedAt: new(snapshotFixedNow.Add(-time.Hour)),
		Summary: generated.SummaryResponse{Version: 1, Message: new("Report ready"), InputTokens: new(int64(30)), OutputTokens: new(int64(10))},
	}}
	m = sendRune(m, '2')
	p.conn, p.scope = m.connGen, m.scopeGen
	m, _ = updateModel(m, cronLoadedMsg(p))
	return m
}

func TestCronLinkFollows(t *testing.T) {
	for _, tc := range []struct {
		name string
		tab  rune
		kind int
		uid  string
	}{
		{"job to run", '2', 2, "01ARZ3NDEKTSV4RRFFQ69G5FAY"},
		{"run to job", '3', 1, "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{"workflow to job", '4', 1, "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{"workflow to run", '4', 2, "01ARZ3NDEKTSV4RRFFQ69G5FAY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sendRune(cronFixture(t), tc.tab)
			m = sendKey(m, tea.KeyEnter)
			_, _, lines := cronScreen(m)
			index := slices.IndexFunc(lines, func(line cronLine) bool { return line.target.kind == tc.kind && line.target.uid == tc.uid })
			require.GreaterOrEqual(t, index, 0)
			if tc.name == "run to job" {
				require.Contains(t, lines[index].text, "current definition changed")
			}
			for range index {
				m = sendRune(m, 'j')
			}
			m = sendKey(m, tea.KeyEnter)
			require.Len(t, m.cron.stack, 2)
			require.Equal(t, tc.uid, m.cron.stack[1].ref.uid)
			require.Equal(t, tc.kind, m.cron.stack[1].ref.kind)
			if tc.name == "run to job" {
				m = sendRune(m, 'h')
				require.Len(t, m.cron.stack, 1)
			}
		})
	}
}

func TestCronFreshness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event tea.Msg
		away  bool
	}{
		{"coalesce with detail open", keyRune('r'), false},
		{"reset", resetRequiredMsg{}, false},
		{"event while away", eventReceivedMsg{eventType: "cron.job.updated", projectID: 7}, true},
		{"project merged", eventReceivedMsg{eventType: "project.merged", projectID: 7}, false},
		{"project merged while away", eventReceivedMsg{eventType: "project.merged", projectID: 7}, true},
		{"reset while away", resetRequiredMsg{}, true},
		{"disappearing record", keyRune('r'), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronFixture(t)
			m.sseCh = nil
			a := &cronReadTestAPI{KataAPI: &inboxTestAPI{}, data: *m.cron.data}
			a.data.jobs = slices.Clone(a.data.jobs)
			a.data.jobs[0].Name = "New title"
			a.data.workflows = slices.Clone(a.data.workflows)
			a.data.workflows[0].Name = "New workflow"
			a.data.runs = slices.Clone(a.data.runs)
			a.data.runs[0].Summary.Message = new("New run result")
			m.api = a
			detail := tc.name == "coalesce with detail open" || tc.name == "disappearing record"
			if detail {
				m = sendKey(m, tea.KeyEnter)
			}
			if tc.name == "disappearing record" {
				a.data.jobs = nil
			}
			if tc.away {
				m = sendRune(m, '1')
			}
			m, cmd := updateModel(m, tc.event)
			if tc.away {
				runCmdTree(cmd)
				require.Zero(t, a.calls)
				m, cmd = updateModel(m, keyRune('2'))
			}
			require.NotNil(t, cmd)
			if tc.name == "coalesce with detail open" {
				for range 10 {
					m, _ = updateModel(m, eventReceivedMsg{eventType: "cron.job.updated", projectID: 7})
				}
				require.Contains(t, stripANSI(m.viewBody()), "Daily summary")
				require.NotContains(t, stripANSI(m.viewBody()), "no longer in the loaded data")
				first := cmd()
				require.Equal(t, 1, a.calls)
				a.data.jobs[0].Name = "Newest title"
				m, cmd = updateModel(m, first)
				require.Contains(t, stripANSI(m.viewBody()), "New title")
				require.NotContains(t, stripANSI(m.viewBody()), "Newest title")
				require.NotNil(t, cmd, "events during the read request one replacement read")
				m, cmd = updateModel(m, cmd())
				require.Nil(t, cmd)
				require.Equal(t, 2, a.calls)
				require.Contains(t, stripANSI(m.viewBody()), "Newest title")
				return
			}
			// Update appends the cron read after the existing issue/SSE commands.
			msg := cmd()
			if batch, ok := msg.(tea.BatchMsg); ok {
				msg = batch[len(batch)-1]()
			}
			m, _ = updateModel(m, msg)
			require.Equal(t, 1, a.calls)
			if tc.name == "disappearing record" {
				require.Contains(t, stripANSI(m.viewBody()), "this job is no longer in the loaded data")
			} else {
				require.Contains(t, stripANSI(m.viewBody()), "New title")
			}
			if strings.HasPrefix(tc.name, "project merged") {
				require.Contains(t, stripANSI(sendRune(m, '4').viewBody()), "New workflow")
				require.Contains(t, stripANSI(sendRune(m, '3').viewBody()), "New run result")
			}
		})
	}
}

func TestCronScopeAndDaemonFence(t *testing.T) {
	for _, connection := range []bool{false, true} {
		m := cronFixture(t)
		stale := cronLoadedMsg(*m.cron.data)
		m = sendKey(m, tea.KeyEnter)
		if connection {
			m.connGen++
		} else {
			m.scopeGen++
			m.scope.projectID = 9
			m.scope.projectName = "other-project"
		}
		m, _ = updateModel(m, tea.FocusMsg{})
		require.Empty(t, m.cron.stack)
		m, _ = updateModel(m, stale)
		require.NotContains(t, stripANSI(m.viewBody()), "Daily summary")
	}
}

func TestCronInputAndGlobals(t *testing.T) {
	for _, detail := range []bool{false, true} {
		m := cronFixture(t)
		if detail {
			m = sendKey(m, tea.KeyEnter)
		}
		h := sendRune(m, '?')
		require.Equal(t, viewCron, h.view)
		require.True(t, h.cron.help)
		if detail {
			require.Contains(t, stripANSI(h.viewBody()), "follow")
		}
		require.Equal(t, h.cron.tab, sendRune(h, '3').cron.tab)
		require.False(t, sendRune(h, '?').cron.help)
		require.Equal(t, viewProjects, sendRune(m, 'P').view)
		q := sendRune(m, 'q')
		require.Equal(t, modalQuitConfirm, q.modal)
	}
}

func TestCronDigitsGoToActiveInput(t *testing.T) {
	for _, name := range []string{"cron filter", "list search", "priority prompt", "federation adopt prompt", "main help view"} {
		t.Run(name, func(t *testing.T) {
			m := cronFixture(t)
			digit := '2'
			switch name {
			case "cron filter":
				m = sendRune(m, '/')
				digit = '3'
			case "list search":
				m = sendRune(m, '1')
				var cmd tea.Cmd
				m, cmd = updateModel(m, keyRune('/'))
				require.NotNil(t, cmd)
				m, _ = updateModel(m, cmd())
			case "priority prompt":
				m = sendRune(m, '1')
				issue := testIssue("abc4")
				m.detail.issue, m.view = &issue, viewDetail
				var cmd tea.Cmd
				m, cmd = updateModel(m, keyRune('!'))
				require.NotNil(t, cmd)
				m, _ = updateModel(m, cmd())
				require.Equal(t, inputPriorityPrompt, m.input.kind)
			case "federation adopt prompt":
				m.view = viewFederation
				m.federation.mode = federationModeAdoptConfirm
			case "main help view":
				m = sendRune(m, '1')
				m = sendRune(m, '?')
				digit = '3'
			}
			want := m.view
			m = sendRune(m, digit)
			require.Equal(t, want, m.view)
			switch name {
			case "cron filter":
				require.Equal(t, 1, m.cron.tab)
				require.Equal(t, "3", m.cron.lists[1].query)
			case "list search":
				require.Contains(t, m.list.filter.Search, "2")
			case "priority prompt":
				require.Equal(t, "2", m.input.fields[0].value())
			case "federation adopt prompt":
				require.Equal(t, "2", m.federation.adoptConfirmInput)
			case "main help view":
				require.Equal(t, viewHelp, m.view)
			}
		})
	}
}

func TestCronDocumentContentAndCursor(t *testing.T) {
	m := cronFixture(t)
	p := m.cron.data
	p.jobs[0].Definition.Action = generated.ActionResponse{Kind: "notify", Recipient: new("reader"), Message: new("Weekly digest\nSecond line")}
	m = sendKey(m, tea.KeyEnter)
	_, _, lines := cronScreen(m)
	document := ""
	for _, l := range lines {
		document += l.text + "\n"
	}
	require.Contains(t, document, `"recipient": "reader"`)
	require.Contains(t, document, "Weekly digest\nSecond line")
	require.Contains(t, document, "Loaded runs (1) · newest 100")
	p.jobs[0].Definition.Action = generated.ActionResponse{
		Kind: "execute", Prompt: new("Summarize all changes\nInclude follow-up tasks."),
	}
	_, _, lines = cronScreen(m)
	document = ""
	for _, line := range lines {
		document += line.text + "\n"
	}
	require.Contains(t, document, "Summarize all changes\nInclude follow-up tasks.")
	m = sendRune(m, '4')
	m = sendKey(m, tea.KeyEnter)
	// Thirty steps with a single link must still let k reach step one.
	for i := range 30 {
		p.workflows[0].Definition.Steps = append(p.workflows[0].Definition.Steps, generated.CronWorkflowStepResponse{Key: fmt.Sprintf("step-%d", i), Kind: "command", Command: new(strings.Repeat("long command ", 12))})
	}
	m = sendRune(m, 'G')
	m.width = 120
	for range 500 {
		m = sendRune(m, 'k')
	}
	require.Equal(t, 0, m.cron.stack[0].cursor)
	require.Contains(t, stripANSI(m.viewBody()), "collect")
	// A refresh can shrink either list or detail; movement starts clamped.
	m = sendRune(m, '2')
	for i := range 2 {
		job := p.jobs[0]
		job.UID = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5FA%d", i)
		job.ID = int64(i + 2)
		job.DefinitionEventUID = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5FB%d", i)
		p.jobs = append(p.jobs, job)
	}
	m.cron.lists[1].cursor = 1000
	m = sendRune(m, 'k')
	require.Equal(t, len(p.jobs)-2, m.cron.lists[1].cursor)
	m = sendKey(m, tea.KeyEnter)
	m.cron.stack[0].cursor = 1000
	_, _, lines = cronScreen(m)
	m = sendRune(m, 'k')
	require.Equal(t, len(lines)-2, m.cron.stack[0].cursor)
}

// Only the read boundary is doubled; assertions exercise Update and rendering.
type cronReadTestAPI struct {
	KataAPI
	data  cronData
	calls int
}

func (a *cronReadTestAPI) ListCronJobs(_ context.Context, id int64) ([]generated.CronJob, error) {
	if id != 7 {
		return nil, fmt.Errorf("unexpected project %d", id)
	}
	a.calls++
	return slices.Clone(a.data.jobs), a.data.jobsErr
}
func (a *cronReadTestAPI) ListCronWorkflows(_ context.Context, id int64) ([]generated.CronWorkflow, error) {
	if id != 7 {
		return nil, fmt.Errorf("unexpected project %d", id)
	}
	return slices.Clone(a.data.workflows), a.data.workflowsErr
}
func (a *cronReadTestAPI) ListCronRuns(_ context.Context, id int64) ([]generated.CronRun, error) {
	if id != 7 {
		return nil, fmt.Errorf("unexpected project %d", id)
	}
	return slices.Clone(a.data.runs), a.data.runsErr
}

func TestCronReturnToKata(t *testing.T) {
	for _, tc := range []struct {
		name         string
		detail       bool
		width        int
		coveredWidth int
		exit         rune
	}{
		{"list", false, 80, 80, 0},
		{"single-pane detail", true, 80, 80, 0},
		{"split list focus", false, 200, 200, 0},
		{"split detail focus", true, 200, 200, 0},
		{"narrowed split", true, 200, 80, 0},
		{"federation exit", true, 80, 80, 'F'},
		{"daemon exit", true, 80, 80, 'C'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sendRune(cronFixture(t), '1')
			issue := testIssue("abc4")
			m.detail.issue, m.view = &issue, viewDetail
			m.focus = focusDetail
			m, _ = updateModel(m, tea.WindowSizeMsg{Width: tc.width, Height: 40})
			if !tc.detail {
				m, _ = updateModel(m, popDetailMsg{})
			}
			want := m.view
			m = sendRune(m, '2')
			m, _ = updateModel(m, tea.WindowSizeMsg{Width: tc.coveredWidth, Height: 24})
			if tc.exit != 0 {
				m = sendRune(m, tc.exit)
				m = sendKey(m, tea.KeyEsc)
			}
			m = sendRune(m, '1')
			require.Equal(t, want, m.view)
			require.Equal(t, map[bool]focusPane{false: focusList, true: focusDetail}[tc.detail], m.focus)
			m = sendRune(m, 'c')
			require.Equal(t, map[bool]inputKind{false: inputNone, true: inputCommentForm}[tc.detail], m.input.kind)
		})
	}
}

func TestCronAllProjects(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 80, 24
	m.scope.allProjects = true
	a := &cronReadTestAPI{}
	m.api = a
	m, cmd := updateModel(m, keyRune('2'))
	require.Nil(t, cmd)
	require.Equal(t, viewProjects, sendRune(m, 'P').view)
}

func TestCronIndependentFailuresAndRetainedRows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tab      rune
		jobsFail bool
	}{
		{"workflow jobs", '4', true},
		{"workflow runs", '4', false},
		{"job runs", '2', false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronFixture(t)
			a := &cronReadTestAPI{data: *m.cron.data}
			if tc.jobsFail {
				a.data.jobs = nil
				a.data.jobsErr = errors.New("jobs read failed")
			} else {
				a.data.runs = nil
				a.data.runsErr = errors.New("runs read failed")
			}
			m.api, m.cron.data = a, nil
			m, cmd := updateModel(m, keyRune('r'))
			m, _ = updateModel(m, cmd())
			m = sendRune(m, tc.tab)
			if tc.jobsFail {
				require.Contains(t, stripANSI(m.viewBody()), "unknown")
				require.NotContains(t, stripANSI(m.viewBody()), "0 jobs")
			}
			m = sendKey(m, tea.KeyEnter)
			_, _, lines := cronScreen(m)
			var document string
			for _, line := range lines {
				document += line.text + "\n"
			}
			if tc.jobsFail {
				require.Contains(t, document, "Used by · unavailable")
				require.NotContains(t, document, "Used by (0)")
			} else {
				require.Contains(t, document, "Loaded runs · unavailable")
				require.NotContains(t, document, "no loaded runs")
			}
		})
	}
	m := cronFixture(t)
	a := &cronReadTestAPI{data: *m.cron.data}
	a.data.jobs = nil
	a.data.jobsErr = errors.New(strings.Repeat("permission denied ", 20))
	m.api = a
	m.cron.data = nil
	m, cmd := updateModel(m, keyRune('r'))
	m, _ = updateModel(m, cmd())
	for line := range strings.SplitSeq(stripANSI(m.viewBody()), "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), 80)
	}
	m = sendRune(m, '4')
	require.Contains(t, stripANSI(m.viewBody()), "Summary flow")
	m = sendRune(m, '3')
	require.Contains(t, stripANSI(m.viewBody()), "Report ready")
	m = sendKey(m, tea.KeyEnter)
	_, _, lines := cronScreen(m)
	var document string
	for _, line := range lines {
		document += line.text + "\n"
	}
	require.Contains(t, document, "unavailable")
	require.NotContains(t, document, "no longer defined")
	m = cronFixture(t)
	a = &cronReadTestAPI{data: *m.cron.data}
	a.data.jobsErr = errors.New("refresh failed")
	a.data.jobs = nil
	m.api = a
	m, cmd = updateModel(m, keyRune('r'))
	m, _ = updateModel(m, cmd())
	require.Contains(t, stripANSI(m.viewBody()), "Daily")
	require.Contains(t, stripANSI(m.viewBody()), "refresh failed")
	m = sendRune(m, '4')
	require.Contains(t, stripANSI(m.viewBody()), "unknown")
	require.NotContains(t, stripANSI(m.viewBody()), "1 job")
}

func TestCronFilterAndTabState(t *testing.T) {
	for _, tab := range []rune{'2', '3', '4'} {
		t.Run(string(tab), func(t *testing.T) {
			m := cronFixture(t)
			m.cron.data.jobs[0].Name = strings.Repeat("long job name ", 12) + "job-suffix"
			m.cron.data.runs[0].Summary.Message = new(strings.Repeat("long summary ", 12) + "run-suffix")
			m.cron.data.workflows[0].Name = strings.Repeat("long workflow name ", 12) + "workflow-suffix"
			m = sendRune(m, tab)
			m = sendRune(m, '/')
			query := map[rune]string{'2': "job-suffix", '3': "run-suffix", '4': "workflow-suffix"}[tab]
			typed := m
			for _, r := range query {
				typed = sendRune(typed, r)
			}
			m, _ = updateModel(m, tea.PasteMsg{Content: query})
			require.Equal(t, stripANSI(typed.viewBody()), stripANSI(m.viewBody()))
			m = sendKey(m, tea.KeyEnter)
			_, _, lines := cronScreen(m)
			require.Len(t, lines, 1)
			m = sendKey(m, tea.KeyEnter)
			require.Len(t, m.cron.stack, 1)
		})
	}
	m := cronFixture(t)
	m = sendRune(m, '/')
	for _, r := range "daily" {
		m = sendRune(m, r)
	}
	m = sendKey(m, tea.KeyEnter)
	m = sendRune(m, '4')
	m = sendRune(m, '2')
	require.Contains(t, stripANSI(m.viewBody()), "/daily")
	m = sendKey(m, tea.KeyEsc)
	require.NotContains(t, stripANSI(m.viewBody()), "/daily")
	m = sendKey(m, tea.KeyEsc)
	require.Equal(t, viewList, m.view)
	m = sendRune(m, '2')
	m = sendRune(m, '/')
	_, cmd := updateModel(m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.IsType(t, tea.QuitMsg{}, cmd())
}

func TestSnapshot_Cron(t *testing.T) {
	defer snapshotInit(t)()
	for _, name := range []string{"jobs", "jobs-filter", "jobs-unavailable", "runs", "job-detail", "run-detail", "workflow-detail", "help", "all-projects"} {
		t.Run(name, func(t *testing.T) {
			m := cronFixture(t)
			switch name {
			case "jobs-filter":
				m = sendRune(m, '/')
				m, _ = updateModel(m, tea.PasteMsg{Content: "daily"})
			case "jobs-unavailable":
				m.cron.data.jobs = nil
				m.cron.data.jobsErr = errors.New("no cron permission (403)")
			case "runs":
				m = sendRune(m, '3')
			case "job-detail":
				m = sendKey(m, tea.KeyEnter)
			case "run-detail":
				m = sendRune(m, '3')
				m = sendKey(m, tea.KeyEnter)
			case "workflow-detail":
				m = sendRune(m, '4')
				m = sendKey(m, tea.KeyEnter)
			case "help":
				m = sendRune(m, '?')
			case "all-projects":
				m.scope.allProjects = true
			}
			assertGolden(t, "cron-"+name, m.viewBody())
		})
	}
}

func TestCronRefreshKeepsSelectedRecord(t *testing.T) {
	for _, tab := range []rune{'2', '3', '4'} {
		t.Run(string(tab), func(t *testing.T) {
			m := cronFixture(t)
			data := *m.cron.data
			job, run, workflow := data.jobs[0], data.runs[0], data.workflows[0]
			job.UID, run.UID, workflow.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAA", "01ARZ3NDEKTSV4RRFFQ69G5FAA", "01ARZ3NDEKTSV4RRFFQ69G5FAA"
			job.ID, run.ID, workflow.ID = 3, 2, 4
			job.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FBC"
			run.DefinitionEventUID = new("01ARZ3NDEKTSV4RRFFQ69G5FBD")
			workflow.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FBE"
			job.Name, workflow.Name = "Selected job", "Selected workflow"
			data.jobs = append(data.jobs, job)
			data.runs = append(data.runs, run)
			data.workflows = append(data.workflows, workflow)
			m, _ = updateModel(m, cronLoadedMsg(data))
			m = sendRune(m, tab)
			m = sendRune(m, 'j')
			job.UID, run.UID, workflow.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAB", "01ARZ3NDEKTSV4RRFFQ69G5FAB", "01ARZ3NDEKTSV4RRFFQ69G5FAB"
			job.ID, run.ID, workflow.ID = 5, 3, 6
			job.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FBF"
			run.DefinitionEventUID = new("01ARZ3NDEKTSV4RRFFQ69G5FBG")
			workflow.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FBH"
			data.jobs = append([]generated.CronJob{job}, data.jobs...)
			data.runs = append([]generated.CronRun{run}, data.runs...)
			data.workflows = append([]generated.CronWorkflow{workflow}, data.workflows...)
			m.api = &cronReadTestAPI{data: data}
			m, refresh := updateModel(m, keyRune('r'))
			require.NotNil(t, refresh)
			m, _ = updateModel(m, refresh())
			m = sendKey(m, tea.KeyEnter)
			require.Len(t, m.cron.stack, 1)
			require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAA", m.cron.stack[0].ref.uid)
		})
	}
}

func TestCronNoticesFitTerminal(t *testing.T) {
	for _, screen := range []string{"jobs", "detail", "all-projects"} {
		t.Run(screen, func(t *testing.T) {
			m := cronFixture(t)
			switch screen {
			case "detail":
				m = sendKey(m, tea.KeyEnter)
			case "all-projects":
				m.scope.allProjects = true
			}
			m.api = &inboxTestAPI{}
			m, lookup := updateModel(m, keyRune('I'))
			require.NotNil(t, lookup)
			m, _ = updateModel(m, lookup())
			rows := strings.Split(stripANSI(m.viewContent()), "\n")
			require.Contains(t, strings.Join(rows[:min(len(rows), m.height)], "\n"), "No Inbox project designated")
		})
	}
}

func TestCronPersistentSearch(t *testing.T) {
	for _, tab := range []rune{'2', '3', '4'} {
		for _, state := range []string{"idle", "empty", "loading", "unavailable"} {
			m := sendRune(cronFixture(t), tab)
			m.opts.Mouse = true
			switch state {
			case "empty":
				m = m.cronFilter("absent-query")
			case "loading":
				m.cron.out = true
			case "unavailable":
				m.cron.data = &cronData{jobsErr: errors.New("offline"), runsErr: errors.New("offline"), workflowsErr: errors.New("offline")}
			}
			line := strings.Split(ansi.Strip(renderCron(m)), "\n")[2]
			want := "/ search"
			if state == "empty" {
				want = "/absent-query"
			}
			require.True(t, strings.HasPrefix(strings.TrimSpace(line), want), "%c %s: %s", tab, state, line)
		}
		for _, prefix := range []string{strings.Repeat("a", 90), strings.Repeat("界", 45)} {
			m := sendRune(cronFixture(t), tab)
			m.opts.Mouse = true
			m = sendRune(m, '/')
			m, _ = updateModel(m, tea.PasteMsg{Content: prefix + "TAIL"})
			m = sendRune(m, 'Z')
			line := strings.Split(ansi.Strip(renderCron(m)), "\n")[2]
			require.Contains(t, line, "TAILZ▏", "tab %c must show the edited tail and caret", tab)
			require.LessOrEqual(t, ansi.StringWidth(line), 80)
		}
	}
}

func TestCronSearchClickGuards(t *testing.T) {
	for _, tab := range []rune{'2', '3', '4'} {
		m := sendRune(cronFixture(t), tab)
		m.opts.Mouse = true
		m, _ = updateModel(m, mouseLeftClick(4, 2))
		require.True(t, m.cron.filtering, "tab %c", tab)
	}

	for _, state := range []string{"help", "detail", "projects"} {
		m := cronFixture(t)
		m.opts.Mouse = true
		switch state {
		case "help":
			m.cron.help = true
		case "detail":
			m = sendKey(m, tea.KeyEnter)
		case "projects":
			m.scope = scope{allProjects: true}
		}
		m, _ = updateModel(m, mouseLeftClick(4, 2))
		require.False(t, m.cron.filtering, state)
	}
}
