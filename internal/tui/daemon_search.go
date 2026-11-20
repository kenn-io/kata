package tui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/helplayout"
)

const (
	daemonSearchLimit = 50
	// daemonSearchDebounce waits for typing to pause so one question goes to
	// the daemon per burst of keystrokes.
	daemonSearchDebounce = 250 * time.Millisecond
	// A meaning search embeds the query first, which is slower than a list read.
	daemonSearchTimeout = 15 * time.Second
	// daemonSearchReadWorkers bounds the reads in flight at once. A search
	// reads at most daemonSearchLimit issues, so one ask or one recheck costs
	// at most that many requests.
	daemonSearchReadWorkers = 8
)

// rankedSearch is the daemon's ranked answer to the search bar's query and the
// state around asking for it. The zero value is "not asking, nothing to show".
type rankedSearch struct {
	// live is true while the search bar is open with daemon search on: the
	// typed text asks the daemon instead of filtering the loaded rows.
	live bool
	// req identifies the newest ask. An answer for another id is dropped; zero
	// never matches, so zeroing it discards whatever is still in flight.
	req     int64
	pending bool
	// asked is the query sent with req, so a repeated query is not asked twice.
	asked string
	err   error

	hasAnswer bool
	// answered is the query the hits answer.
	answered SearchQuery
	// hits are the answer's issues in the daemon's order, replaced by the
	// list's own copy of an issue whenever a refresh brings a fresher one.
	hits    []Issue
	lexical bool
}

// hitRead is the outcome of reading one hit the loaded list does not hold.
type hitRead struct {
	uid   string
	issue Issue
	// gone is true when the issue was deleted or now lives in another project.
	gone bool
	err  error
}

// missingHitUIDs lists the hits the loaded issues do not hold.
func missingHitUIDs(hits, issues []Issue) []string {
	loaded := make(map[string]struct{}, len(issues))
	for _, iss := range issues {
		loaded[iss.UID] = struct{}{}
	}
	var uids []string
	for _, hit := range hits {
		if _, ok := loaded[hit.UID]; !ok {
			uids = append(uids, hit.UID)
		}
	}
	return uids
}

// readHits reads each uid in project pid, a few at a time. The search route's
// hits carry no labels or planning dates, so this is how an issue beyond a
// truncated list gets the data the list filters read; it is also how a hit that
// vanished is noticed when the loaded list cannot say. The show route answers
// 404 for a deleted issue, so no deleted-at check is needed.
func readHits(ctx context.Context, api listAPI, pid int64, uids []string) []hitRead {
	reads := make([]hitRead, len(uids))
	sem := make(chan struct{}, daemonSearchReadWorkers)
	var wg sync.WaitGroup
	for i, uid := range uids {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			reads[i].uid = uid
			iss, err := api.GetIssue(ctx, pid, uid)
			var apiErr *APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
				reads[i].gone = true
			case err != nil:
				reads[i].err = err
			case iss == nil || iss.ProjectID != pid:
				reads[i].gone = true
			default:
				reads[i].issue = *iss
			}
		}()
	}
	wg.Wait()
	return reads
}

// mergeReads applies reads to hits, keeping the daemon's order: a gone hit is
// dropped, a read one is swapped for its full copy, and any other is kept. When
// strict, a failed read is returned as the error instead: an ask filtering on
// partial data would silently hide valid hits, while a recheck can simply keep
// what it has.
func mergeReads(hits []Issue, reads []hitRead, strict bool) ([]Issue, error) {
	byUID := make(map[string]hitRead, len(reads))
	for _, read := range reads {
		if read.err != nil && strict {
			return nil, read.err
		}
		byUID[read.uid] = read
	}
	merged := make([]Issue, 0, len(hits))
	for _, hit := range hits {
		read, ok := byUID[hit.UID]
		switch {
		case ok && read.gone:
		case ok && read.err == nil:
			merged = append(merged, read.issue)
		default:
			merged = append(merged, hit)
		}
	}
	return merged, nil
}

// cancelAskReads stops the reads of the ask in flight, the way detail reads are
// stopped when their issue is left.
func (m Model) cancelAskReads() Model {
	if m.askCancel != nil {
		m.askCancel()
		m.askCancel = nil
	}
	return m
}

// cancelRecheck stops the recheck in flight; its answer, if it still comes,
// is dropped because recheckReq no longer matches.
func (m Model) cancelRecheck() Model {
	if m.recheckCancel != nil {
		m.recheckCancel()
		m.recheckCancel = nil
	}
	m.recheckReq = 0
	return m
}

// cancelSearchReads stops every search read still out: on leaving the search,
// or when the scope or daemon its answers belong to goes away.
func (m Model) cancelSearchReads() Model {
	return m.cancelAskReads().cancelRecheck()
}

// recheckRankedHits is run after the list refreshes. A refresh that was cut
// off at the working-set limit cannot say a missing hit is gone, so the hits it
// does not hold are read one by one; the answer drops the vanished ones.
func (m Model) recheckRankedHits() (Model, tea.Cmd) {
	r := m.list.ranked
	if m.api == nil || m.scope.allProjects || m.scope.projectID == 0 || !m.list.truncated ||
		!r.hasAnswer || m.recheckCancel != nil {
		return m, nil
	}
	uids := missingHitUIDs(r.hits, m.list.issues)
	if len(uids) == 0 {
		return m, nil
	}
	m.nextSearchReq++
	req := m.nextSearchReq
	ctx, cancel := context.WithTimeout(context.Background(), daemonSearchTimeout)
	m.recheckReq, m.recheckCancel = req, cancel
	api, pid, connGen := m.api, m.scope.projectID, m.connGen
	return m, func() tea.Msg {
		defer cancel()
		return daemonSearchRecheckMsg{req: req, connGen: connGen, projectID: pid, reads: readHits(ctx, api, pid, uids)}
	}
}

func (m Model) handleDaemonSearchRecheck(msg daemonSearchRecheckMsg) (Model, tea.Cmd) {
	if msg.req != m.recheckReq {
		return m, nil
	}
	m = m.cancelRecheck()
	if m.staleConnMsg(msg.connGen) || m.scope.allProjects || m.scope.projectID != msg.projectID {
		return m, nil
	}
	prevPID, prevUID, prevHas := highlightedIdentity(m.list)
	hits, _ := mergeReads(m.list.ranked.hits, msg.reads, false)
	m.list.ranked.hits = hits
	// A hit the list took in meanwhile is better served by its loaded copy.
	m.list.ranked = m.list.ranked.refreshed(m.list.issues, false)
	m.list = m.list.clampCursorToFilter()
	m.list = m.list.syncSelection(m.list.visibleRows())
	return m.reconcileSplitDetailAfterRefetch(prevPID, prevUID, prevHas)
}

// refreshed swaps each hit for the list's current copy of the same issue.
// When issues is the complete list (complete), a hit it lacks is gone: deleted,
// moved, or outside what the list fetches (the Inbox holds only open issues).
func (r rankedSearch) refreshed(issues []Issue, complete bool) rankedSearch {
	if len(r.hits) == 0 {
		return r
	}
	byUID := make(map[string]Issue, len(issues))
	for _, iss := range issues {
		if iss.UID != "" {
			byUID[iss.UID] = iss
		}
	}
	hits := make([]Issue, 0, len(r.hits))
	for _, hit := range r.hits {
		if cur, ok := byUID[hit.UID]; ok {
			hits = append(hits, cur)
		} else if !complete {
			hits = append(hits, hit)
		}
	}
	r.hits = hits
	return r
}

// discardInFlight keeps a saved answer but makes any ask still out stale. The
// recorded query goes with an ask that never answered, so asking for it again
// is not skipped as already done.
func (r rankedSearch) discardInFlight() rankedSearch {
	if r.pending {
		r.asked = ""
	}
	r.live, r.pending, r.req = false, false, 0
	return r
}

// rankedShown reports whether the rows are the daemon's answer: while the bar is
// open the previous answer stays until the next one lands; once the search is
// committed it stays only while its query stands.
func (lm listModel) rankedShown() bool {
	r := lm.ranked
	if !r.hasAnswer {
		return false
	}
	return r.live || r.answered.Query == strings.TrimSpace(lm.filter.Search)
}

// rankedRows lists the daemon's hits flat, in its order. The route only knows
// the query, so every list filter narrows the hits the way it narrows the
// loaded rows.
func (lm listModel) rankedRows() []queueRow {
	narrow := lm.filter
	narrow.Search = ""
	if lm.inboxOnly && narrow.Status == "" {
		narrow.Status = "open"
	}
	rows := make([]queueRow, 0, len(lm.ranked.hits))
	for _, iss := range lm.ranked.hits {
		if !matchesFilter(iss, narrow) {
			continue
		}
		rows = append(rows, queueRow{issue: iss, key: issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}})
	}
	return rows
}

// dropRanked forgets the answer and discards any ask still in flight. A search
// error the status line still shows goes with it.
func (lm listModel) dropRanked() listModel {
	if lm.ranked.err != nil {
		lm.status = ""
	}
	lm.ranked = rankedSearch{live: lm.ranked.live}
	return lm
}

// rankedNote is the short line beside the search bar: what the daemon is
// doing, or how it ranked the answer.
func (lm listModel) rankedNote() string {
	r := lm.ranked
	shown := lm.rankedShown()
	switch {
	case !r.live && !shown:
		return ""
	case shown && r.lexical:
		return "ranked by words only (lexical)"
	case shown:
		return "ranked by words and meaning"
	case r.err != nil:
		return "search failed"
	case strings.TrimSpace(lm.filter.Search) == "":
		return "type to ask the daemon"
	}
	return "searching…"
}

// daemonSearchAvailable is the one predicate behind the ctrl+t hint in the
// footer and the key's dispatch: the search bar is open over exactly one
// project, which is the scope the daemon's search route covers.
func (m Model) daemonSearchAvailable() bool {
	return m.modal == modalNone && m.input.kind == inputSearchBar &&
		!m.scope.allProjects && m.scope.projectID != 0
}

// daemonSearchHint is what the footer needs to name the key.
type daemonSearchHint struct{ available, on bool }

func (m Model) daemonSearchHint() daemonSearchHint {
	return daemonSearchHint{available: m.daemonSearchAvailable(), on: m.daemonSearch}
}

func (h daemonSearchHint) helpItem() helplayout.HelpItem {
	if h.on {
		return helplayout.HelpItem{Key: "ctrl+t", Description: "filter rows"}
	}
	return helplayout.HelpItem{Key: "ctrl+t", Description: "ask daemon"}
}

// syncDaemonSearch recomputes whether typed text goes to the daemon. Called
// wherever the bar opens, closes or changes so the list never filters rows by
// a query that is meant for the daemon.
func (m Model) syncDaemonSearch() Model {
	m.list.ranked.live = m.daemonSearch && m.daemonSearchAvailable()
	return m
}

// toggleDaemonSearch is ctrl+t in the search bar.
func (m Model) toggleDaemonSearch() (Model, tea.Cmd) {
	if !m.daemonSearchAvailable() {
		return m, nil
	}
	m.daemonSearch = !m.daemonSearch
	m.searchTickGen++
	m = m.cancelSearchReads()
	prevPID, prevUID, prevHas := highlightedIdentity(m.list)
	m = m.syncDaemonSearch()
	m.list = m.list.dropRanked()
	var askCmd tea.Cmd
	if !m.daemonSearch {
		m.list = m.list.clampCursorToFilter()
	} else {
		m, askCmd = m.askDaemonIfStale()
	}
	m, followCmd := m.reconcileSplitDetailAfterRefetch(prevPID, prevUID, prevHas)
	return m, combineCmds(askCmd, followCmd)
}

// debounceDaemonSearch runs after a keystroke changed the bar's text: a blank
// query asks nothing and shows the normal list; anything else is asked once
// typing pauses.
func (m Model) debounceDaemonSearch() (Model, tea.Cmd) {
	if !m.list.ranked.live {
		return m, nil
	}
	m.searchTickGen++
	query := strings.TrimSpace(m.list.filter.Search)
	if query == "" {
		m.list = m.list.dropRanked()
		m.list = m.list.clampCursorToFilter()
		return m, nil
	}
	if query == m.list.ranked.asked {
		return m, nil
	}
	gen := m.searchTickGen
	return m, tickAfter(daemonSearchDebounce, func(time.Time) tea.Msg {
		return daemonSearchTickMsg{gen: gen}
	})
}

func (m Model) handleDaemonSearchTick(msg daemonSearchTickMsg) (Model, tea.Cmd) {
	if msg.gen != m.searchTickGen {
		return m, nil
	}
	return m.askDaemonIfStale()
}

// askDaemonIfStale asks for the bar's query unless it was already asked or is
// blank. Enter and the move into the results call it so the user never commits
// text the daemon has not seen.
func (m Model) askDaemonIfStale() (Model, tea.Cmd) {
	query := strings.TrimSpace(m.list.filter.Search)
	if !m.list.ranked.live || query == "" || query == m.list.ranked.asked || m.api == nil {
		return m, nil
	}
	m.nextSearchReq++
	req := m.nextSearchReq
	m.searchTickGen++
	m.list.ranked.req, m.list.ranked.asked, m.list.ranked.pending = req, query, true
	ask := SearchQuery{Query: query, Limit: daemonSearchLimit}
	m = m.cancelAskReads()
	ctx, cancel := context.WithTimeout(context.Background(), daemonSearchTimeout)
	m.askCancel = cancel
	api, pid, connGen := m.api, m.scope.projectID, m.connGen
	return m, func() tea.Msg {
		defer cancel()
		answer, err := api.SearchIssues(ctx, pid, ask)
		return daemonSearchDoneMsg{req: req, connGen: connGen, projectID: pid, query: ask, answer: answer, err: err}
	}
}

// guardAnswer decides whether an answer for req, asked for query,
// may change the rows now. A refusal also settles what the ask leaves behind.
func (m Model) guardAnswer(connGen uint64, req int64, query SearchQuery) (Model, bool) {
	r := m.list.ranked
	if m.staleConnMsg(connGen) || req == 0 || req != r.req {
		return m, false
	}
	if query.Query != strings.TrimSpace(m.list.filter.Search) {
		// The text moved on before the newest ask was sent. Forget this one so
		// typing the old text again asks again instead of waiting for it.
		m.list.ranked.pending, m.list.ranked.asked = false, ""
		return m, false
	}
	if m.modal != modalNone || m.view != viewList || (m.input.kind != inputNone && m.input.kind != inputSearchBar) {
		// Swapping the rows would move the selection under a form or another
		// view. Forget the ask so the next time the bar opens it asks again.
		m.list.ranked = r.discardInFlight()
		return m, false
	}
	return m, true
}

// failAsk shows why the ask failed and keeps the rows as they were.
func (m Model) failAsk(err error) Model {
	m.list.ranked.pending = false
	m.list.ranked.err = err
	m.list.status = errorStyle.Render("search failed: " + sanitizeForLine(err.Error()))
	return m
}

func (m Model) handleDaemonSearchDone(msg daemonSearchDoneMsg) (Model, tea.Cmd) {
	m, ok := m.guardAnswer(msg.connGen, msg.req, msg.query)
	if !ok {
		return m, nil
	}
	if msg.err == nil && msg.answer == nil {
		msg.err = errors.New("the daemon sent no search answer")
	}
	if msg.err != nil {
		return m.failAsk(msg.err), nil
	}
	// The list may have changed since the ask went out, so what is missing from
	// it is judged now: a complete list means those hits are outside it, a
	// truncated one cannot say and the hits are read.
	missing := missingHitUIDs(msg.answer.Issues, m.list.issues)
	if !m.list.truncated || len(missing) == 0 {
		return m.applyAnswer(msg.query, msg.answer)
	}
	m = m.cancelAskReads()
	ctx, cancel := context.WithTimeout(context.Background(), daemonSearchTimeout)
	m.askCancel = cancel
	api, pid := m.api, msg.projectID
	return m, func() tea.Msg {
		defer cancel()
		return daemonSearchHydrateMsg{
			daemonSearchDoneMsg: msg, reads: readHits(ctx, api, pid, missing),
		}
	}
}

// handleDaemonSearchHydrate lands an answer whose missing hits were read.
func (m Model) handleDaemonSearchHydrate(msg daemonSearchHydrateMsg) (Model, tea.Cmd) {
	m, ok := m.guardAnswer(msg.connGen, msg.req, msg.query)
	if !ok {
		return m, nil
	}
	hits, err := mergeReads(msg.answer.Issues, msg.reads, true)
	if err != nil {
		return m.failAsk(err), nil
	}
	answer := *msg.answer
	answer.Issues = hits
	return m.applyAnswer(msg.query, &answer)
}

// applyAnswer shows the daemon's hits, starting at the first one.
func (m Model) applyAnswer(query SearchQuery, answer *SearchAnswer) (Model, tea.Cmd) {
	r := m.list.ranked
	r.pending = false
	if r.err != nil {
		m.list.status = ""
	}
	r.err = nil
	r.hasAnswer = true
	r.hits = answer.Issues
	r.answered = query
	r.lexical = answer.Mode == "" || answer.Mode == "lexical"
	prevPID, prevUID, prevHas := highlightedIdentity(m.list)
	m.list.ranked = r.refreshed(m.list.issues, m.list.complete())
	m.list.cursor, m.list.windowStart = 0, 0
	m.list.selectedUID, m.list.selectedProjectID = "", 0
	m.list = m.list.syncSelection(m.list.visibleRows())
	return m.reconcileSplitDetailAfterRefetch(prevPID, prevUID, prevHas)
}

func (m Model) routeDaemonSearch(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case daemonSearchTickMsg:
		next, cmd := m.handleDaemonSearchTick(msg)
		return next, cmd, true
	case daemonSearchDoneMsg:
		next, cmd := m.handleDaemonSearchDone(msg)
		return next, cmd, true
	case daemonSearchHydrateMsg:
		next, cmd := m.handleDaemonSearchHydrate(msg)
		return next, cmd, true
	case daemonSearchRecheckMsg:
		next, cmd := m.handleDaemonSearchRecheck(msg)
		return next, cmd, true
	}
	return m, nil, false
}

// daemonSearchTickMsg fires when typing has paused; gen is the
// Model.searchTickGen at scheduling time so a later keystroke drops it.
type daemonSearchTickMsg struct{ gen int64 }

// daemonSearchDoneMsg carries the daemon's answer to ask; req identifies the
// ask so an answer that is no longer the newest is dropped.
type daemonSearchDoneMsg struct {
	req       int64
	connGen   uint64
	projectID int64
	query     SearchQuery
	answer    *SearchAnswer
	err       error
}

// daemonSearchHydrateMsg is an answer together with the reads of the hits the
// truncated list did not hold.
type daemonSearchHydrateMsg struct {
	daemonSearchDoneMsg
	reads []hitRead
}

// daemonSearchRecheckMsg carries the reads of the hits a truncated list did not
// hold; req is Model.recheckReq at the start, so a cancelled recheck drops.
type daemonSearchRecheckMsg struct {
	req       int64
	connGen   uint64
	projectID int64
	reads     []hitRead
}
