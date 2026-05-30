package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// LabelCount mirrors the daemon's LabelsListResponse.Body.Labels wire
// shape (db.LabelCount). Local definition keeps the TUI free of an
// internal/db import — the package boundary stays at the wire layer.
type LabelCount struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// labelCache holds per-project label aggregates feeding the `+`
// suggestion menu. Keyed by projectID; each entry carries its own
// dispatch generation so a slow response can't clobber a newer one.
type labelCache struct {
	byProject map[int64]labelCacheEntry
}

// labelCacheEntry is one project's snapshot. gen is the dispatch-time
// generation so handleLabelsFetched can reject stale responses by
// comparing msg.gen against entry.gen. fetching is true between
// dispatchLabelFetch and the matching response (or its rejection by
// the gen-mismatch path); the suggestion-menu placeholder reads it.
//
// pid is redundant with the map key but kept on the entry so callers
// (and tests) can assert the entry's identity without re-deriving it.
type labelCacheEntry struct {
	labels   []LabelCount
	gen      int64
	pid      int64
	err      error
	fetching bool
}

// newLabelCache returns a zero-valued cache with the inner map
// allocated so callers never have to nil-check before assignment.
func newLabelCache() *labelCache {
	return &labelCache{byProject: map[int64]labelCacheEntry{}}
}

// labelLister is the subset of *Client the label-cache dispatcher
// needs. Defining it as an interface (instead of taking *Client
// directly) lets tests inject a fake without standing up an httptest
// server — same pattern as listAPI / detailAPI.
type labelLister interface {
	ListLabels(ctx context.Context, projectID int64) ([]LabelCount, error)
}

// dispatchLabelFetch stamps the cache entry for pid with a fresh
// generation, marks it fetching, and returns the tea.Cmd that issues
// the underlying HTTP call. The gen is stamped BEFORE the request
// goes out so a slow response that arrives after a newer dispatch
// will see msg.gen < entry.gen and be dropped — without this
// ordering a sequence of "open prompt → switch project → response
// from old project lands" would populate the wrong cache entry.
func (m Model) dispatchLabelFetch(pid int64) (Model, tea.Cmd) {
	if m.projectLabels == nil {
		m.projectLabels = newLabelCache()
	}
	m.nextLabelsGen++
	gen := m.nextLabelsGen
	entry := m.projectLabels.byProject[pid]
	entry.gen = gen
	entry.pid = pid
	entry.fetching = true
	entry.err = nil
	m.projectLabels.byProject[pid] = entry
	var api labelLister
	if m.api != nil {
		api = m.api
	}
	return m, withConnGen(fetchLabelsCmd(api, pid, gen), m.connGen)
}

// fetchLabelsCmd returns a tea.Cmd that calls api.ListLabels and
// emits labelsFetchedMsg. nil api returns a synthetic error message
// so the cache reflects the failure rather than spinning forever in
// fetching=true (matches how the rest of the TUI handles the missing-
// client path under tests).
func fetchLabelsCmd(api labelLister, pid, gen int64) tea.Cmd {
	if api == nil {
		return func() tea.Msg {
			return labelsFetchedMsg{pid: pid, gen: gen}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		labels, err := api.ListLabels(ctx, pid)
		return labelsFetchedMsg{pid: pid, gen: gen, labels: labels, err: err}
	}
}

// handleLabelsFetched routes a labelsFetchedMsg into the cache.
// Messages whose gen lags behind the entry's gen are dropped — a
// later dispatch supersedes the in-flight one.
//
// Pid mismatch (msg.pid != current target) is NOT a drop: the cache
// is keyed by pid, so a response carrying pid=X always belongs in
// the pid=X slot regardless of which project the user is currently
// viewing. Dropping inactive-project responses left entry.fetching=
// true forever, so dispatchLabelFetchIfNeeded would later see "entry
// exists" and skip the re-fetch — leaving the suggestion menu stuck
// in "loading…" when the user returned to that project (jobs 240/241).
func (m Model) handleLabelsFetched(msg labelsFetchedMsg) Model {
	if m.projectLabels == nil {
		return m
	}
	entry, exists := m.projectLabels.byProject[msg.pid]
	if !exists || msg.gen < entry.gen {
		return m
	}
	entry.labels = msg.labels
	entry.err = msg.err
	entry.fetching = false
	m.projectLabels.byProject[msg.pid] = entry
	return m
}
