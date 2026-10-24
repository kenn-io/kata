package tui

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func TestCronTabsPreserveNativeInput(t *testing.T) {
	for _, input := range []inputKind{inputNone, inputPriorityPrompt, inputNewIssueForm} {
		m := initialModel(Options{})
		m.input.kind = input
		h := newCronHost(m)
		out, _ := h.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
		got := out.(*cronHost)
		if input == inputNone && got.tab != 1 {
			t.Fatal("numeric tab did not select Jobs")
		}
		if input != inputNone && got.tab != 0 {
			t.Fatalf("input %v lost digit to tabs", input)
		}
	}
	m := initialModel(Options{})
	m.modal = modalQuitConfirm
	h := newCronHost(m)
	out, _ := h.Update(tea.KeyPressMsg{Code: '3', Text: "3"})
	if out.(*cronHost).tab != 0 {
		t.Fatal("modal lost digit")
	}
	h = newCronHost(initialModel(Options{}))
	out, _ = h.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if out.(*cronHost).tab != 0 {
		t.Fatal("native Tab escaped to cron")
	}
}

func TestCronChromeGeometry(t *testing.T) {
	for _, dim := range [][2]int{{80, 24}, {120, 40}, {160, 40}, {80, 8}, {80, 2}} {
		h := newCronHost(initialModel(Options{}))
		out, _ := h.Update(tea.WindowSizeMsg{Width: dim[0], Height: dim[1]})
		h = out.(*cronHost)
		if h.native.height != max(0, dim[1]-3) {
			t.Fatalf("child height %d", h.native.height)
		}
		lines := strings.Split(h.View().Content, "\n")
		if len(lines) > dim[1] {
			t.Fatalf("frame has %d rows for %v", len(lines), dim)
		}
		if dim[1] >= 3 && !strings.Contains(ansi.Strip(lines[1]), "kata カタ") {
			t.Fatal("bar missing")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > dim[0] {
				t.Fatal("wide frame")
			}
		}
	}
}

func TestCronScopeRejectsLateResults(t *testing.T) {
	n := initialModel(Options{})
	n.api = &cronTestAPI{}
	n.scope.projectID = 7
	h := newCronHost(n)
	h.tab = 1
	pending := h.loadCronCatalog()
	stale := pending().(cronCatalogMsg)
	h.native.scope.projectID = 8
	h.syncCronScope()
	h.acceptCronCatalog(stale)
	if len(h.catalogs) != 0 {
		t.Fatal("accepted previous scope catalog")
	}
	h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request + 1, catalogs: map[int64]cronCatalog{8: {name: "wrong-request"}}})
	if len(h.catalogs) != 0 {
		t.Fatal("accepted wrong request")
	}
}

func TestCronAccessLossClearsContent(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request, err: &APIError{Status: 401}})
	if len(h.catalogs) != 0 {
		t.Fatal("authentication loss retained protected content")
	}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.native.issueScoped = true
	h.syncCronScope()
	if len(h.catalogs) != 0 {
		t.Fatal("issue-scoped credential retained cron content")
	}
}

func TestCronEventsCoalesceUntilDebounce(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.tab = 1
	h.dirty = false
	h.native.api = &cronTestAPI{}
	if h.invalidateCron("unknown-project-uid") == nil {
		t.Fatal("first event needs debounce")
	}
	if h.invalidateCron("unknown-project-uid") != nil {
		t.Fatal("duplicate event queued another timer")
	}
	if !h.dirty {
		t.Fatal("event did not invalidate")
	}
	if h.loadCronCatalog() != nil {
		t.Fatal("fetch bypassed debounce")
	}
}

type cronTestAPI struct{ KataAPI }

func (*cronTestAPI) ListProjects(context.Context) ([]ProjectSummary, error) {
	return nil, fmt.Errorf("project catalog unavailable in this focused client")
}
func (*cronTestAPI) ListCronJobs(context.Context, int64) ([]records.Job, error) { return nil, nil }
func (*cronTestAPI) ListCronWorkflows(context.Context, int64) ([]records.Workflow, error) {
	return nil, nil
}
func (*cronTestAPI) ListCronRuns(context.Context, int64, string, string) (records.RunPage, error) {
	return records.RunPage{}, nil
}
func (*cronTestAPI) ShowCronRun(context.Context, int64, string) (records.Run, error) {
	return records.Run{}, nil
}

func TestCronRunHistoryPagination(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.catalogs = map[int64]cronCatalog{7: {name: "first-project"}, 8: {name: "second-project"}}
	k7, k8 := cronRunKey{project: 7}, cronRunKey{project: 8}
	h.runRequests = map[cronRunKey]uint64{k7: 1, k8: 2}
	run := records.Run{UID: "first-run", Status: "succeeded"}
	h.acceptCronRuns(cronRunsMsg{epoch: h.epoch, request: 1, key: k7, page: records.RunPage{Runs: []records.Run{run}, NextBeforeUID: "older-7"}})
	h.acceptCronRuns(cronRunsMsg{epoch: h.epoch, request: 2, key: k8, page: records.RunPage{Runs: []records.Run{{UID: "second-run", Status: "succeeded"}}, NextBeforeUID: "older-8"}})
	h.runRequests[k7] = 3
	h.acceptCronRuns(cronRunsMsg{epoch: h.epoch, request: 3, history: true, key: k7, page: records.RunPage{Runs: []records.Run{run, {UID: "older", Status: "failed"}}}})
	if len(h.catalogs[7].runs) != 2 || len(h.catalogs[8].runs) != 1 {
		t.Fatal("page deduplication or independent project history changed", h.catalogs)
	}
	if h.catalogs[7].before != "" || h.catalogs[8].before != "older-8" {
		t.Fatal("cursors crossed project identities")
	}
	before := len(h.catalogs[7].runs)
	h.acceptCronRuns(cronRunsMsg{epoch: h.epoch - 1, request: 3, history: true, key: k7, page: records.RunPage{Runs: []records.Run{{UID: "stale"}}}})
	if len(h.catalogs[7].runs) != before {
		t.Fatal("old scope run accepted")
	}
}

func TestCronNativeRoundTrip(t *testing.T) {
	m := initialModel(Options{})
	m.list.cursor = 3
	m.list.filter.Search = "example-filter"
	h := newCronHost(m)
	h.selectCronTab(1)
	h.selectCronTab(2)
	h.selectCronTab(3)
	h.selectCronTab(0)
	if h.native.list.cursor != 3 || h.native.list.filter.Search != "example-filter" {
		t.Fatal("cron changed native issue state")
	}
}
func TestCronQuitOverlayFromCron(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	h.selectCronTab(1)
	h.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if h.native.modal != modalQuitConfirm {
		t.Fatal("cron bypassed quit confirmation")
	}
	if !strings.Contains(ansi.Strip(h.View().Content), "Are you sure") {
		t.Fatal(h.View().Content)
	}
	h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if h.native.modal != modalNone || h.tab != 1 {
		t.Fatal("quit cancellation lost cron tab")
	}
}
func TestCronRunEventsInvalidateHistory(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.catalogs = map[int64]cronCatalog{7: {runsLoaded: true}}
	h.invalidateCron("unresolved-uid")
	if h.catalogs[7].runsLoaded {
		t.Fatal("cron event retained a clean run page")
	}
}
func TestCronMouseOffset(t *testing.T) {
	h := newCronHost(initialModel(Options{Mouse: true}))
	h.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	def := jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summary"},"overlap":"allow","catchup":"skip"}`)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{{UID: "first", Name: "First", ProjectUID: "7", Definition: def}, {UID: "second", Name: "Second", ProjectUID: "7", Definition: def}}}}
	h.selectCronTab(1)
	h.View()
	h.Update(tea.MouseClickMsg{X: 10, Y: 5, Button: tea.MouseLeft})
	text := ansi.Strip(h.View().Content)
	selected := ""
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "▶") {
			selected = line
		}
	}
	if !strings.Contains(selected, "Second") {
		t.Fatal("cron row click used wrong screen offset", text)
	}
}

func TestCronDaemonSwitchFencesPendingSave(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.catalogs = map[int64]cronCatalog{7: {name: "old-project"}}
	old := h.epoch
	h.native.connGen++
	h.syncCronScope()
	h.updateCronBoard()
	h.catalogs = map[int64]cronCatalog{8: {name: "new-project"}}
	saved := records.Job{UID: "old-job", ProjectUID: "8", Name: "old result"}
	h.Update(cronChildMsg{epoch: old, msg: cronui.ChangedMsg{Saved: &saved}})
	if len(h.catalogs[8].jobs) != 0 {
		t.Fatal("save crossed daemon generation")
	}
}

func TestCronRunPagesKeepNewestEvidence(t *testing.T) {
	got := mergeCronRuns([]records.Run{{UID: "same", Revision: 2, Status: "succeeded"}}, []records.Run{{UID: "same", Revision: 1, Status: "running"}})
	if len(got) != 1 || got[0].Status != "succeeded" {
		t.Fatal("late detail replaced a newer observation", got)
	}
}

func TestCronChromeDegenerateGeometry(t *testing.T) {
	for _, dim := range [][2]int{{0, 0}, {80, 0}, {0, 24}} {
		h := newCronHost(initialModel(Options{}))
		h.Update(tea.WindowSizeMsg{Width: dim[0], Height: dim[1]})
		if h.View().Content != "" {
			t.Fatal("empty viewport emitted content", dim)
		}
	}
	h := newCronHost(initialModel(Options{}))
	h.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	h.selectCronTab(1)
	if !strings.Contains(ansi.Strip(h.View().Content), "kata tui needs more space") {
		t.Fatal("cron bypassed native narrow-terminal hint")
	}
}

func FuzzCronHostGeometry(f *testing.F) {
	for _, size := range [][3]uint8{{80, 24, 0}, {120, 40, 1}, {160, 40, 2}, {80, 8, 3}, {0, 0, 0}, {60, 10, 1}} {
		f.Add(size[0], size[1], size[2])
	}
	f.Fuzz(func(t *testing.T, width, height, tab uint8) {
		// A byte covers widths 0–255; height is materialized to fit a small terminal.
		w, h := int(width), int(height)%64
		m := newCronHost(initialModel(Options{}))
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		m.selectCronTab(int(tab % 4))
		text := m.View().Content
		if text == "" {
			return
		}
		lines := strings.Split(text, "\n")
		if len(lines) > h {
			t.Fatal("render exceeds height", w, h, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > w {
				t.Fatal("render exceeds width", w, ansi.StringWidth(line))
			}
		}
	})
}

func TestCronAccessLossDropsProtectedDraft(t *testing.T) {
	m := initialModel(Options{})
	m.api = NewClient("http://daemon.example", nil)
	h := newCronHost(m)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.selectCronTab(1)
	h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if h.board.CanLeave() {
		t.Fatal("fixture did not open native job draft")
	}
	h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request, err: &APIError{Status: 401}})
	h.updateCronBoard()
	if !h.board.CanLeave() {
		t.Fatal("authentication loss retained a protected draft")
	}
}

func TestCronPermissionRecoveryOnScopeChange(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.native.api = &cronTestAPI{}
			h.tab = 1
			h.clearCronAccess(&APIError{Status: status})
			h.native.connGen++
			h.native.activeAuthRejected = false
			h.native.authCapabilitiesReady = true
			h.syncCronScope()
			if cmd := h.loadCronCatalog(); cmd == nil {
				t.Fatal("authorized new scope cannot load cron after permission loss")
			}
		})
	}
}

func TestCronHostSourceChrome(t *testing.T) {
	h := newCronHost(initialModel(Options{Mouse: true}))
	h.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	def := jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summary"},"overlap":"allow","catchup":"skip"}`)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{{UID: "first", Name: "First", ProjectUID: "7", Definition: def}}, runs: []records.Run{{UID: "run-1", ProjectKey: "7", Status: "running"}}}}
	h.selectCronTab(1)
	text := ansi.Strip(h.View().Content)
	lines := strings.Split(text, "\n")
	if !strings.HasSuffix(lines[1], "1 running") || !strings.Contains(lines[3], "schedule") {
		t.Fatal("source list chrome", text)
	}
	h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if h.cronTabHeight() != 0 || strings.Contains(ansi.Strip(h.View().Content), "kata カタ") {
		t.Fatal("detail retained outer tabs")
	}
	h.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if len(strings.Split(h.View().Content, "\n")) > 40 {
		t.Fatal("detail exceeded resized terminal")
	}
	h.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if h.cronTabHeight() != 3 {
		t.Fatal("back did not restore tabs")
	}
}
