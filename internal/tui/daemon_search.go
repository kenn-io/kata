package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// all is the answer's issues in the daemon's order. hits are those among
	// the loaded rows, each the list's own copy, rebuilt from all on every
	// fetch so a hit enters or leaves as the loaded rows change.
	all, hits []Issue
	// beyond counts the answer's hits that are not among the loaded rows.
	beyond  int
	lexical bool
}

// loadedOnly rebuilds hits from all: the ones the loaded issues hold, in the
// daemon's order, each as the list's own copy. The route's hits carry no labels or planning dates and
// may be stale; the loaded rows are what every other list filter reads.
func (r rankedSearch) loadedOnly(issues []Issue) rankedSearch {
	byUID := make(map[string]Issue, len(issues))
	for _, iss := range issues {
		byUID[iss.UID] = iss
	}
	r.hits = make([]Issue, 0, len(r.all))
	for _, hit := range r.all {
		if cur, ok := byUID[hit.UID]; ok {
			r.hits = append(r.hits, cur)
		}
	}
	r.beyond = len(r.all) - len(r.hits)
	return r
}

// narrowRanked applies loadedOnly once there are loaded rows to judge against:
// a list still loading is empty, so its hits stay as the answer gave them.
func (lm listModel) narrowRanked() listModel {
	if !lm.loading {
		lm.ranked = lm.ranked.loadedOnly(lm.issues)
	}
	return lm
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
	case shown:
		note := "ranked by words and meaning"
		if r.lexical {
			note = "ranked by words only (lexical)"
		}
		if lm.truncated && r.beyond > 0 {
			// The list holds the first issues only; say what the answer had past them.
			hits := "hits"
			if r.beyond == 1 {
				hits = "hit"
			}
			note += fmt.Sprintf(" · %d %s beyond the loaded issues", r.beyond, hits)
		}
		return note
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
	api, pid, connGen := m.api, m.scope.projectID, m.connGen
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonSearchTimeout)
		defer cancel()
		answer, err := api.SearchIssues(ctx, pid, ask)
		return daemonSearchDoneMsg{req: req, connGen: connGen, query: ask, answer: answer, err: err}
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
	return m.applyAnswer(msg.query, msg.answer)
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
	r.all, r.hits = answer.Issues, answer.Issues
	r.answered = query
	r.lexical = answer.Mode == "" || answer.Mode == "lexical"
	prevPID, prevUID, prevHas := highlightedIdentity(m.list)
	m.list.ranked = r
	m.list = m.list.narrowRanked()
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
	}
	return m, nil, false
}

// daemonSearchTickMsg fires when typing has paused; gen is the
// Model.searchTickGen at scheduling time so a later keystroke drops it.
type daemonSearchTickMsg struct{ gen int64 }

// daemonSearchDoneMsg carries the daemon's answer to ask; req identifies the
// ask so an answer that is no longer the newest is dropped.
type daemonSearchDoneMsg struct {
	req     int64
	connGen uint64
	query   SearchQuery
	answer  *SearchAnswer
	err     error
}
