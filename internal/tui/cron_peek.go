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
	if h.board != nil {
		h.board.SetPeek(cronui.PeekState{})
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
		h.board.SetPeek(cronui.PeekState{Kind: "unreadable"})
		return nil
	}
	if !refresh {
		h.board.SetPeek(cronui.PeekState{Kind: "loading"})
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
// peeked issue's title or comments. Each change restarts the read's delay, so
// a burst of events costs one read.
func (h *cronHost) invalidateCronPeek(msg eventReceivedMsg) {
	switch msg.eventType {
	case "issue.commented", "issue.comment_edited", "issue.updated":
	default:
		return
	}
	if h.peekKey != (cronPeekKey{}) && msg.issueUID == h.peekKey.issueUID {
		h.peekStale = true
	}
}

func (h *cronHost) acceptCronPeek(msg cronPeekMsg) {
	key, ok := h.cronPeekSelection()
	if !ok || msg.generation != h.peekGeneration || msg.key != h.peekKey || msg.key != key || !reflect.DeepEqual(h.peekAuth, h.native.activeAuth) {
		return
	}
	if records.AuthRejected(msg.err) {
		h.clearCronAccess(msg.err)
		h.updateCronBoard()
		return
	}
	state := cronui.PeekState{Kind: "ready", Issue: msg.issue}
	if msg.err != nil {
		state = cronui.PeekState{Kind: "unreadable"}
	} else if msg.issue.Latest == nil {
		state.Kind = "no_comments"
	}
	h.board.SetPeek(state)
}

// Close releases request authority as well as the existing child editor.
func (h *cronHost) Close() error {
	h.peekClosed = true
	h.clearCronPeek()
	if h.board != nil {
		return h.board.Close()
	}
	return nil
}
