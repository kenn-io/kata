package db

import "sort"

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
	_, ok := federationLinkAffectingEventTypes[eventType]
	return ok
}

var federationLinkAffectingEventTypes = map[string]struct{}{
	"issue.created":            {},
	"issue.snapshot":           {},
	"issue.linked":             {},
	"issue.unlinked":           {},
	"issue.links_changed":      {},
	"issue.moved":              {},
	"issue.soft_deleted":       {},
	"issue.restored":           {},
	"project.author_rewritten": {},
}

// FederationLinkAffectingEventTypes returns the event types FederationEventAffectsLinks
// accepts, sorted so the result is stable for query construction.
//
// A fold restricted to these types produces the same FoldProjection.Links as a
// fold over every event: no other case writes Links, the issue state that link
// author rewriting consults is established by the creation and move events that
// are included, and everything excluded touches only issue, comment, and label
// state that link reconciliation never reads.
func FederationLinkAffectingEventTypes() []string {
	out := make([]string, 0, len(federationLinkAffectingEventTypes))
	for eventType := range federationLinkAffectingEventTypes {
		out = append(out, eventType)
	}
	sort.Strings(out)
	return out
}
