package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/kata/internal/tui/cron/records"
)

type cronScope struct {
	conn, scope           uint64
	project               int64
	all, empty, forbidden bool
	ready                 bool
}
type cronCatalog struct {
	name, uid  string
	jobs       []records.Job
	workflows  []records.Workflow
	runs       []records.Run
	before     string
	runsLoaded bool
	evidence   uint64
	problem    string
	err        error
}
type cronCatalogMsg struct {
	generations    map[int64]uint64
	epoch, request uint64
	catalogs       map[int64]cronCatalog
	err            error
}
type cronDebounceMsg struct{ epoch uint64 }

func (h *cronHost) syncCronScope() {
	n := h.native
	stamp := cronScope{conn: n.connGen, scope: n.scope.scopeGen, project: n.scope.projectID, all: n.scope.allProjects, empty: n.scope.empty, forbidden: n.issueScoped || n.activeAuthRejected || n.activeAuth.ExpiresAt != nil && !n.toastNow().Before(*n.activeAuth.ExpiresAt), ready: n.authCapabilitiesReady}
	stamp.all = stamp.all || stamp.project == 0 && !stamp.empty
	if stamp == h.cronScope {
		return
	}
	h.cronScope = stamp
	h.cronDenied = false
	h.epoch++
	if h.board != nil {
		_ = h.board.Close()
	}
	h.catalogs = nil
	h.board = nil
	h.runRequests = nil
	h.detailRequests = nil
	h.detailPending = nil
	h.runPending = nil
	h.jobPages = nil
	h.jobPageEvidence = nil
	h.loading = false
	h.dirty = true
	h.debounce = false
	h.cronProblem = ""
	if stamp.forbidden {
		h.cronProblem = "Cron unavailable: this credential has no cron permission"
	}
}
func (h *cronHost) loadCronCatalog() tea.Cmd {
	if h.cronDenied || h.debounce || h.loading || !h.dirty || h.tab == 0 || h.cronScope.forbidden {
		return nil
	}
	if h.native.authCapabilitiesRequired && !h.native.authCapabilitiesReady {
		return nil
	}
	api := cronAPIFor(h.native.api)
	if api == nil {
		h.cronProblem = "Cron unavailable: daemon client has no cron capability"
		return nil
	}
	projectsAPI := h.native.api
	slots := h.slots
	sc, epoch := h.native.scope, h.epoch
	h.loading = true
	h.dirty = false
	h.request++
	request := h.request
	generations := maps.Clone(h.issueGenerations)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		projects := []ProjectSummary{}
		if sc.projectID > 0 && !sc.allProjects {
			current, err := projectsAPI.ListProjects(ctx)
			if err == nil {
				for _, p := range current {
					if p.ID == sc.projectID {
						projects = append(projects, p)
						break
					}
				}
			} else if records.AuthRejected(err) {
				return cronCatalogMsg{epoch: epoch, request: request, generations: generations, err: err}
			} else {
				// Cron-only credentials need not have project-catalog permission.
				projects = append(projects, ProjectSummary{ID: sc.projectID, Name: sc.projectName})
			}
		} else if !sc.empty {
			var err error
			projects, err = projectsAPI.ListProjects(ctx)
			if err != nil {
				return cronCatalogMsg{epoch: epoch, request: request, generations: generations, err: err}
			}
		}
		catalogs := make(map[int64]cronCatalog, len(projects))
		var mu sync.Mutex
		sem := slots
		var wg sync.WaitGroup
		for _, project := range projects {
			wg.Add(1)
			go func(p ProjectSummary) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					catalogs[p.ID] = cronCatalog{name: p.Name, uid: p.UID, problem: "Network error: " + ctx.Err().Error()}
					mu.Unlock()
					return
				}
				defer func() { <-sem }()
				cat := cronCatalog{name: p.Name, uid: p.UID}
				var err error
				cat.jobs, err = api.ListCronJobs(ctx, p.ID)
				if err == nil {
					cat.workflows, err = api.ListCronWorkflows(ctx, p.ID)
				}
				if err != nil {
					cat.problem = cronProblemText(err)
					cat.err = err
				}
				for i := range cat.jobs {
					cat.jobs[i].Project = p.Name
				}
				for i := range cat.workflows {
					cat.workflows[i].Project = p.Name
				}
				mu.Lock()
				catalogs[p.ID] = cat
				mu.Unlock()
			}(project)
		}
		wg.Wait()
		return cronCatalogMsg{epoch: epoch, request: request, generations: generations, catalogs: catalogs}
	}
}
func cronProblemText(err error) string {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		switch apiErr.Status {
		case 401:
			return "Authentication required: " + apiErr.Error()
		case 403:
			return "No cron permission: " + apiErr.Error()
		case 404:
			return "Cron capability unavailable: " + apiErr.Error()
		}
	}
	return "Network error (stale): " + err.Error()
}
func (h *cronHost) acceptCronCatalog(msg cronCatalogMsg) {
	if msg.epoch != h.epoch || msg.request != h.request {
		return
	}
	h.loading = false
	if msg.err != nil {
		if h.clearCronAccess(msg.err) {
			return
		}
		h.cronProblem = cronProblemText(msg.err)
		var apiErr *APIError
		if errors.As(msg.err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
			h.catalogs = nil
		}
		return
	}
	h.cronProblem = ""
	if h.catalogs == nil {
		h.catalogs = map[int64]cronCatalog{}
	}
	for id, cat := range msg.catalogs {
		if msg.generations[id] != h.issueGenerations[id] {
			continue
		}
		if records.AuthRejected(cat.err) {
			h.clearCronAccess(cat.err)
			return
		}
		if strings.HasPrefix(cat.problem, "Authentication required") {
			h.clearCronAccess(&APIError{Status: 401, Message: cat.problem})
			return
		}
		if strings.HasPrefix(cat.problem, "No cron permission") || strings.HasPrefix(cat.problem, "Cron capability unavailable") {
			h.catalogs[id] = cat
			h.denyCronProject(id, cat.problem)
			continue
		}
		if old, ok := h.catalogs[id]; ok {
			if strings.HasPrefix(cat.problem, "Network error") {
				old.problem = cat.problem
				h.catalogs[id] = old
				continue
			}
			cat.runs, cat.before, cat.runsLoaded = old.runs, old.before, old.runsLoaded
			cat.evidence = old.evidence
		}
		h.catalogs[id] = cat
	}
	// An accessible project removed from the catalog must not survive a refresh.
	for id := range h.catalogs {
		if _, ok := msg.catalogs[id]; !ok && msg.generations[id] == h.issueGenerations[id] {
			h.forgetCronProject(id)
			delete(h.catalogs, id)
		}
	}
}
func (h *cronHost) invalidateCron(projectUID string) tea.Cmd {
	var affected int64
	if projectUID != "" {
		for id, uid := range h.native.projectUIDByID {
			if uid == projectUID {
				affected = id
				if !h.cronScope.all && h.cronScope.project != id {
					return nil
				}
				break
			}
		}
		// Unresolved UIDs deliberately invalidate every cached project.
	}
	h.resetCronEvidenceFor(affected)
	if h.tab == 0 || h.debounce {
		return nil
	}
	h.debounce = true
	epoch := h.epoch
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return cronDebounceMsg{epoch} })
}

func (h *cronHost) resetCronEvidence() { h.resetCronEvidenceFor(0) }
func (h *cronHost) resetCronEvidenceFor(project int64) {
	for id, c := range h.catalogs {
		if project != 0 && id != project {
			continue
		}
		c.evidence++
		c.runsLoaded = false
		h.catalogs[id] = c
		if pending, ok := h.detailPending[id]; ok {
			pending.again = true
			h.detailPending[id] = pending
		}
	}
	h.dirty = true
}
func (h *cronHost) refreshCron() tea.Cmd {
	h.resetCronEvidence()
	h.debounce = false
	if h.native.authCapabilitiesRequired && !h.native.authCapabilitiesReady {
		return h.native.fetchAuthCapabilities()
	}
	return h.loadCronCatalog()
}

func (h *cronHost) denyCronProject(id int64, problem string) {
	cat, ok := h.catalogs[id]
	if !ok {
		return
	}
	h.forgetCronProject(id)
	cat.jobs = nil
	cat.workflows = nil
	cat.runs = nil
	cat.before = ""
	cat.runsLoaded = false
	cat.problem = problem
	h.catalogs[id] = cat
}

func (h *cronHost) forgetCronProject(id int64) {
	delete(h.detailPending, id)
	h.issueRequest++
	if h.issueGenerations == nil {
		h.issueGenerations = map[int64]uint64{}
	}
	h.issueGenerations[id]++
	if h.board != nil {
		h.board.ForgetProject(fmt.Sprint(id))
	}
	if h.detailRequests == nil {
		h.detailRequests = map[int64]uint64{}
	}
	h.detailRequests[id]++
	for key := range h.jobPages {
		if key.project == id {
			delete(h.jobPages, key)
			delete(h.jobPageEvidence, key)
		}
	}
	for key := range h.runRequests {
		if key.project == id {
			h.runRequests[key]++
			delete(h.runPending, key)
		}
	}
}

func (h *cronHost) clearCronAccess(err error) bool {
	status := records.Status(err)
	if status != 401 && status != 403 {
		return false
	}
	if h.board != nil {
		_ = h.board.Close()
	}
	h.catalogs = nil
	h.jobPages = nil
	h.jobPageEvidence = nil
	h.board = nil
	h.runRequests = nil
	h.detailRequests = nil
	h.detailPending = nil
	h.runPending = nil
	h.epoch++
	h.loading = false
	h.dirty = false
	h.cronDenied = true
	h.cronProblem = cronProblemText(err)
	if records.AuthRejected(err) {
		h.native.activeAuthRejected = true
		h.native.authCapabilitiesReady = false
	}
	return true
}
