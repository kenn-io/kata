package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// handleMutationKey dispatches the mutation bindings. Close/reopen and
// clear-owner fire immediately; the others open a modal and the actual
// mutation runs from handleModalKey on Enter. ok=true means the key was
// consumed.
//
// NewChild opens the new-issue form pre-wired with the current issue
// as parent. The key is consumed even before dm.issue is seeded so a
// stray N during detail load does not bleed into other bindings; if
// the issue isn't ready yet, the press becomes a no-op rather than
// triggering an unintended mutation.
func (dm detailModel) handleMutationKey(
	msg tea.KeyPressMsg, km keymap, api detailAPI,
) (detailModel, tea.Cmd, bool) {
	if next, cmd, ok := dm.handleStatusKey(msg, km, api); ok {
		return next, cmd, true
	}
	if km.NewChild.matches(msg) {
		if dm.issue == nil {
			return dm, nil, true
		}
		dm.status = ""
		return dm, openNewChildInputCmd(dm.issue.ShortID), true
	}
	return dm.handleModalOpenKey(msg, km)
}

// handleStatusKey routes the keys that don't open a modal: close,
// reopen, clear-owner. Each fires a single mutation immediately.
func (dm detailModel) handleStatusKey(
	msg tea.KeyPressMsg, km keymap, api detailAPI,
) (detailModel, tea.Cmd, bool) {
	switch {
	case km.Close.matches(msg):
		return dm, dm.dispatchClose(api), true
	case km.Reopen.matches(msg):
		return dm, dm.dispatchReopen(api), true
	case km.ClearOwner.matches(msg):
		return dm, dm.dispatchAssign(api, ""), true
	}
	return dm, nil, false
}

// handleModalOpenKey routes the keys that open a panel-local prompt
// (M3b shell). Each binding emits openInputMsg so Model.openInput
// constructs the inputState centrally; dm itself holds no input
// state any more (M3b retired dm.modal).
func (dm detailModel) handleModalOpenKey(
	msg tea.KeyPressMsg, km keymap,
) (detailModel, tea.Cmd, bool) {
	var kind inputKind
	switch {
	case km.AddLabel.matches(msg):
		kind = inputLabelPrompt
	case km.RemoveLabel.matches(msg):
		kind = inputRemoveLabelPrompt
	case km.AssignOwner.matches(msg):
		kind = inputOwnerPrompt
	case km.SetParent.matches(msg):
		kind = inputParentPrompt
	case km.AddBlocker.matches(msg):
		kind = inputBlockerPrompt
	case km.AddLink.matches(msg):
		kind = inputLinkPrompt
	case km.SetPriority.matches(msg):
		kind = inputPriorityPrompt
	default:
		return dm, nil, false
	}
	dm.status = ""
	return dm, openInputCmd(kind), true
}

// dispatchPanelPromptCommit routes a panel-local prompt's committed
// buffer through the right detail-side dispatcher. Called from
// Model.commitInput when the active input is one of the M3b prompt
// kinds. Parse failures (e.g., non-numeric blocker) surface as a
// status hint via the existing parseFailedCmd path.
//
// dm is returned for shape-consistency with other detail handlers,
// even though no dm field is mutated (the dispatch is purely a
// command).
func (dm detailModel) dispatchPanelPromptCommit(
	api detailAPI, kind inputKind, buf string,
) (detailModel, tea.Cmd) {
	switch kind {
	case inputLabelPrompt:
		return dm, dm.dispatchLabel(api, buf, true)
	case inputRemoveLabelPrompt:
		return dm, dm.dispatchLabel(api, buf, false)
	case inputOwnerPrompt:
		return dm, dm.dispatchAssign(api, buf)
	case inputParentPrompt:
		return dm, dm.dispatchLink(api, "parent", buf)
	case inputBlockerPrompt:
		return dm, dm.dispatchLink(api, "blocks", buf)
	case inputLinkPrompt:
		return dm, dm.dispatchLink(api, "related", buf)
	case inputPriorityPrompt:
		return dm, dm.dispatchSetPriority(api, buf)
	}
	return dm, nil
}

// applyMutation handles mutationDoneMsg arriving back at the detail
// view. Success seeds a status hint and dispatches a single-issue
// refetch (so the body, comments, events, and links reflect the new
// state); failure surfaces an error toast in dm.status.
//
// Messages with origin != "detail" or gen != dm.gen are dropped: a list
// mutation that completed after the user opened detail must not steal
// the detail status line, and a detail mutation in flight when the user
// jumped or popped must not refetch the now-stale issue.
func (dm detailModel) applyMutation(
	m mutationDoneMsg, api detailAPI,
) (detailModel, tea.Cmd) {
	if m.origin != "detail" || m.gen != dm.gen {
		return dm, nil
	}
	if m.err != nil {
		dm.err = m.err
		dm.status = errorStyle.Render(
			fmt.Sprintf("%s failed: %s", m.kind, m.err.Error()),
		)
		return dm, nil
	}
	dm.status = mutationSuccessText(m, dm.issue)
	return dm, dm.refetchAfterMutation(api)
}

// successTemplates maps mutation-kind to the printf template used by
// mutationSuccessText. Keeping the dispatch table-driven keeps the
// formatter at cyclomatic ≤8 and makes adding kinds (Task 11+) trivial.
var successTemplates = map[string]string{
	"close":          "closed #%s",
	"reopen":         "reopened #%s",
	"label.add":      "added label to #%s",
	"label.remove":   "removed label from #%s",
	"owner.assign":   "assigned #%s",
	"owner.clear":    "unassigned #%s",
	"link.parent":    "linked #%s",
	"link.blocks":    "linked #%s",
	"link.relates":   "linked #%s",
	"body.edit":      "updated body of #%s",
	"comment.add":    "added comment to #%s",
	"priority.set":   "set priority of #%s",
	"priority.clear": "cleared priority of #%s",
}

// mutationSuccessText is the per-kind toast for a successful mutation.
// The issue short_id is read off dm.issue because the resp may not
// carry it (the daemon's mutation envelope embeds the issue, but the
// test fakes don't always populate that — and dm.issue is
// authoritative).
func mutationSuccessText(m mutationDoneMsg, iss *Issue) string {
	ref := ""
	if iss != nil {
		ref = iss.ShortID
	}
	if m.resp != nil && m.resp.Issue != nil && m.resp.Issue.ShortID != "" {
		ref = m.resp.Issue.ShortID
	}
	if tpl, ok := successTemplates[m.kind]; ok {
		return fmt.Sprintf(tpl, ref)
	}
	return ""
}

// refetchAfterMutation re-fetches the issue and the three tabs so the
// rendered detail reflects the new state without waiting for the SSE
// consumer (Task 11) to invalidate. The four fetches run in parallel
// via tea.Batch — the order they land doesn't matter because each
// fetch updates a distinct slice on dm. dm.gen rides every fetch so a
// later jump/pop discards stale results.
func (dm detailModel) refetchAfterMutation(api detailAPI) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	pid := dm.scopePID
	ref := dm.issue.ShortID
	gen := dm.gen
	return tea.Batch(
		fetchIssue(api, pid, ref, gen),
		fetchComments(api, pid, ref, gen),
		fetchEvents(api, pid, ref, gen),
		fetchLinks(api, pid, ref, gen),
	)
}

// dispatchClose returns a tea.Cmd that calls api.Close and reports the
// result via mutationDoneMsg. Returns nil if the issue isn't seeded.
// origin="detail" + gen route the response back to dm.applyMutation.
func (dm detailModel) dispatchClose(api detailAPI) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	pid, ref, actor, gen := dm.scopePID, dm.issue.ShortID, dm.actor, dm.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.Close(ctx, pid, ref, actor)
		return mutationDoneMsg{
			origin: "detail", gen: gen, kind: "close", resp: resp, err: err,
		}
	}
}

// dispatchReopen mirrors dispatchClose for the reopen action.
func (dm detailModel) dispatchReopen(api detailAPI) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	pid, ref, actor, gen := dm.scopePID, dm.issue.ShortID, dm.actor, dm.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.Reopen(ctx, pid, ref, actor)
		return mutationDoneMsg{
			origin: "detail", gen: gen, kind: "reopen", resp: resp, err: err,
		}
	}
}

// dispatchLabel routes to AddLabel or RemoveLabel by add/!add.
func (dm detailModel) dispatchLabel(
	api detailAPI, label string, add bool,
) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	pid, ref, actor, gen := dm.scopePID, dm.issue.ShortID, dm.actor, dm.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var (
			resp *MutationResp
			err  error
			kind string
		)
		if add {
			resp, err = api.AddLabel(ctx, pid, ref, label, actor)
			kind = "label.add"
		} else {
			resp, err = api.RemoveLabel(ctx, pid, ref, label, actor)
			kind = "label.remove"
		}
		return mutationDoneMsg{
			origin: "detail", gen: gen, kind: kind, resp: resp, err: err,
		}
	}
}

// dispatchAssign calls Assign with the given owner. Empty owner is the
// clear case; the client routes that to /actions/unassign automatically.
func (dm detailModel) dispatchAssign(api detailAPI, owner string) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	pid, ref, actor, gen := dm.scopePID, dm.issue.ShortID, dm.actor, dm.gen
	kind := "owner.assign"
	if owner == "" {
		kind = "owner.clear"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.Assign(ctx, pid, ref, owner, actor)
		return mutationDoneMsg{
			origin: "detail", gen: gen, kind: kind, resp: resp, err: err,
		}
	}
}

// dispatchLink calls AddLink with the given type and a string target.
// The target is trimmed and a leading "#" is stripped so users can
// type "#abc4" or "abc4" interchangeably; an empty trimmed input
// surfaces as a parse error. The daemon enforces the type vocabulary
// and the ref shape, so we don't pre-validate the short_id format
// here.
func (dm detailModel) dispatchLink(
	api detailAPI, linkType, target string,
) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	toRef := strings.TrimPrefix(strings.TrimSpace(target), "#")
	if toRef == "" {
		return parseFailedCmd(linkType, target, dm.gen)
	}
	pid, ref, actor, gen := dm.scopePID, dm.issue.ShortID, dm.actor, dm.gen
	kind := "link." + linkType
	if linkType == "related" {
		kind = "link.relates"
	}
	body := LinkBody{Type: linkType, ToRef: toRef}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.AddLink(ctx, pid, ref, body, actor)
		return mutationDoneMsg{
			origin: "detail", gen: gen, kind: kind, resp: resp, err: err,
		}
	}
}

// dispatchSetPriority parses the priority prompt buffer. "-" clears,
// "0".."4" sets. Mirrors the CLI's parseEditPriority shape so the two
// surfaces accept the same input. Bad input surfaces as a parse error
// via the same parseFailedCmd path the link prompt uses.
func (dm detailModel) dispatchSetPriority(
	api detailAPI, buf string,
) tea.Cmd {
	if dm.issue == nil {
		return nil
	}
	trimmed := strings.TrimSpace(buf)
	var (
		priority *int64
		kind     = "priority.set"
	)
	if trimmed == "-" {
		kind = "priority.clear"
	} else {
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || n < 0 || n > 4 {
			return parsePriorityFailedCmd(buf, dm.gen)
		}
		priority = &n
	}
	pid, ref, actor, gen := dm.scopePID, dm.issue.ShortID, dm.actor, dm.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.SetPriority(ctx, pid, ref, priority, actor)
		return mutationDoneMsg{
			origin: "detail", gen: gen, kind: kind, resp: resp, err: err,
		}
	}
}

// parsePriorityFailedCmd is the priority-prompt analog of parseFailedCmd:
// surfaces a parse error through the standard mutationDoneMsg path so
// the detail status line picks it up.
func parsePriorityFailedCmd(input string, gen int64) tea.Cmd {
	return func() tea.Msg {
		return mutationDoneMsg{
			origin: "detail",
			gen:    gen,
			kind:   "priority.set",
			err:    fmt.Errorf("parse %q failed: expected 0..4 or '-' to clear", input),
		}
	}
}

// parseFailedCmd surfaces a parse error as a synthetic mutationDoneMsg
// so the standard error-handling path renders the status line. gen is
// captured so the parse-error toast respects the same scoping rule as
// real mutations.
func parseFailedCmd(kind, input string, gen int64) tea.Cmd {
	return func() tea.Msg {
		return mutationDoneMsg{
			origin: "detail",
			gen:    gen,
			kind:   "link." + kind,
			err:    fmt.Errorf("parse %q failed: expected a short_id (e.g. abc4)", input),
		}
	}
}
