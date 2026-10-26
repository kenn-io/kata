package tui

import (
	"context"
	"reflect"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type cronPeekKey struct {
	epoch, evidence, projectGeneration uint64
	project                            int64
	projectUID, issueUID               string
}

// cronRelatedMsg carries the optional AgentsView locator for an accepted peek.
type cronRelatedMsg struct {
	key        cronPeekKey
	generation uint64
	url        string
	err        error
}

type cronPeekMsg struct {
	key        cronPeekKey
	generation uint64
	issue      records.IssuePeek
	err        error
}

func (h *cronHost) cronPeekSelection() (cronPeekKey, bool) {
	if h.peekClosed || h.tab == 0 || h.board == nil || h.native.issueScoped || h.native.activeAuthRejected || h.native.authCapabilitiesRequired && !h.native.authCapabilitiesReady || h.native.activeAuth.ExpiresAt != nil && !h.native.toastNow().Before(*h.native.activeAuth.ExpiresAt) {
		return cronPeekKey{}, false
	}
	run, ok := h.board.SelectedRun()
	if !ok || records.Value(run.IssueUID) == "" {
		return cronPeekKey{}, false
	}
	project, err := strconv.ParseInt(run.ProjectKey, 10, 64)
	if err != nil || !h.cronProjectAccessible(project) {
		return cronPeekKey{}, false
	}
	cat := h.catalogs[project]
	return cronPeekKey{epoch: h.epoch, evidence: cat.evidence, projectGeneration: h.issueGenerations[project], project: project, projectUID: cat.uid, issueUID: *run.IssueUID}, true
}

func (h *cronHost) clearCronPeek() {
	h.peekGeneration++
	if h.peekCancel != nil {
		h.peekCancel()
		h.peekCancel = nil
	}
	h.peekKey = cronPeekKey{}
	h.peekAuth = AuthInfo{}
	h.peekStale = false
	h.peekRelatedPending = true
	h.setCronPeek(cronui.PeekState{})
}

func (h *cronHost) setCronPeek(state cronui.PeekState) {
	h.peekState = state
	if h.board != nil {
		h.board.SetPeek(state)
	}
}

// syncCronPeek runs after host message handling so every selection path shares
// one debounce and stale replies cannot start work from the child renderer.
func (h *cronHost) syncCronPeek() tea.Cmd {
	key, ok := h.cronPeekSelection()
	if !ok {
		if h.peekKey != (cronPeekKey{}) {
			h.clearCronPeek()
		}
		return nil
	}
	refresh := key == h.peekKey && reflect.DeepEqual(h.peekAuth, h.native.activeAuth)
	if refresh && !h.peekStale {
		return nil
	}
	if refresh {
		// The selection is unchanged, so its context stays visible while
		// the replacement read fences and cancels the older one.
		h.peekGeneration++
		if h.peekCancel != nil {
			h.peekCancel()
		}
		h.peekStale = false
	} else {
		h.clearCronPeek()
		h.peekKey, h.peekAuth = key, h.native.activeAuth
	}
	api := h.native.api
	if wrapped, ok := api.(*undoClient); ok {
		api = wrapped.KataAPI
	}
	peekAPI, ok := api.(CronPeekAPI)
	if !ok {
		h.setCronPeek(cronui.PeekState{Kind: "unreadable"})
		return nil
	}
	if !refresh {
		h.setCronPeek(cronui.PeekState{Kind: "loading"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
	h.peekCancel = cancel
	generation, slots := h.peekGeneration, h.slots
	return func() tea.Msg {
		defer cancel()
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return cronPeekMsg{key: key, generation: generation, err: ctx.Err()}
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return cronPeekMsg{key: key, generation: generation, err: ctx.Err()}
		}
		defer func() { <-slots }()
		if err := ctx.Err(); err != nil {
			return cronPeekMsg{key: key, generation: generation, err: err}
		}
		issue, err := peekAPI.CronIssuePeek(ctx, key.project, key.issueUID)
		return cronPeekMsg{key: key, generation: generation, issue: issue, err: err}
	}
}

// invalidateCronPeek marks the peek stale when a live event changes the
// peeked issue's title, comments, or readability in the run's project. Each
// change restarts the read's delay, so a burst of events costs one read. A
// close, move, restore or snapshot can change which transcript locator is
// recorded or readable, so those also reread the optional related session.
func (h *cronHost) invalidateCronPeek(msg eventReceivedMsg) {
	switch msg.eventType {
	case "issue.commented", "issue.comment_edited", "issue.updated", "issue.closed",
		"issue.soft_deleted", "issue.restored", "issue.moved", "issue.snapshot":
	default:
		return
	}
	if h.peekKey == (cronPeekKey{}) || msg.issueUID != h.peekKey.issueUID {
		return
	}
	h.peekStale = true
	switch msg.eventType {
	case "issue.closed", "issue.restored", "issue.moved", "issue.snapshot":
		h.peekRelatedPending = true
	}
}

// currentCronPeek fences a reply to the selection, generation and authority
// that requested it.
func (h *cronHost) currentCronPeek(key cronPeekKey, generation uint64) bool {
	current, ok := h.cronPeekSelection()
	return ok && generation == h.peekGeneration && key == h.peekKey && key == current && reflect.DeepEqual(h.peekAuth, h.native.activeAuth)
}

func (h *cronHost) acceptCronPeek(msg cronPeekMsg) tea.Cmd {
	if !h.currentCronPeek(msg.key, msg.generation) {
		return nil
	}
	if records.AuthRejected(msg.err) {
		h.clearCronAccess(msg.err)
		h.updateCronBoard()
		return nil
	}
	if msg.err != nil {
		// Whatever made the issue unreadable also voids its related session.
		h.peekRelatedPending = true
		h.setCronPeek(cronui.PeekState{Kind: "unreadable"})
		return nil
	}
	state := cronui.PeekState{Kind: "ready", Issue: msg.issue}
	if msg.issue.Latest == nil {
		state.Kind = "no_comments"
	}
	if !h.peekRelatedPending && h.peekState.Issue.ProjectID == msg.issue.ProjectID {
		// A comment or title refresh keeps the locator already read; only
		// a new selection or a close of this issue reads it again.
		state.RelatedURL = h.peekState.RelatedURL
		h.setCronPeek(state)
		return nil
	}
	h.setCronPeek(state)
	return h.loadCronRelated(msg.key, msg.generation, msg.issue.ProjectID)
}

// loadCronRelated is the optional AgentsView read. It starts only after the
// ordinary peek is readable and only when a browser opener exists, so its
// failure or delay never affects the core peek.
func (h *cronHost) loadCronRelated(key cronPeekKey, generation uint64, issueProject int64) tea.Cmd {
	api := h.native.api
	if wrapped, ok := api.(*undoClient); ok {
		api = wrapped.KataAPI
	}
	related, ok := api.(CronRelatedAPI)
	if !ok || h.native.opts.OpenURL == nil || key.projectUID == "" {
		return nil
	}
	// History is read where the issue lives now. An unmoved issue binds the
	// read to the run's canonical catalog project UID, never the decimal UI
	// key; a moved one binds to its new project's catalog UID when known.
	project := cronRelatedProject{id: key.project, uid: key.projectUID, runUID: key.projectUID}
	if issueProject > 0 && issueProject != key.project {
		project.id, project.uid = issueProject, h.catalogs[issueProject].uid
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
	h.peekCancel = cancel
	return func() tea.Msg {
		defer cancel()
		locator, err := related.CronRelatedTranscript(ctx, project, key.issueUID)
		return cronRelatedMsg{key: key, generation: generation, url: locator, err: err}
	}
}

func (h *cronHost) acceptCronRelated(msg cronRelatedMsg) {
	if !h.currentCronPeek(msg.key, msg.generation) || h.peekState.Issue.IssueUID != msg.key.issueUID {
		return
	}
	h.peekRelatedPending = false
	// Kata rejecting the credential keeps its baseline effect; every other
	// failure leaves the readable peek without the optional action.
	if records.AuthRejected(msg.err) {
		h.clearCronAccess(msg.err)
		h.updateCronBoard()
		return
	}
	if msg.err != nil || msg.url == "" {
		return
	}
	state := h.peekState
	state.RelatedURL = msg.url
	h.setCronPeek(state)
}

// openCronRelated opens the current peek's locator only, as one argument to
// the platform browser adapter.
func (h *cronHost) openCronRelated(v cronui.OpenRelatedMsg) tea.Cmd {
	open := h.native.opts.OpenURL
	key, ok := h.cronPeekSelection()
	if open == nil || !ok || key != h.peekKey || v.URL == "" || v.URL != h.peekState.RelatedURL {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		return cronRelatedOpenedMsg{err: open(ctx, v.URL)}
	}
}

type cronRelatedOpenedMsg struct{ err error }

// Close releases request authority as well as the existing child editor.
func (h *cronHost) Close() error {
	h.peekClosed = true
	h.clearCronPeek()
	if h.board != nil {
		return h.board.Close()
	}
	return nil
}
