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

func TestCronWalkAndRecordedDefinitions(t *testing.T) {
	m := cronFixture(t)
	m = sendKey(m, tea.KeyEnter)
	require.Contains(t, stripANSI(m.viewBody()), "jobs / Daily summary")
	// Every line, including prose and JSON, is traversable.
	_, _, lines := cronScreen(m)
	for i, l := range lines {
		if l.target.kind == 2 {
			for range i {
				m = sendRune(m, 'j')
			}
			break
		}
	}
	m = sendKey(m, tea.KeyEnter)
	require.Contains(t, stripANSI(m.viewBody()), "runs / 01ARZ3NDEKTSV4RRFFQ69G5FAY")
	_, _, lines = cronScreen(m)
	jobLine := -1
	for i, l := range lines {
		if l.target.kind == 1 {
			jobLine = i
			break
		}
	}
	require.GreaterOrEqual(t, jobLine, 0)
	require.Contains(t, lines[jobLine].text, "current definition changed")
	for range jobLine {
		m = sendRune(m, 'j')
	}
	m = sendKey(m, tea.KeyEnter)
	require.Contains(t, stripANSI(m.viewBody()), "jobs / Daily summary")
	m = sendRune(m, 'h')
	m = sendRune(m, 'h')
	require.Len(t, m.cron.stack, 1)
}

func TestCronRefreshKeepsDetailAndCoalescesEvents(t *testing.T) {
	m := cronFixture(t)
	a := &cronReadTestAPI{data: *m.cron.data}
	a.data.jobs = slices.Clone(a.data.jobs)
	m.api = a
	m = sendKey(m, tea.KeyEnter)
	var cmd tea.Cmd
	m, cmd = updateModel(m, keyRune('r'))
	require.NotNil(t, cmd)
	for range 10 {
		m, _ = updateModel(m, eventReceivedMsg{eventType: "cron.job.updated", projectID: 7})
	}
	require.Contains(t, stripANSI(m.viewBody()), "Daily summary")
	require.NotContains(t, stripANSI(m.viewBody()), "no longer in the loaded data")
	first := cmd()
	require.Equal(t, 1, a.calls)
	a.data.jobs[0].Name = "New name"
	m, cmd = updateModel(m, first)
	require.Contains(t, stripANSI(m.viewBody()), "Daily summary")
	require.NotContains(t, stripANSI(m.viewBody()), "New name")
	require.NotNil(t, cmd, "events during the read request one replacement read")
	m, cmd = updateModel(m, cmd())
	require.Nil(t, cmd)
	require.Equal(t, 2, a.calls)
	require.Contains(t, stripANSI(m.viewBody()), "New name")
}

func TestCronFreshness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event tea.Msg
		away  bool
	}{
		{"event", eventReceivedMsg{eventType: "cron.job.updated", projectID: 7}, false},
		{"reset", resetRequiredMsg{}, false},
		{"refresh", keyRune('r'), false},
		{"event while away", eventReceivedMsg{eventType: "cron.job.updated", projectID: 7}, true},
		{"reset while away", resetRequiredMsg{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronFixture(t)
			a := &cronReadTestAPI{data: *m.cron.data}
			a.data.jobs = slices.Clone(a.data.jobs)
			a.data.jobs[0].Name = "New title"
			m.api = a
			if tc.away {
				m = sendRune(m, '1')
			}
			m, cmd := updateModel(m, tc.event)
			if tc.away {
				require.Zero(t, a.calls)
				m, cmd = updateModel(m, keyRune('2'))
			}
			require.NotNil(t, cmd)
			// Update appends the cron read after the existing issue/SSE commands.
			msg := cmd()
			if batch, ok := msg.(tea.BatchMsg); ok {
				msg = batch[len(batch)-1]()
			}
			m, _ = updateModel(m, msg)
			require.Equal(t, 1, a.calls)
			require.Contains(t, stripANSI(m.viewBody()), "New title")
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
		m = sendRune(m, '1')
		m = sendRune(m, '2')
		require.Empty(t, m.cron.stack)
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
		require.Contains(t, stripANSI(h.viewBody()), "A job is a schedule plus an action.")
		require.Contains(t, stripANSI(h.viewBody()), "kata cron --help")
		require.Contains(t, stripANSI(h.viewBody()), map[bool]string{false: "filter", true: "follow"}[detail])
		require.Equal(t, h.cron.tab, sendRune(h, '3').cron.tab)
		require.Equal(t, viewCron, sendRune(h, '?').view)
		require.Equal(t, viewProjects, sendRune(m, 'P').view)
		q := sendRune(m, 'q')
		require.Equal(t, modalQuitConfirm, q.modal)
	}
	m := cronFixture(t)
	m = sendRune(m, '/')
	pasted, _ := updateModel(m, tea.PasteMsg{Content: "daily"})
	typed := m
	for _, r := range "daily" {
		typed = sendRune(typed, r)
	}
	require.Equal(t, stripANSI(typed.viewBody()), stripANSI(pasted.viewBody()))
	require.Contains(t, stripANSI(pasted.viewBody()), "1 job")
	pasted = sendRune(pasted, '3')
	require.Equal(t, 1, pasted.cron.tab)
	require.Contains(t, pasted.cron.lists[1].query, "3")
}

func TestCronTabsRespectExistingInputs(t *testing.T) {
	m := cronFixture(t)
	m = sendRune(m, '1')
	m, cmd := updateModel(m, keyRune('/'))
	require.NotNil(t, cmd)
	m, _ = updateModel(m, cmd())
	m = sendRune(m, '2')
	require.Equal(t, viewList, m.view)
	require.Contains(t, m.list.filter.Search, "2")
	m = cronFixture(t)
	m = sendRune(m, '1')
	m.list = snapListModel([]Issue{testIssue("abc4")})
	m = sendRune(m, '!')
	m = sendRune(m, '2')
	require.Equal(t, viewList, m.view)
	// Typed prompts own digit keys before global routing.
	m = cronFixture(t)
	m.view = viewFederation
	m.federation.mode = federationModeAdoptConfirm
	m = sendRune(m, '2')
	require.Equal(t, viewFederation, m.view)
	require.Equal(t, "2", m.federation.adoptConfirmInput)
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
	_, _, lines = cronScreen(m)
	document = ""
	for _, l := range lines {
		document += l.text + "\n"
	}
	require.Contains(t, document, "echo first\necho second")
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
	m.cron.lists[1].cursor = 100
	m = sendRune(m, 'k')
	require.Equal(t, 0, m.cron.lists[1].cursor)
	m = sendKey(m, tea.KeyEnter)
	m.cron.stack[0].cursor = 1000
	m = sendRune(m, 'j')
	_, _, lines = cronScreen(m)
	m = sendRune(m, 'k')
	require.Less(t, m.cron.stack[0].cursor, len(lines))
}

func TestCronRunsOrderAndEndTime(t *testing.T) {
	m := cronFixture(t)
	p := m.cron.data
	other := p.runs[0]
	other.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	p.runs = append(p.runs, other)
	m = sendRune(m, '3')
	_, _, lines := cronScreen(m)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAY", lines[0].target.uid)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAX", lines[1].target.uid)
	require.Contains(t, lines[0].text, "05-01 11:00")
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

func TestCronReturnAcrossLayouts(t *testing.T) {
	for _, detail := range []bool{false, true} {
		t.Run(fmt.Sprint(detail), func(t *testing.T) {
			m := cronFixture(t)
			m = sendRune(m, '1')
			issue := testIssue("abc4")
			m.detail.issue = &issue
			m.view = viewDetail
			m = sendRune(m, '?')
			m = sendRune(m, '?')
			if !detail {
				m, _ = updateModel(m, popDetailMsg{})
			}
			want := m.view
			m = sendRune(m, '2')
			m, _ = updateModel(m, tea.WindowSizeMsg{Width: 200, Height: 40})
			m = sendRune(m, '?')
			m = sendRune(m, '?')
			m = sendRune(m, '1')
			require.Equal(t, want, m.view)
			require.Equal(t, map[bool]focusPane{false: focusList, true: focusDetail}[detail], m.focus)
			m = sendRune(m, 'c')
			require.Equal(t, map[bool]inputKind{false: inputNone, true: inputCommentForm}[detail], m.input.kind)
			// Narrowing while covered also preserves the focused pane.
			m.input.kind = inputNone
			m = sendRune(m, '2')
			m, _ = updateModel(m, tea.WindowSizeMsg{Width: 80, Height: 24})
			m = sendRune(m, '1')
			require.Equal(t, want, m.view)
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
	require.Zero(t, a.calls)
	require.Len(t, strings.Split(m.viewBody(), "\n"), 24)
	require.Contains(t, stripANSI(m.viewBody()), "Press P to pick a project, then 2 for Jobs, 3 for Runs, or 4 for Workflows.")
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
	require.Contains(t, stripANSI(m.viewBody()), "jobs unavailable: see error below")
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
			m, _ = updateModel(m, tea.PasteMsg{Content: query})
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

func TestCronEntryKeepsTitle(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 80, 24
	require.Contains(t, stripANSI(m.viewBody()), "[kata カタ] jobs runs workflows")
	title, _, _ := strings.Cut(m.viewBody(), "\n")
	m = sendRune(m, '2')
	rows := strings.Split(m.viewBody(), "\n")
	require.Equal(t, title, rows[0])
	require.Contains(t, stripANSI(rows[1]), "kata カタ [jobs] runs workflows")
	m = sendRune(m, '1')
	require.Equal(t, viewList, m.view)
	require.Contains(t, stripANSI(m.viewBody()), "[kata カタ] jobs runs workflows")
	m = sendRune(m, '?')
	m = sendRune(m, '3')
	require.Equal(t, viewHelp, m.view)
}

func TestCronWorkflowLinksAndDisappearingRecord(t *testing.T) {
	m := cronFixture(t)
	m = sendRune(m, '4')
	m = sendKey(m, tea.KeyEnter)
	_, _, lines := cronScreen(m)
	for i, line := range lines {
		if line.target.kind == 1 {
			for range i {
				m = sendRune(m, 'j')
			}
			m = sendKey(m, tea.KeyEnter)
			require.Contains(t, stripANSI(m.viewBody()), "jobs / Daily summary")
			m = sendRune(m, 'h')
			break
		}
	}
	m = sendRune(m, 'G')
	m = sendKey(m, tea.KeyEnter)
	require.Contains(t, stripANSI(m.viewBody()), "runs / 01ARZ3NDEKTSV4RRFFQ69G5FAY")
	m = sendRune(m, '2')
	m = sendKey(m, tea.KeyEnter)
	data := *m.cron.data
	data.jobs = nil
	m, _ = updateModel(m, cronLoadedMsg(data))
	require.Contains(t, stripANSI(m.viewBody()), "this job is no longer in the loaded data")
}

func TestCronGlobalExitKeepsKataReturn(t *testing.T) {
	for _, key := range []rune{'F', 'C'} {
		t.Run(string(key), func(t *testing.T) {
			m := cronFixture(t)
			m = sendRune(m, '1')
			issue := testIssue("abc4")
			m.detail.issue, m.view = &issue, viewDetail
			m = sendRune(m, '2')
			m = sendRune(m, key)
			m = sendKey(m, tea.KeyEsc)
			m = sendRune(m, '1')
			require.Equal(t, viewDetail, m.view)
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
			job.Name, workflow.Name = "Selected job", "Selected workflow"
			data.jobs = append(data.jobs, job)
			data.runs = append(data.runs, run)
			data.workflows = append(data.workflows, workflow)
			m, _ = updateModel(m, cronLoadedMsg(data))
			m = sendRune(m, tab)
			m = sendRune(m, 'j')
			job.UID, run.UID, workflow.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAB", "01ARZ3NDEKTSV4RRFFQ69G5FAB", "01ARZ3NDEKTSV4RRFFQ69G5FAB"
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
	for _, screen := range []string{"jobs", "runs", "workflows", "detail", "all-projects"} {
		for _, notice := range []string{"inbox", "reset"} {
			t.Run(screen+"/"+notice, func(t *testing.T) {
				m := cronFixture(t)
				switch screen {
				case "runs":
					m = sendRune(m, '3')
				case "workflows":
					m = sendRune(m, '4')
				case "detail":
					m = sendKey(m, tea.KeyEnter)
				case "all-projects":
					m.scope.allProjects = true
				}
				want := "resynced"
				if notice == "inbox" {
					m.api = &inboxTestAPI{}
					var lookup tea.Cmd
					m, lookup = updateModel(m, keyRune('I'))
					require.NotNil(t, lookup)
					m, _ = updateModel(m, lookup())
					want = "No Inbox project designated"
				} else {
					m, _ = updateModel(m, resetRequiredMsg{})
				}
				rows := strings.Split(stripANSI(m.viewContent()), "\n")
				require.Contains(t, strings.Join(rows[:min(len(rows), m.height)], "\n"), want)
			})
		}
	}
}
