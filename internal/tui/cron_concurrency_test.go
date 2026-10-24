package tui

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type boundedCronAPI struct {
	cronTestAPI
	active, peak atomic.Int64
	release      chan struct{}
}

func (a *boundedCronAPI) enter() {
	n := a.active.Add(1)
	for old := a.peak.Load(); n > old; old = a.peak.Load() {
		if a.peak.CompareAndSwap(old, n) {
			break
		}
	}
	<-a.release
	a.active.Add(-1)
}
func (a *boundedCronAPI) ListProjects(context.Context) ([]ProjectSummary, error) {
	var projects []ProjectSummary
	for id := int64(1); id <= 8; id++ {
		projects = append(projects, ProjectSummary{ID: id, Name: "example-project"})
	}
	return projects, nil
}
func (a *boundedCronAPI) ListCronJobs(context.Context, int64) ([]records.Job, error) {
	a.enter()
	return nil, nil
}
func (a *boundedCronAPI) ListCronWorkflows(context.Context, int64) ([]records.Workflow, error) {
	a.enter()
	return nil, nil
}
func (a *boundedCronAPI) ListCronRuns(context.Context, int64, string, string) (records.RunPage, error) {
	a.enter()
	return records.RunPage{}, nil
}

func (a *boundedCronAPI) ShowCronRun(_ context.Context, _ int64, uid string) (records.Run, error) {
	a.enter()
	return records.Run{UID: uid}, nil
}

func TestCronRequestsShareFourSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &boundedCronAPI{release: make(chan struct{})}
		m := initialModel(Options{})
		m.api = api
		m.scope = scope{allProjects: true}
		h := newCronHost(m)
		h.tab = 2
		h.catalogs = map[int64]cronCatalog{1: {name: "first-project"}, 2: {name: "second-project"}, 3: {name: "third-project"}, 4: {name: "fourth-project"}}
		catalog := h.loadCronCatalog()
		runs := h.loadCronRuns("", 0, false)
		go func() { catalog() }()
		synctest.Wait()
		go executeCronBatch(runs)
		for id := int64(1); id <= 4; id++ {
			detail := h.cronDetail(cronui.DetailMsg{Kind: "run", ProjectKey: fmt.Sprint(id), UID: "run"})
			go executeCronBatch(detail)
		}
		synctest.Wait()
		if peak := api.peak.Load(); peak != 4 {
			t.Errorf("request bound = %d, want four shared slots", peak)
		}
		close(api.release)
		synctest.Wait()
		if api.active.Load() != 0 {
			t.Fatal("request did not finish")
		}
	})
}
func executeCronBatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			go executeCronBatch(c)
		}
	}
}
