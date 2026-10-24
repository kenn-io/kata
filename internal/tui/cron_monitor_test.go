package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func TestCronTabsRespectNativePriorityAndAdoption(t *testing.T) {
	for _, prompt := range []string{"priority", "adoption"} {
		t.Run(prompt, func(t *testing.T) {
			m := initialModel(Options{})
			if prompt == "priority" {
				m.list.pendingPriority = true
			} else {
				m.view = viewFederation
				m.federation.mode = federationModeAdoptConfirm
			}
			h := newCronHost(m)
			h.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
			if h.tab != 0 {
				t.Fatal("tab shortcut stole prompt input")
			}
			if prompt == "adoption" && h.native.federation.adoptConfirmInput != "2" {
				t.Fatal("confirmation did not receive digit")
			}
			if prompt == "priority" && h.native.list.pendingPriority {
				t.Fatal("priority prompt did not consume digit")
			}
		})
	}
}

func TestCronInvalidationKeepsCatalogRequestInFlight(t *testing.T) {
	n := initialModel(Options{})
	n.api = &cronTestAPI{}
	n.scope.projectID = 7
	n.scope.projectName = "example-project"
	h := newCronHost(n)
	h.tab = 1
	pending := h.loadCronCatalog()
	if pending == nil {
		t.Fatal("catalog request did not start")
	}
	request := h.request
	h.invalidateCron("")
	h.invalidateCron("")
	if !h.loading || h.request != request {
		t.Fatal("event invalidated in-flight catalog request")
	}
	h.Update(pending())
	if _, ok := h.catalogs[7]; !ok {
		t.Fatal("in-flight catalog response was discarded")
	}
}

type monitorRunAPI struct{ cronTestAPI }

func (*monitorRunAPI) ListCronRuns(context.Context, int64, string, string) (records.RunPage, error) {
	return records.RunPage{Runs: []records.Run{{UID: "observed", Status: "succeeded"}}}, nil
}

type monitorHistoryAPI struct{ cronTestAPI }

func (*monitorHistoryAPI) ListCronRuns(_ context.Context, _ int64, _ string, before string) (records.RunPage, error) {
	if before != "" {
		return records.RunPage{Runs: []records.Run{{UID: "obsolete", Status: "running"}}}, nil
	}
	return records.RunPage{Runs: []records.Run{{UID: "fresh", Status: "succeeded"}}}, nil
}

func TestCronInvalidatedHistoryPageCannotRestoreOldEvidence(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &monitorHistoryAPI{}
	h.tab = 2
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", runsLoaded: true, before: "older"}}
	pending := h.loadCronRuns("", 7, true)
	if pending == nil {
		t.Fatal("history request did not start")
	}
	h.resetCronEvidence()
	h.acceptCronRuns(pending().(cronRunsMsg))
	if cat := h.catalogs[7]; len(cat.runs) != 0 || cat.runsLoaded {
		t.Fatalf("invalidated history restored obsolete records before refresh: %+v", cat)
	}
	if h.runPending[cronRunKey{project: 7}] {
		t.Fatal("history request stayed pending")
	}
	fresh := h.loadCronRuns("", 7, false)
	if fresh == nil {
		t.Fatal("fresh history request did not start")
	}
	h.acceptCronRuns(fresh().(cronRunsMsg))
	cat := h.catalogs[7]
	if len(cat.runs) != 1 || cat.runs[0].UID != "fresh" || !cat.runsLoaded {
		t.Fatalf("invalidated history restored obsolete records: %+v", cat.runs)
	}
	if h.runPending[cronRunKey{project: 7}] {
		t.Fatal("history request stayed pending")
	}
}

type monitorFailingRunAPI struct {
	cronTestAPI
	calls int
}

func (a *monitorFailingRunAPI) ListCronRuns(context.Context, int64, string, string) (records.RunPage, error) {
	a.calls++
	if a.calls == 1 {
		return records.RunPage{}, errors.New("older read failed")
	}
	return records.RunPage{Runs: []records.Run{{UID: "fresh", Status: "succeeded"}}}, nil
}
func TestCronStaleRunErrorDoesNotPoisonRefreshedCatalog(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	api := &monitorFailingRunAPI{}
	h.native.api = api
	h.tab = 1
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	pending := h.loadCronRuns("", 0, false)
	h.resetCronEvidence()
	h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request, catalogs: map[int64]cronCatalog{7: {name: "example-project"}}})
	deliverCronReviewCmd(t, h, pending, 0)
	if h.catalogs[7].problem != "" || len(h.catalogs[7].runs) != 1 || api.calls != 2 {
		t.Fatal("stale run failure blocked fresh history", h.catalogs[7].problem, api.calls)
	}
}

func TestCronReadbackDenialDiscardsProtectedEditor(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "PUT" {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"write outcome unknown"}}`))
					return
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"access lost"}}`))
			}))
			defer s.Close()
			n := initialModel(Options{})
			n.api = NewClient(s.URL, s.Client())
			n.scope.projectID = 7
			h := newCronHost(n)
			h.width = 200
			h.height = 40
			h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{{UID: "job", Name: "Example", ProjectUID: "7", DefinitionEventUID: "guard", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)}}}}
			h.selectCronTab(1)
			h.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
			h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			h.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			_, cmd := h.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			deliverCronReviewCmd(t, h, cmd, 0)
			if !h.board.CanLeave() || len(h.catalogs[7].jobs) != 0 {
				t.Fatal("denied editor retained protected content or stayed busy", h.View().Content)
			}
		})
	}
}

func TestCronDeleteStaysRemovedWhenRefreshFails(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"offline"}}`))
	}))
	defer s.Close()
	n := initialModel(Options{})
	n.api = NewClient(s.URL, s.Client())
	n.scope.projectID = 7
	h := newCronHost(n)
	h.width = 200
	h.height = 40
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{{UID: "job", Name: "Example", ProjectUID: "7", DefinitionEventUID: "guard", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"overlap":"allow","catchup":"skip"}`)}}}}
	h.selectCronTab(1)
	h.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	_, cmd := h.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	deliverCronReviewCmd(t, h, cmd, 0)
	if len(h.catalogs[7].jobs) != 0 {
		t.Fatal("deleted job returned from stale host catalog")
	}
}

func TestCronRefreshRetainsHistoryUntilReplacement(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.catalogs = map[int64]cronCatalog{7: {runs: []records.Run{{UID: "older", Revision: 1}}, before: "next", runsLoaded: true}}
	h.resetCronEvidence()
	if len(h.catalogs[7].runs) != 1 || h.catalogs[7].before != "next" || h.catalogs[7].runsLoaded {
		t.Fatal("refresh hid accepted history or failed to mark it stale")
	}
	h.acceptCronRuns(cronRunsMsg{epoch: h.epoch, key: cronRunKey{project: 7}, evidence: h.catalogs[7].evidence})
	if len(h.catalogs[7].runs) != 0 || h.catalogs[7].before != "" {
		t.Fatal("completed head refresh retained obsolete history")
	}
}

func TestCronCredentialRejectionClearsAllProjects(t *testing.T) {
	for _, code := range []string{"token_invalid", "auth_invalid"} {
		for _, surface := range []string{"catalog", "runs", "detail", "write"} {
			t.Run(code+"/"+surface, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(403)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"credential rejected"}}`, code)
				}))
				defer s.Close()
				n := initialModel(Options{})
				n.api = NewClient(s.URL, s.Client())
				n.scope.projectID = 7
				h := newCronHost(n)
				def := []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"overlap":"allow","catchup":"skip"}`)
				h.catalogs = map[int64]cronCatalog{7: {name: "first-project", jobs: []records.Job{{UID: "job", ProjectUID: "7", Definition: def}}}, 8: {name: "second-project", jobs: []records.Job{{UID: "sibling", ProjectUID: "8", Definition: def}}}}
				h.tab = 1
				h.updateCronBoard()
				err := decodeError([]byte(fmt.Sprintf(`{"error":{"code":%q,"message":"credential rejected"}}`, code)), 403, "GET", "/api/v1/example")
				switch surface {
				case "catalog":
					h.Update(h.loadCronCatalog()())
				case "runs":
					key := cronRunKey{project: 7}
					h.runRequests = map[cronRunKey]uint64{key: 1}
					h.Update(cronRunsMsg{epoch: h.epoch, request: 1, key: key, err: err})
				case "detail":
					h.detailRequests = map[int64]uint64{7: 1}
					h.Update(cronRunDetailMsg{epoch: h.epoch, request: 1, project: 7, err: err})
				case "write":
					_, cmd := h.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
					deliverCronReviewCmd(t, h, cmd, 0)
				}
				if len(h.catalogs) != 0 || !h.native.activeAuthRejected {
					t.Fatal("rejected credential retained sibling authority", h.catalogs, h.native.activeAuthRejected)
				}
			})
		}
	}
}

func TestCronUnboundIssueJumpKeepsProject(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronReviewAPI{}
	h.native.projectUIDByID = map[int64]string{7: "project-uid"}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.dirty = false
	if h.invalidateCron("project-uid") == nil && !h.dirty {
		t.Fatal("unbound catalog rejected project event")
	}
	h.Update(h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "issue"})())
	if h.native.detail.scopePID != 7 {
		t.Fatal("issue detail uses wrong project", h.native.detail.scopePID)
	}
}

func TestCronProjectEventsInvalidateCatalog(t *testing.T) {
	for _, event := range []string{"project.created", "project.removed", "project.restored", "project.renamed", "project.merged"} {
		t.Run(event, func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.native.api = &cronTestAPI{}
			h.tab = 1
			h.dirty = false
			h.Update(eventReceivedMsg{gen: h.native.connGen, eventType: event})
			if !h.dirty && !h.loading {
				t.Fatal("project event left catalog clean")
			}
		})
	}
}

func TestCronCatalogRefreshReloadsVisibleRunEvidence(t *testing.T) {
	for _, tab := range []int{1, 3} {
		h := newCronHost(initialModel(Options{}))
		h.native.api = &cronTestAPI{}
		h.tab = tab
		h.catalogs = map[int64]cronCatalog{7: {name: "example-project", runsLoaded: false}}
		h.Update(cronCatalogMsg{epoch: h.epoch, request: h.request, catalogs: map[int64]cronCatalog{7: {name: "example-project"}}})
		if !h.runPending[cronRunKey{project: 7}] {
			t.Fatal("visible cron view retained stale observations", tab)
		}
	}
}

func TestCronWorkflowSaveRejectsUnknownMembers(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow":{}}`))
	}))
	defer s.Close()
	c := NewClient(s.URL, s.Client())
	_, err := c.SaveCronWorkflow(context.Background(), 7, "example-actor", records.Workflow{UID: "flow", Definition: []byte(`{"version":1,"steps":[{"key":"step","kind":"prompt","prompt":"work","unknown":true}]}`)})
	if err == nil || requests != 0 {
		t.Fatal("unknown member reached typed write", err, requests)
	}
}

func TestCronRemovedProjectDiscardsEditor(t *testing.T) {
	n := initialModel(Options{})
	n.api = NewClient("http://daemon.example", http.DefaultClient)
	h := newCronHost(n)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{{ProjectUID: "7", UID: "job", Name: "Protected definition", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)}}}}
	h.selectCronTab(1)
	h.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if h.board.CanLeave() {
		t.Fatal("editor did not open")
	}
	h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request, catalogs: map[int64]cronCatalog{}})
	h.updateCronBoard()
	if !h.board.CanLeave() {
		t.Fatal("removed project retained protected editor")
	}
}

type monitorHostActionService struct{ cronui.Service }

func (monitorHostActionService) Pause(j records.Job) (records.Job, error) { return j, nil }

func TestCronDeniedProjectRejectsQueuedChange(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	j := records.Job{ProjectUID: "7", UID: "job", Name: "Protected definition", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{j}}}
	h.selectCronTab(1)
	h.board.SetServices(map[string]cronui.Service{"7": monitorHostActionService{}}, false, nil)
	_, cmd := h.board.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("pause did not start")
	}
	_, changed := h.board.Update(cmd())
	if changed == nil {
		t.Fatal("pause emitted no change")
	}
	msg := changed()
	h.denyCronProject(7, "No cron permission")
	h.updateCronChild(msg)
	if len(h.catalogs[7].jobs) != 0 {
		t.Fatal("queued change restored denied project definition")
	}
}

func TestCronUnboundIssueBackReturnsProjectPicker(t *testing.T) {
	n := initialModel(Options{})
	n.view = viewProjects
	h := newCronHost(n)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.Update(cronIssueMsg{epoch: h.epoch, request: h.issueRequest, project: 7, detail: &IssueDetail{Issue: &Issue{UID: "issue", ShortID: "abcd", ProjectID: 7}}})
	_, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		h.Update(cmd())
	}
	if h.native.view != viewProjects {
		t.Fatal("linked issue back lost project picker", h.native.view)
	}
}

func TestCronCtrlCFromProjectChooserQuits(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.selectCronTab(1)
	h.board.SetData(cronui.Data{Projects: map[string]cronui.Data{"7": {Project: "first"}, "8": {Project: "second"}}})
	h.board.SetServices(map[string]cronui.Service{"7": monitorHostActionService{}, "8": monitorHostActionService{}}, false, nil)
	h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if h.board.CanLeave() {
		t.Fatal("chooser did not open")
	}
	_, quit := h.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if quit == nil {
		t.Fatal("chooser swallowed ctrl+c")
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c did not reach immediate native quit")
	}
}

type monitorHostConflictService struct{ cronui.Service }

func (monitorHostConflictService) Pause(j records.Job) (records.Job, error) {
	return records.Job{}, &records.ConflictError{Pending: j, Newer: j, Cause: errors.New("conflict")}
}
func TestCronConflictQUsesQuitOverlay(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.selectCronTab(1)
	j := records.Job{ProjectUID: "7", UID: "job", Name: "Example"}
	h.board.SetData(cronui.Data{Jobs: []records.Job{j}})
	h.board.SetServices(map[string]cronui.Service{"7": monitorHostConflictService{}}, false, nil)
	_, cmd := h.board.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("pause did not start")
	}
	h.board.Update(cmd())
	if h.board.CanLeave() {
		t.Fatal("conflict did not open")
	}
	h.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if h.native.modal != modalQuitConfirm {
		t.Fatal("conflict swallowed advertised quit")
	}
}

func TestCronWorkflowAcknowledgementSurvivesRefreshFailure(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronTestAPI{}
	old := records.Workflow{ProjectUID: "7", UID: "flow", Name: "Example", DefinitionEventUID: "old"}
	saved := old
	saved.DefinitionEventUID = "acknowledged"
	saved.Definition = []byte(`{"version":1,"steps":[{"key":"read","kind":"command","command":"echo updated"}]}`)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", workflows: []records.Workflow{old}}}
	h.updateCronChild(cronui.ChangedMsg{SavedWorkflow: &saved, ProjectKey: "7"})
	h.acceptCronCatalog(cronCatalogMsg{epoch: h.epoch, request: h.request, err: errors.New("refresh unavailable")})
	if got := h.catalogs[7].workflows[0]; got.DefinitionEventUID != saved.DefinitionEventUID || string(got.Definition) != string(saved.Definition) {
		t.Fatal("refresh failure lost acknowledged workflow", got)
	}
}

func TestCronLinkedIssueDoesNotInterceptProjectSelection(t *testing.T) {
	n := initialModel(Options{})
	n.view = viewProjects
	h := newCronHost(n)
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	h.Update(cronIssueMsg{epoch: h.epoch, request: h.issueRequest, project: 7, detail: &IssueDetail{Issue: &Issue{UID: "issue", ProjectID: 7}}})
	h.native.view = viewProjects
	h.native.projectsByID = map[int64]string{7: "example-project"}
	h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if h.native.view != viewList || !h.native.scope.allProjects {
		t.Fatal("linked issue return intercepted explicit project selection")
	}
}

type monitorHeadRunAPI struct {
	cronTestAPI
	calls int
}

func (a *monitorHeadRunAPI) ListCronRuns(context.Context, int64, string, string) (records.RunPage, error) {
	a.calls++
	if a.calls == 1 {
		return records.RunPage{Runs: []records.Run{{UID: "obsolete", Status: "running"}}}, nil
	}
	return records.RunPage{Runs: []records.Run{{UID: "fresh", Status: "succeeded"}}}, nil
}
func TestCronEveryTabLoadsMissingHistory(t *testing.T) {
	for _, tab := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(tab), func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.native.api = &monitorRunAPI{}
			h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
			h.dirty = false
			cmd := h.selectCronTab(tab)
			if !h.runPending[cronRunKey{project: 7}] {
				t.Fatal("tab did not request unloaded history")
			}
			msg := cmd()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, child := range batch {
					if result, ok := child().(cronRunsMsg); ok {
						h.acceptCronRuns(result)
					}
				}
			} else if result, ok := msg.(cronRunsMsg); ok {
				h.acceptCronRuns(result)
			}
			if !h.catalogs[7].runsLoaded {
				t.Fatal("tab history was not accepted")
			}
		})
	}
}
func TestCronForgottenProjectFencesIssueJump(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronReviewAPI{}
	h.tab = 1
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project"}}
	pending := h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "issue"})
	late := pending()
	h.forgetCronProject(7)
	h.catalogs[7] = cronCatalog{name: "example-project"}
	h.Update(late)
	if h.tab != 1 || h.native.view == viewDetail {
		t.Fatal("discarded project issue reply opened protected content")
	}
}
func TestCronIssueReplyCannotHideNewEditor(t *testing.T) {
	for _, key := range []string{"e", "D"} {
		t.Run(key, func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.native.api = &cronReviewAPI{}
			j := records.Job{ProjectUID: "7", UID: "job", Name: "Example", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)}
			h.tab = 1
			h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{j}}}
			h.updateCronBoard()
			h.board.SetServices(map[string]cronui.Service{"7": monitorHostActionService{}}, false, nil)
			pending := h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "issue"})
			late := pending()
			h.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
			if h.board.CanLeave() {
				t.Fatal("editor or confirmation did not open")
			}
			h.Update(late)
			if h.tab != 1 || h.native.view == viewDetail {
				t.Fatal("late issue reply hid active input")
			}
		})
	}
}

func TestCronQueuedIssueIntentRespectsNavigation(t *testing.T) {
	for _, switchTabs := range []bool{false, true} {
		t.Run(fmt.Sprint(switchTabs), func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.native.api = &cronReviewAPI{}
			h.tab = 1
			j := records.Job{ProjectUID: "7", UID: "job", Name: "Example", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"issue-scheduled","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"},"action":{"kind":"notify","recipient":"operator","message":"Attention"},"overlap":"forbid","catchup":"latest"}`)}
			h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{j}, runsLoaded: true}}
			h.dirty = false
			h.updateCronBoard()
			h.board.SetActive(false) // Keep animation ticks out of this command-ordering test.
			_, queued := h.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
			if queued == nil {
				t.Fatal("issue intent did not queue")
			}
			if switchTabs {
				h.selectCronTab(3)
			}
			_, lookup := h.Update(queued())
			if lookup != nil {
				h.Update(lookup())
			}
			if switchTabs {
				if h.tab != 3 || h.native.view == viewDetail {
					t.Fatal("queued issue intent overrode newer tab selection")
				}
			} else if h.tab != 0 || h.native.view != viewDetail {
				t.Fatal("current issue intent did not open detail")
			}
		})
	}
}

func TestCronBackgroundHistoryPreservesIssueJump(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronReviewAPI{}
	h.tab = 1
	j := records.Job{ProjectUID: "7", UID: "job", Name: "Example", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{j}, runsLoaded: true}}
	h.dirty = false
	h.updateCronBoard()
	h.board.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if _, ok := h.board.ActiveDetail(); !ok {
		t.Fatal("job detail did not open")
	}
	pending := h.openCronIssue(cronui.IssueMsg{ProjectKey: "7", UID: "issue"})
	reply := pending()
	h.Update(cronRunsMsg{epoch: h.epoch, key: cronRunKey{project: 7}, evidence: h.catalogs[7].evidence})
	h.Update(reply)
	if h.tab != 0 || h.native.view != viewDetail {
		t.Fatal("background history cancelled current issue jump")
	}
}

func TestCronDeferredNativeInputRespectsTabSwitch(t *testing.T) {
	for _, switchTabs := range []bool{false, true} {
		t.Run(fmt.Sprint(switchTabs), func(t *testing.T) {
			n := initialModel(Options{})
			n.scope.projectID = 7
			h := newCronHost(n)
			_, queued := h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
			if queued == nil {
				t.Fatal("native new-issue command did not queue")
			}
			if switchTabs {
				h.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
			}
			h.Update(queued())
			if switchTabs {
				if h.native.input.kind != inputNone {
					t.Fatal("deferred native form opened behind cron tab")
				}
				h.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
				if h.native.modal != modalQuitConfirm {
					t.Fatal("cron quit was captured by hidden native input")
				}
			} else if h.native.input.kind == inputNone {
				t.Fatal("current native input command was discarded")
			}
		})
	}
}

func TestCronEventsKeepAcceptedHistoryAndCoalesceReads(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &monitorHeadRunAPI{}
	h.native.projectUIDByID = map[int64]string{7: "first-project", 8: "second-project"}
	h.catalogs = map[int64]cronCatalog{7: {name: "first", runs: []records.Run{{UID: "accepted"}}}, 8: {name: "second", runs: []records.Run{{UID: "sibling"}}, runsLoaded: true}}
	pending := h.loadCronRuns("", 7, false)
	for range 3 {
		h.invalidateCron("first-project")
	}
	if len(h.catalogs[7].runs) != 1 || h.catalogs[7].runs[0].UID != "accepted" {
		t.Fatal("refresh hid accepted history")
	}
	if !h.catalogs[8].runsLoaded || h.catalogs[8].runs[0].UID != "sibling" {
		t.Fatal("project event invalidated unrelated history")
	}
	h.acceptCronRuns(pending().(cronRunsMsg))
	if len(h.catalogs[7].runs) != 1 || h.catalogs[7].runs[0].UID != "obsolete" || h.catalogs[7].runsLoaded {
		t.Fatal("in-flight read did not make progress with follow-up queued")
	}
	fresh := h.loadCronRuns("", 7, false)
	if fresh == nil {
		t.Fatal("intervening events did not queue follow-up")
	}
	h.acceptCronRuns(fresh().(cronRunsMsg))
	if got := h.catalogs[7]; !got.runsLoaded || len(got.runs) != 1 || got.runs[0].UID != "fresh" {
		t.Fatal("fresh head did not replace invalidated history", got.runs)
	}
}

func TestCronQueuedTabRespectsForeground(t *testing.T) {
	for _, action := range []string{"unchanged", "editor", "newer-tab"} {
		t.Run(action, func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.tab = 1
			j := records.Job{ProjectUID: "7", UID: "job", Name: "Example", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"work"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)}
			h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{j}}}
			h.updateCronBoard()
			h.board.SetActive(false)
			h.board.SetServices(map[string]cronui.Service{"7": monitorHostActionService{}}, false, nil)
			_, queued := h.Update(tea.KeyPressMsg{Code: 'K', Text: "K"})
			if queued == nil {
				t.Fatal("tab intent did not queue")
			}
			switch action {
			case "editor":
				h.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
				if h.board.CanLeave() {
					t.Fatal("editor did not open")
				}
			case "newer-tab":
				h.selectCronTab(3)
			}
			h.Update(queued())
			want := 0
			switch action {
			case "editor":
				want = 1
			case "newer-tab":
				want = 3
			}
			if h.tab != want {
				t.Fatal("queued tab intent ignored foreground ownership", action, h.tab)
			}
		})
	}
}

func TestCronWorkflowLaunchRespectsEditorLifetime(t *testing.T) {
	for _, state := range []string{"active", "closing", "closed"} {
		t.Run(state, func(t *testing.T) {
			h := newCronHost(initialModel(Options{}))
			h.tab = 3
			w := records.Workflow{ProjectUID: "7", UID: "flow", Name: "Example", Definition: []byte(`{"version":1,"steps":[{"key":"read","kind":"command","command":"true"}]}`)}
			h.catalogs = map[int64]cronCatalog{7: {name: "example-project", workflows: []records.Workflow{w}}}
			h.updateCronBoard()
			t.Cleanup(func() { _ = h.board.Close() })
			h.board.SetActive(false)
			h.board.SetServices(map[string]cronui.Service{"7": monitorHostActionService{}}, false, nil)
			_, queued := h.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
			if queued == nil {
				t.Fatal("workflow editor launch did not queue")
			}
			if state != "active" {
				_, cancel := h.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				if cancel == nil {
					t.Fatal("workflow cancellation did not queue")
				}
				if state == "closed" {
					h.Update(cancel())
				}
				if state == "closed" && !h.board.CanLeave() {
					t.Fatal("workflow editor did not close")
				}
			}
			_, launch := h.Update(queued())
			if state != "active" && launch != nil {
				t.Fatal("cancelled editor launch remained executable")
			}
			if state == "active" && launch == nil {
				t.Fatal("current editor launch was discarded")
			}

		})
	}
}

func TestCronStaleJobHistoryCannotReturnAfterHeadRefresh(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &monitorHeadRunAPI{}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", runsLoaded: true}}
	pending := h.loadCronRuns("job", 7, false)
	old := pending().(cronRunsMsg)
	h.resetCronEvidence()
	fresh := h.loadCronRuns("", 7, false)
	h.acceptCronRuns(fresh().(cronRunsMsg))
	h.acceptCronRuns(old)
	if len(h.jobPages) != 0 {
		t.Fatal("old job page reintroduced stale evidence after fresh head")
	}
	if got := h.catalogs[7].runs; len(got) != 1 || got[0].UID != "fresh" {
		t.Fatal("fresh head was lost", got)
	}
}
func TestCronPaginationHintRequiresLoadableHistory(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.tab = 2
	h.width, h.height = 100, 30
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", before: "older"}}
	h.updateCronBoard()
	if strings.Contains(h.board.View().Content, "] loads more") {
		t.Fatal("refreshing history advertised unusable pagination")
	}
	c := h.catalogs[7]
	c.runsLoaded = true
	h.catalogs[7] = c
	h.updateCronBoard()
	if !strings.Contains(h.board.View().Content, "] loads more") {
		t.Fatal("loadable history lost pagination hint")
	}
}

type monitorRevisionRunAPI struct{ cronReviewAPI }

func (*monitorRevisionRunAPI) ListCronRuns(context.Context, int64, string, string) (records.RunPage, error) {
	return records.RunPage{Runs: []records.Run{{UID: "observed", Revision: 1, Status: "running"}}}, nil
}
func (*monitorRevisionRunAPI) ShowCronRun(context.Context, int64, string) (records.Run, error) {
	return records.Run{UID: "observed", Revision: 2, Status: "succeeded"}, nil
}
func TestCronOlderHeadPreservesNewerDetailRevision(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &monitorRevisionRunAPI{}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", runs: []records.Run{{UID: "outside-head", Revision: 9}}}}
	pending := h.loadCronRuns("", 7, false)
	old := pending().(cronRunsMsg)
	detail := h.cronDetail(cronui.DetailMsg{Kind: "run", ProjectKey: "7", UID: "observed"})
	h.Update(detail())
	h.acceptCronRuns(old)
	got := h.catalogs[7].runs
	if len(got) != 1 || got[0].UID != "observed" || got[0].Revision != 2 || got[0].Status != "succeeded" {
		t.Fatal("head membership or newest detail revision lost", got)
	}
}

func monitorIssueNavigationHost() *cronHost {
	h := newCronHost(initialModel(Options{}))
	h.native.api = &cronReviewAPI{}
	h.tab = 1
	j := records.Job{ProjectUID: "7", UID: "job", Name: "Example", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"issue-scheduled","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"},"action":{"kind":"notify","recipient":"operator","message":"Attention"},"overlap":"forbid","catchup":"latest"}`)}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", jobs: []records.Job{j}, runsLoaded: true}}
	h.dirty = false
	h.updateCronBoard()
	h.board.SetActive(false)
	return h
}
func TestCronDelayedDetailNotificationPreservesNewerIssueIntent(t *testing.T) {
	h := monitorIssueNavigationHost()
	_, detail := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if detail == nil {
		t.Fatal("job detail notification did not queue")
	}
	_, issue := h.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if issue == nil {
		t.Fatal("issue intent did not queue")
	}
	_, lookup := h.Update(issue())
	if lookup == nil {
		t.Fatal("issue lookup did not start")
	}
	h.Update(detail())
	h.Update(lookup())
	if h.tab != 0 || h.native.view != viewDetail {
		t.Fatal("older detail notification cancelled newer issue intent")
	}
}
func TestCronSuccessfulIssueJumpFencesOlderTabIntent(t *testing.T) {
	h := monitorIssueNavigationHost()
	_, tab := h.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if tab == nil {
		t.Fatal("tab intent did not queue")
	}
	_, issue := h.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	_, lookup := h.Update(issue())
	h.Update(lookup())
	if h.tab != 0 || h.native.view != viewDetail {
		t.Fatal("issue jump did not open")
	}
	h.Update(tab())
	if h.tab != 0 || h.native.view != viewDetail {
		t.Fatal("older tab intent overrode completed issue jump")
	}
}

type monitorDetailCountAPI struct {
	cronReviewAPI
	calls int
}

func (a *monitorDetailCountAPI) ShowCronRun(context.Context, int64, string) (records.Run, error) {
	a.calls++
	return records.Run{UID: "observed", Revision: int64(a.calls), Status: "succeeded"}, nil
}
func TestCronRunDetailRefreshCoalescesPendingReads(t *testing.T) {
	h := newCronHost(initialModel(Options{}))
	api := &monitorDetailCountAPI{}
	h.native.api = api
	h.tab, h.width, h.height = 2, 100, 30
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", runsLoaded: true, runs: []records.Run{{UID: "observed", ProjectKey: "7", Status: "running"}}}}
	h.dirty = false
	h.updateCronBoard()
	h.board.SetActive(false)
	h.board.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pending := h.loadVisibleCronDetail()
	if pending == nil {
		t.Fatal("detail request did not start")
	}
	for range 5 {
		if h.loadVisibleCronDetail() != nil {
			t.Fatal("duplicate detail refresh was not coalesced")
		}
	}
	_, follow := h.Update(pending())
	if api.calls != 1 || h.catalogs[7].runs[0].Revision != 1 || follow == nil {
		t.Fatal("first detail read was discarded or follow-up lost")
	}
	_, next := h.Update(follow())
	if api.calls != 2 || h.catalogs[7].runs[0].Revision != 2 || next != nil {
		t.Fatal("detail invalidations did not coalesce to one follow-up", api.calls)
	}
}

type monitorCatalogRefreshAPI struct{ cronTestAPI }

func (*monitorCatalogRefreshAPI) ListProjects(context.Context) ([]ProjectSummary, error) {
	return []ProjectSummary{{ID: 7, Name: "first-project"}, {ID: 8, Name: "second-project"}}, nil
}
func (*monitorCatalogRefreshAPI) ListCronJobs(_ context.Context, project int64) ([]records.Job, error) {
	return []records.Job{{ProjectUID: fmt.Sprint(project), UID: fmt.Sprintf("job-%d", project), Name: "Fresh definition"}}, nil
}
func TestCronProjectDenialPreservesSiblingCatalogRefresh(t *testing.T) {
	n := initialModel(Options{})
	n.scope = scope{allProjects: true}
	n.api = &monitorCatalogRefreshAPI{}
	h := newCronHost(n)
	h.tab = 1
	h.catalogs = map[int64]cronCatalog{7: {name: "first-project", jobs: []records.Job{{UID: "protected"}}}, 8: {name: "second-project", jobs: []records.Job{{UID: "job-8", Name: "Old definition"}}}}
	pending := h.loadCronCatalog()
	if pending == nil {
		t.Fatal("catalog refresh did not start")
	}
	h.denyCronProject(7, "No cron permission")
	h.Update(pending())
	if len(h.catalogs[7].jobs) != 0 {
		t.Fatal("older catalog restored denied project")
	}
	if got := h.catalogs[8].jobs; len(got) != 1 || got[0].Name != "Fresh definition" {
		t.Fatal("project denial discarded sibling refresh", got)
	}
}
func TestCronSelectedProjectRefreshUsesRenamedProject(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":7,"name":"renamed-project"}]}`))
		case strings.Contains(r.URL.Path, "/cron/jobs"):
			_, _ = w.Write([]byte(`{"jobs":[]}`))
		case strings.Contains(r.URL.Path, "/cron/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[]}`))
		case strings.Contains(r.URL.Path, "/issues/"):
			_, _ = w.Write([]byte(`{"issue":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","short_id":"abcd","project_id":7}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c := NewClient(s.URL, s.Client())
	n := initialModel(Options{})
	n.scope = scope{projectID: 7, projectName: "original-project"}
	n.api = c
	h := newCronHost(n)
	h.tab = 1
	pending := h.loadCronCatalog()
	h.acceptCronCatalog(pending().(cronCatalogMsg))
	if h.catalogs[7].name != "renamed-project" {
		t.Fatal("selected catalog retained old project name", h.catalogs[7].name)
	}
	service := cronProjectClient{client: c, projectID: 7, project: h.catalogs[7].name}
	if _, err := service.ShowIssue(t.Context(), "renamed-project#abcd"); err != nil {
		t.Fatal("renamed project reference rejected", err)
	}
}
