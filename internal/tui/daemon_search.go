package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/helplayout"
)

const (
	// daemonSearchMode lets the daemon rank by words and meaning when its
	// embeddings work and by words alone otherwise.
	daemonSearchMode  = "auto"
	daemonSearchLimit = 50
	// daemonSearchDebounce waits for typing to pause so one question goes to
	// the daemon per burst of keystrokes.
	daemonSearchDebounce = 250 * time.Millisecond
	// A meaning search embeds the query first, which is slower than a list read.
	daemonSearchTimeout      = 15 * time.Second
	daemonSearchNeedsProject = "daemon search needs one project"
)

// rankedSearch is the daemon's ranked answer to the search bar's query and the
// state around asking for it. The zero value is "not asking, nothing to show".
type rankedSearch struct {
	// live is true while the search bar is open with daemon search on: the
	// typed text asks the daemon instead of filtering the loaded rows.
	live bool
	// refused is true after ctrl+t in a scope the daemon search cannot cover.
	refused bool
	// req identifies the newest ask. An answer for another id is dropped; zero
	// never matches, so zeroing it discards whatever is still in flight.
	req     int64
	pending bool
	// asked is the query sent with req, so a repeated query is not asked twice.
	asked string
	err   error

	have bool
	// hits are the answer's issues in the daemon's order, replaced by the
	// list's own copy of an issue whenever a refresh brings a fresher one.
	hits   []Issue
	query  string
	status string
	labels []string

	lexical   bool
	degraded  bool
	reason    string
	truncated bool
}

// refreshed swaps each hit for the list's current copy of the same issue.
func (r rankedSearch) refreshed(issues []Issue) rankedSearch {
	if len(r.hits) == 0 {
		return r
	}
	byUID := make(map[string]Issue, len(issues))
	for _, iss := range issues {
		if iss.UID != "" {
			byUID[iss.UID] = iss
		}
	}
	hits := make([]Issue, len(r.hits))
	for i, hit := range r.hits {
		hits[i] = hit
		if cur, ok := byUID[hit.UID]; ok {
			hits[i] = cur
		}
	}
	r.hits = hits
	return r
}

// discardInFlight keeps a saved answer but makes any ask still out stale.
func (r rankedSearch) discardInFlight() rankedSearch {
	r.live, r.refused, r.pending, r.req = false, false, false, 0
	return r
}

// rankedStatus is the status the daemon is asked for: the Inbox fetch only
// ever holds open issues.
func (lm listModel) rankedStatus() string {
	if lm.inboxOnly {
		return "open"
	}
	return lm.filter.Status
}

// rankedShown reports whether the rows are the daemon's answer: while the bar is
// open the previous answer stays until the next one lands; once the search is
// committed it stays only while the query and the filters it was asked with
// still stand.
func (lm listModel) rankedShown() bool {
	r := lm.ranked
	if !r.have || r.status != lm.rankedStatus() || !slices.Equal(r.labels, lm.filter.Labels) {
		return false
	}
	return r.live || r.query == strings.TrimSpace(lm.filter.Search)
}

// rankedRows lists the daemon's hits flat, in its order. The route already
// applied the query, status and labels, so only the filters it cannot apply
// narrow the hits.
func (lm listModel) rankedRows() []queueRow {
	narrow := ListFilter{
		Owner: lm.filter.Owner, Author: lm.filter.Author, Date: lm.filter.Date, Day: lm.filter.Day,
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
// doing, how it ranked the answer, and why that may fall short.
func (lm listModel) rankedNote() string {
	r := lm.ranked
	if r.refused {
		return daemonSearchNeedsProject
	}
	shown := lm.rankedShown()
	if !r.live && !shown {
		return ""
	}
	var parts []string
	switch {
	case r.err != nil:
		parts = append(parts, "search failed")
	case !shown && strings.TrimSpace(lm.filter.Search) == "":
		parts = append(parts, "type to ask the daemon")
	case !shown:
		parts = append(parts, "searching…")
	}
	if !shown {
		return strings.Join(parts, " · ")
	}
	if r.lexical {
		parts = append(parts, "ranked by words only (lexical)")
	} else {
		parts = append(parts, "ranked by words and meaning")
	}
	switch {
	case r.reason != "":
		parts = append(parts, sanitizeForLine(r.reason))
	case r.degraded:
		parts = append(parts, "degraded")
	}
	if r.truncated {
		parts = append(parts, fmt.Sprintf("showing the best %d", daemonSearchLimit))
	}
	return strings.Join(parts, " · ")
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
	if m.input.kind != inputSearchBar {
		m.list.ranked.refused = false
	}
	return m
}

// toggleDaemonSearch is ctrl+t in the search bar.
func (m Model) toggleDaemonSearch() (Model, tea.Cmd) {
	if !m.daemonSearchAvailable() {
		m.list.ranked.refused = true
		return m, nil
	}
	m.list.ranked.refused = false
	m.daemonSearch = !m.daemonSearch
	m.searchTickGen++
	m = m.syncDaemonSearch()
	m.list = m.list.dropRanked()
	if !m.daemonSearch {
		m.list = m.list.clampCursorToFilter()
		return m, nil
	}
	return m.askDaemonIfStale()
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
	ask := SearchQuery{
		Query:  query,
		Mode:   daemonSearchMode,
		Limit:  daemonSearchLimit,
		Status: m.list.rankedStatus(),
		Labels: slices.Clone(m.list.filter.Labels),
	}
	api, pid, connGen := m.api, m.scope.projectID, m.connGen
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonSearchTimeout)
		defer cancel()
		answer, err := api.SearchIssues(ctx, pid, ask)
		return daemonSearchDoneMsg{req: req, connGen: connGen, query: ask, answer: answer, err: err}
	}
}

func (m Model) handleDaemonSearchDone(msg daemonSearchDoneMsg) (Model, tea.Cmd) {
	r := m.list.ranked
	if m.staleConnMsg(msg.connGen) || msg.req == 0 || msg.req != r.req {
		return m, nil
	}
	r.pending = false
	if msg.err == nil && msg.answer == nil {
		msg.err = errors.New("the daemon sent no search answer")
	}
	if msg.err != nil {
		r.err = msg.err
		m.list.ranked = r
		m.list.status = errorStyle.Render("search failed: " + sanitizeForLine(msg.err.Error()))
		return m, nil
	}
	if r.err != nil {
		m.list.status = ""
	}
	r.err = nil
	r.have = true
	r.hits = msg.answer.Issues
	r.query = msg.query.Query
	r.status, r.labels = msg.query.Status, msg.query.Labels
	r.lexical = msg.answer.Mode == "" || msg.answer.Mode == "lexical"
	r.degraded = msg.answer.Degraded
	r.reason = msg.answer.DegradedReason
	r.truncated = msg.answer.Truncated
	m.list.ranked = r.refreshed(m.list.issues)
	m.list.cursor, m.list.windowStart = 0, 0
	m.list.selectedUID, m.list.selectedProjectID = "", 0
	m.list = m.list.syncSelection(m.list.visibleRows())
	return m, nil
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
