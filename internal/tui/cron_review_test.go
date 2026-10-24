package tui

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func TestCronNarrowFormsRemainVisible(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		h := newCronHost(initialModel(Options{}))
		h.native.input = newNewIssueForm()
		h.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
		if !strings.Contains(ansi.Strip(h.View().Content), "new issue") {
			t.Fatal("native form hidden", h.View().Content)
		}
	})
	t.Run("cron", func(t *testing.T) {
		m := initialModel(Options{})
		m.api = NewClient("http://daemon.example", nil)
		h := newCronHost(m)
		h.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
		h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
		h.selectCronTab(1)
		h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
		if !strings.Contains(ansi.Strip(h.View().Content), "new job") {
			t.Fatal("cron form hidden", h.View().Content)
		}
	})
}

func TestCronDeniedProjectClearsAndFencesRunEvidence(t *testing.T) {
	for _, surface := range []string{"catalog", "runs", "detail"} {
		t.Run(surface, func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.native.api = &cronReviewAPI{}
			h.catalogs = map[int64]cronCatalog{7: {name: "denied-project", runs: []records.Run{{UID: "old"}}}, 8: {name: "accessible-project", runs: []records.Run{{UID: "sibling"}}}}
			key := cronRunKey{7, "job"}
			h.jobPages = map[cronRunKey]records.RunPage{key: {Runs: []records.Run{{UID: "protected"}}}}
			late := h.cronDetail(cronui.DetailMsg{Kind: "run", ProjectKey: "7", UID: "late"})()
			denied := &APIError{Status: 403}
			switch surface {
			case "catalog":
				h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request, catalogs: map[int64]cronCatalog{7: {problem: cronProblemText(denied)}, 8: h.catalogs[8]}})
			case "runs":
				h.runRequests = map[cronRunKey]uint64{key: 1}
				h.acceptCronRuns(cronRunsMsg{epoch: h.epoch, request: 1, key: key, err: denied})
			case "detail":
				h.cronDetail(cronui.DetailMsg{Kind: "run", ProjectKey: "7", UID: "newer"})
				h.Update(cronRunDetailMsg{epoch: h.epoch, request: h.detailRequests[7], project: 7, err: denied})
			}
			h.Update(late)
			if len(h.catalogs[7].runs) != 0 || len(h.jobPages) != 0 {
				t.Fatal("denied evidence survived or returned", h.catalogs[7], h.jobPages)
			}
			if len(h.catalogs[8].runs) != 1 {
				t.Fatal("denial cleared accessible sibling")
			}
		})
	}
}

func TestCronRefreshReloadsOpenJobHistory(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronReviewAPI{}
	job := records.Job{UID: "job", ProjectUID: "7", Name: "Example"}
	cat := cronCatalog{name: "example-project", jobs: []records.Job{job}}
	h.catalogs = map[int64]cronCatalog{7: cat}
	h.selectCronTab(1)
	h.board.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	h.dirty = false
	h.invalidateCron("")
	h.Update(cronDebounceMsg{epoch: h.epoch})
	h.Update(cronCatalogMsg{epoch: h.epoch, request: h.request, catalogs: map[int64]cronCatalog{7: cat}})
	if !h.runPending[cronRunKey{7, "job"}] {
		t.Fatal("open job history was not reloaded after invalidation")
	}
}

type cronReviewAPI struct{ cronTestAPI }

func (*cronReviewAPI) ShowCronRun(_ context.Context, _ int64, uid string) (records.Run, error) {
	return records.Run{UID: uid}, nil
}

func (a *cronReviewAPI) GetIssueDetail(_ context.Context, id int64, uid string) (*IssueDetail, error) {
	return &IssueDetail{Issue: &Issue{ProjectID: id, UID: uid}}, nil
}

func TestCronIssueJumpRejectsSupersededNavigation(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronReviewAPI{}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.selectCronTab(1)
	first := h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "first"})()
	second := h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "second"})()
	h.Update(second)
	h.Update(first)
	if h.native.detail.issue.UID != "second" {
		t.Fatal("older jump replaced latest issue", h.native.detail.issue.UID)
	}
	h.selectCronTab(1)
	abandoned := h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "abandoned"})()
	h.selectCronTab(3)
	h.Update(abandoned)
	if h.tab != 3 {
		t.Fatal("abandoned issue jump stole tab focus")
	}
}

func TestCronQuitModalOwnsMouse(t *testing.T) {
	h := newCronHost(initialModel(Options{Mouse: true}))
	h.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	h.catalogs = map[int64]cronCatalog{7: {jobs: []records.Job{{ProjectUID: "7", UID: "job", Name: "Example"}}}}
	h.selectCronTab(1)
	h.native.modal = modalQuitConfirm
	h.View()
	h.Update(tea.MouseClickMsg{X: 10, Y: 5, Button: tea.MouseLeft})
	if _, ok := h.board.ActiveDetail(); ok {
		t.Fatal("quit modal allowed underlying detail navigation")
	}
}

func TestCronAutoBackgroundSurvivesScopeChange(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("KATA_COLOR_MODE", "auto")
	h := newCronHost(initialModel(Options{}))
	h.Update(tea.BackgroundColorMsg{Color: color.White})
	auto := cronui.Tabs(1, 80)
	t.Setenv("KATA_COLOR_MODE", "light")
	cronui.New(cronui.Options{})
	light := cronui.Tabs(1, 80)
	if auto != light {
		t.Fatal("automatic light background did not reach cron child")
	}
	t.Setenv("KATA_COLOR_MODE", "auto")
	h.native.connGen++
	h.syncCronScope()
	h.updateCronBoard()
	if got := cronui.Tabs(1, 80); got != light {
		t.Fatal("scope recreation forgot terminal background")
	}
}

func TestCronProjectDenialFencesCatalogAndDraft(t *testing.T) {
	m := initialModel(Options{})
	m.api = NewClient("http://daemon.example", nil)
	h := newCronHost(m)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.selectCronTab(1)
	h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if h.board.CanLeave() {
		t.Fatal("fixture did not open draft")
	}
	late := cronCatalogMsg{epoch: h.epoch, request: h.request, catalogs: map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{{UID: "protected"}}}}}
	key := cronRunKey{project: 7}
	h.runRequests = map[cronRunKey]uint64{key: 1}
	h.Update(cronRunsMsg{epoch: h.epoch, request: 1, key: key, err: &APIError{Status: 403}})
	if !h.board.CanLeave() {
		t.Fatal("denied project retained protected draft")
	}
	h.Update(late)
	if len(h.catalogs[7].jobs) != 0 {
		t.Fatal("pre-denial catalog refilled protected definitions")
	}
}

func TestCronWriteForbiddenPreservesSibling(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || r.URL.Path != "/api/v1/projects/7/cron/jobs/01ARZ3NDEKTSV4RRFFQ69G5FAV" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"project is read-only"}}`))
			}))
			defer server.Close()
			m := initialModel(Options{})
			m.api = NewClient(server.URL, server.Client())
			h := newCronHost(m)
			def := jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summarize"},"overlap":"allow","catchup":"skip"}`)
			h.catalogs = map[int64]cronCatalog{7: {name: "readonly-project", jobs: []records.Job{{ProjectUID: "7", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "First", DefinitionEventUID: "guard", Definition: def}}}, 8: {name: "writable-project", jobs: []records.Job{{ProjectUID: "8", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Name: "Second", Definition: def}}}}
			h.selectCronTab(1)
			_, cmd := h.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
			deliverCronReviewCmd(t, h, cmd, 0)
			if status == 401 {
				if len(h.catalogs) != 0 {
					t.Fatal("401 retained protected catalogs")
				}
				return
			}
			if len(h.catalogs[8].jobs) != 1 || h.cronDenied {
				t.Fatal("project write denial disabled accessible sibling", h.catalogs, h.cronDenied)
			}
			if len(h.catalogs[7].jobs) != 0 {
				t.Fatal("denied project retained definitions")
			}
			h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
			if h.board.CanLeave() {
				t.Fatal("writable sibling could no longer open job form")
			}
		})
	}
}
func deliverCronReviewCmd(t *testing.T, h *cronHost, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil {
		return
	}
	if depth > 20 {
		t.Fatal("unbounded command lifecycle")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			deliverCronReviewCmd(t, h, child, depth+1)
		}
		return
	}
	_, next := h.Update(msg)
	deliverCronReviewCmd(t, h, next, depth+1)
}
