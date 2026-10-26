package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type cronRunKey struct {
	project int64
	job     string
}
type cronRunsMsg struct {
	epoch, request uint64
	evidence       uint64
	history        bool
	key            cronRunKey
	page           records.RunPage
	err            error
}
type cronDetailFlight struct {
	uid     string
	request uint64
	again   bool
}
type cronRunDetailMsg struct {
	epoch   uint64
	request uint64
	project int64
	run     records.Run
	err     error
}
type cronIssueMsg struct {
	generation uint64
	epoch      uint64
	request    uint64
	project    int64
	target     int64
	detail     *IssueDetail
	err        error
}

func (h *cronHost) loadCronRuns(job string, project int64, more bool) tea.Cmd {
	api := cronAPIFor(h.native.api)
	if api == nil {
		return nil
	}
	if h.runRequests == nil {
		h.runRequests = map[cronRunKey]uint64{}
	}
	if h.runPending == nil {
		h.runPending = map[cronRunKey]bool{}
	}
	if h.jobPages == nil {
		h.jobPages = map[cronRunKey]records.RunPage{}
	}
	cmds := []tea.Cmd{}
	// A single command executes bounded project requests; the messages are still
	// delivered separately so one denied project cannot hide accessible siblings.
	sem := h.slots
	for id, cat := range h.catalogs {
		if project != 0 && id != project || cat.problem != "" {
			continue
		}
		key := cronRunKey{id, job}
		if h.runPending[key] {
			continue
		}
		before := ""
		if job == "" {
			if cat.runsLoaded && !more {
				continue
			}
			if more {
				if !cat.runsLoaded {
					continue
				}
				before = cat.before
				if before == "" {
					continue
				}
			}
		} else if _, loaded := h.jobPages[key]; loaded && h.jobPageEvidence[key] == cat.evidence && !more {
			continue
		}
		h.runRequests[key]++
		h.runPending[key] = true
		epoch, request, evidence := h.epoch, h.runRequests[key], cat.evidence
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
			defer cancel()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return cronRunsMsg{epoch: epoch, request: request, evidence: evidence, history: more, key: key, err: ctx.Err()}
			}
			defer func() { <-sem }()
			page, err := api.ListCronRuns(ctx, key.project, key.job, before)
			return cronRunsMsg{epoch: epoch, request: request, evidence: evidence, history: more, key: key, page: page, err: err}
		})
	}
	return tea.Batch(cmds...)
}
func (h *cronHost) acceptCronRuns(msg cronRunsMsg) {
	if msg.epoch != h.epoch || h.runRequests[msg.key] != msg.request {
		return
	}
	delete(h.runPending, msg.key)
	cat, ok := h.catalogs[msg.key.project]
	if !ok {
		return
	}
	if msg.err != nil {
		var ae *APIError
		if records.AuthRejected(msg.err) {
			h.clearCronAccess(msg.err)
			return
		}
		if errors.As(msg.err, &ae) && ae.Status == 403 {
			h.denyCronProject(msg.key.project, cronProblemText(msg.err))
			return
		}
		if msg.evidence != cat.evidence {
			// A failed older read must not block the refresh queued after it.
			// Authentication and permission failures above still revoke access.
			return
		}
		cat.problem = cronProblemText(msg.err)
		h.catalogs[msg.key.project] = cat
		return
	}
	if (msg.history || msg.key.job != "") && msg.evidence != cat.evidence {
		return
	}

	// Keep authoritative latest evidence and accepted revisions consistent across
	// catalog, history and detail without changing page membership or cursors.
	for i := range msg.page.Runs {
		msg.page.Runs[i] = h.reconcileCronRun(msg.key.project, msg.page.Runs[i])
	}
	cat = h.catalogs[msg.key.project]
	if msg.key.job != "" {
		if h.jobPages == nil {
			h.jobPages = map[cronRunKey]records.RunPage{}
		}
		h.jobPages[msg.key] = msg.page
		if h.jobPageEvidence == nil {
			h.jobPageEvidence = map[cronRunKey]uint64{}
		}
		h.jobPageEvidence[msg.key] = msg.evidence
		return
	}
	if msg.history {
		cat.runs = mergeCronRuns(cat.runs, msg.page.Runs)
	} else {
		cat.runs = append([]records.Run(nil), msg.page.Runs...)
	}
	cat.before = msg.page.NextBeforeUID
	cat.runsLoaded = msg.evidence == cat.evidence
	if !msg.history && cat.runsLoaded {
		for key := range h.jobPages {
			if key.project == msg.key.project && h.jobPageEvidence[key] != cat.evidence {
				delete(h.jobPages, key)
				delete(h.jobPageEvidence, key)
			}
		}
	}
	h.catalogs[msg.key.project] = cat
}

// reconcileCronRun preserves higher accepted revisions within one project.
func (h *cronHost) reconcileCronRun(project int64, run records.Run) records.Run {
	cat, ok := h.catalogs[project]
	if !ok {
		return run
	}
	consider := func(old records.Run) {
		if old.UID == run.UID && old.Revision > run.Revision {
			run = old
		}
	}
	for _, old := range cat.runs {
		consider(old)
	}
	for _, job := range cat.jobs {
		if job.LastRun != nil {
			consider(*job.LastRun)
		}
	}
	for key, page := range h.jobPages {
		if key.project == project {
			for _, old := range page.Runs {
				consider(old)
			}
		}
	}
	run.ProjectID, run.ProjectKey, run.ProjectName = project, fmt.Sprint(project), cat.name
	for i := range cat.runs {
		if cat.runs[i].UID == run.UID {
			cat.runs[i] = run
		}
	}
	for i := range cat.jobs {
		if last := cat.jobs[i].LastRun; last != nil && last.UID == run.UID {
			cat.jobs[i].LastRun = &run
		}
	}
	for key, page := range h.jobPages {
		if key.project == project {
			for i := range page.Runs {
				if page.Runs[i].UID == run.UID {
					page.Runs[i] = run
				}
			}
		}
	}
	h.catalogs[project] = cat
	return run
}

func mergeCronRuns(old, in []records.Run) []records.Run {
	out := append([]records.Run(nil), old...)
	index := map[string]int{}
	for i, r := range out {
		index[r.UID] = i
	}
	for _, r := range in {
		if i, ok := index[r.UID]; ok {
			if r.Revision >= out[i].Revision {
				out[i] = r
			}
		} else {
			index[r.UID] = len(out)
			out = append(out, r)
		}
	}
	return out
}
func (h *cronHost) updateCronBoard() {
	if h.board == nil {
		h.board = cronui.New(cronui.Options{Mouse: h.native.opts.Mouse})
	}
	if h.background != nil {
		h.board.Update(*h.background)
	}
	d := cronui.Data{Projects: map[string]cronui.Data{}}
	ids := make([]int64, 0, len(h.catalogs))
	for id := range h.catalogs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := h.catalogs[id]
		key := fmt.Sprint(id)
		d.Projects[key] = cronui.Data{Project: c.name}
		d.Workflows = append(d.Workflows, c.workflows...)
		runs := c.runs
		for k, page := range h.jobPages {
			if k.project == id {
				runs = mergeCronRuns(runs, page.Runs)
			}
		}
		// Job reads provide authoritative latest runs even when a job's last
		// execution falls outside the loaded history page. Merge only within
		// this project, retaining newer accepted revisions and page cursors.
		lastRuns := make([]records.Run, 0, len(c.jobs))
		for _, job := range c.jobs {
			if job.LastRun != nil {
				run := *job.LastRun
				run.ProjectID, run.ProjectKey, run.ProjectName = id, key, c.name
				job.LastRun = &run
				lastRuns = append(lastRuns, run)
			}
			d.Jobs = append(d.Jobs, job)
		}
		if len(lastRuns) > 0 {
			runs = mergeCronRuns(runs, lastRuns)
		}
		d.Runs = append(d.Runs, runs...)
		d.MoreRuns = d.MoreRuns || c.runsLoaded && c.before != ""
		for _, problem := range []string{c.problem, c.detailProblem} {
			if problem != "" {
				d.Problems = append(d.Problems, c.name+": "+problem)
			}
		}
	}
	if h.cronProblem != "" {
		d.Problems = append(d.Problems, h.cronProblem)
	}
	if h.loading && len(h.catalogs) == 0 {
		d.Problems = append(d.Problems, "Loading cron definitions…")
	}
	services := map[string]cronui.Service{}
	var client *Client
	switch api := h.native.api.(type) {
	case *Client:
		client = api
	case *undoClient:
		client, _ = api.KataAPI.(*Client)
	}
	if client != nil {
		for _, id := range ids {
			if strings.HasPrefix(h.catalogs[id].problem, "No cron permission") || strings.HasPrefix(h.catalogs[id].problem, "Cron capability unavailable") {
				continue
			}
			services[fmt.Sprint(id)] = cronProjectClient{client: client, projectID: id, project: h.catalogs[id].name, actor: h.native.list.actor}
		}
	}
	readOnly := h.cronDenied || h.cronScope.forbidden || h.native.authCapabilitiesRequired && !h.native.authCapabilitiesReady
	switch h.native.activeAuth.Kind {
	case "insecure_readonly", "bootstrap", "trusted_proxy_absent":
		readOnly = true
	}
	epoch := h.epoch
	h.board.SetServices(services, readOnly, func(msg tea.Msg) tea.Msg { return cronChildMsg{epoch, msg} })
	h.board.SetActive(h.tab > 0)
	h.board.SetData(d)
	h.resizeCronBoard()
	if h.tab > 0 {
		h.board.SelectTab(h.tab - 1)
	}
}
func (h *cronHost) selectCronTab(tab int) tea.Cmd {
	if h.tab == 0 && tab > 0 {
		h.native.inboxPending = false
	}
	h.tabGeneration++
	h.issueRequest++
	h.tab = tab
	h.updateCronBoard()
	var runs tea.Cmd
	if tab > 0 {
		runs = h.loadCronRuns("", 0, false)
	}
	return tea.Batch(h.loadCronCatalog(), runs, h.loadVisibleCronDetail(), h.wakeCron())
}
func (h *cronHost) openCronIssue(v cronui.IssueMsg) tea.Cmd {
	if v.UID == "" || h.board == nil || !h.board.CanLeave() || h.native.modal != modalNone {
		return nil
	}
	id, err := strconv.ParseInt(v.ProjectKey, 10, 64)
	if err != nil {
		return nil
	}
	if !h.cronProjectAccessible(id) {
		return nil
	}
	// A peek that followed a move knows the issue's current project.
	target := id
	if h.peekKey.project == id && h.peekKey.issueUID == v.UID && h.peekProject > 0 {
		target = h.peekProject
	}
	h.issueRequest++
	request, generation := h.issueRequest, h.issueGenerations[id]
	api, epoch := h.native.api, h.epoch
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		detail, err := api.GetIssueDetail(ctx, target, v.UID)
		return cronIssueMsg{generation: generation, epoch: epoch, request: request, project: id, target: target, detail: detail, err: err}
	}
}
func (h *cronHost) cronDetail(v cronui.DetailMsg) tea.Cmd {
	id, err := strconv.ParseInt(v.ProjectKey, 10, 64)
	if err != nil {
		return nil
	}
	if v.Kind == "job" {
		return h.loadCronRuns(v.UID, id, false)
	}
	if v.Kind != "run" || !h.cronProjectAccessible(id) {
		return nil
	}
	api, epoch := cronAPIFor(h.native.api), h.epoch
	if api == nil {
		return nil
	}
	if pending, ok := h.detailPending[id]; ok && pending.uid == v.UID {
		pending.again = true
		h.detailPending[id] = pending
		return nil
	}
	if h.detailRequests == nil {
		h.detailRequests = map[int64]uint64{}
	}
	if h.detailPending == nil {
		h.detailPending = map[int64]cronDetailFlight{}
	}
	h.detailRequests[id]++
	request := h.detailRequests[id]
	h.detailPending[id] = cronDetailFlight{uid: v.UID, request: request}
	sem := h.slots
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return cronRunDetailMsg{epoch: epoch, request: request, project: id, err: ctx.Err()}
		}
		defer func() { <-sem }()
		run, err := api.ShowCronRun(ctx, id, v.UID)
		return cronRunDetailMsg{epoch: epoch, request: request, project: id, run: run, err: err}
	}
}

func (h *cronHost) loadVisibleCronDetail() tea.Cmd {
	if h.tab == 0 || h.board == nil {
		return nil
	}
	detail, ok := h.board.ActiveDetail()
	if !ok {
		return nil
	}
	// Job history has its own in-flight fence and can refresh while a queued
	// catalog refresh is running. Single-run detail still waits for that catalog.
	if detail.Kind != "job" && (h.loading || h.debounce || h.dirty) {
		return nil
	}
	id, err := strconv.ParseInt(detail.ProjectKey, 10, 64)
	if err != nil {
		return nil
	}
	cat, ok := h.catalogs[id]
	if !ok || cat.problem != "" {
		return nil
	}
	return h.cronDetail(detail)
}

func (h *cronHost) cronProjectAccessible(project int64) bool {
	cat, ok := h.catalogs[project]
	return ok && !h.cronDenied && !h.cronScope.forbidden && !strings.HasPrefix(cat.problem, "No cron permission") && !strings.HasPrefix(cat.problem, "Cron capability unavailable")
}
