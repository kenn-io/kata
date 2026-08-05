package db

// FederationEventAffectsLinks reports whether an event type can change
// federated link state, or the issue-to-project endpoint resolution that link
// reconciliation depends on.
//
// Link reconciliation folds every event of every federated project in the
// binding group, so its cost scales with the whole group rather than with the
// batch being ingested. A batch whose events cannot touch link state leaves the
// reconciled result identical, so that fold can be skipped for it.
//
// The set is deliberately conservative: it covers the fold cases that write
// FoldProjection.Links (issue creation/snapshot payloads carry links, the
// explicit link events, and the author rewrite that restamps link authors) plus
// the issue lifecycle events that move an issue between projects or change
// whether its endpoint resolves at all.
func FederationEventAffectsLinks(eventType string) bool {
	switch eventType {
	case "issue.created",
		"issue.snapshot",
		"issue.linked",
		"issue.unlinked",
		"issue.links_changed",
		"issue.moved",
		"issue.soft_deleted",
		"issue.restored",
		"project.author_rewritten":
		return true
	default:
		return false
	}
}
