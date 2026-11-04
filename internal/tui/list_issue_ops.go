package tui

// Delete (`X`) and move (`M`) are offered only on the selected row of the plain
// issue list. The detail view, the Inbox and a detail opened from Attention keep
// saved copies of an issue (Back history, the split pane, the return context)
// that a removal or a move would leave stale, so these keys are not offered
// there. One predicate decides both the footer and help and the dispatch.

// listIssueOpsHidden reports whether credentials or a saved browsing context
// prevent deleting and moving. Footer, help overlay, and chrome all read it.
func (m Model) listIssueOpsHidden() bool {
	return !m.mutationAllowed(false) || m.listIssueOpsBlockedByContext()
}

func (m Model) listIssueOpsBlockedByContext() bool {
	return m.inboxReturn != nil || m.attention.returnContext != nil
}

// listIssueTarget is the issue `X` or `M` acts on: the selected row of the
// plain issue list, and nothing in any other view, pane or context.
func (m Model) listIssueTarget() (formTarget, Issue, bool) {
	if (m.view != viewList && m.view != viewDetail) || !m.listIsActive() || m.listIssueOpsBlockedByContext() {
		return formTarget{}, Issue{}, false
	}
	return m.activeIssueTarget()
}

// helpKeymap is the keymap the help overlay lists: without the delete and move
// keys when they are not on offer.
func (m Model) helpKeymap() keymap {
	km := m.keymap
	if m.listIssueOpsHidden() {
		km.Delete, km.MoveIssue = key{}, key{}
	}
	return km
}
