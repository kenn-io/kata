package db

import "sort"

// ReconcileFoldStatusIntent advances private write intent from the previously
// effective status writer. Late historical events cannot replace or cancel an
// already pending mutation. Newly effective events use the ordinary fold rules
// so same-state restatements retain intent and opposite transitions clear it.
func ReconcileFoldStatusIntent(previous, current FoldIssue, pendingUID string, accepted []FoldEvent) string {
	if current.UID == "" {
		return ""
	}
	previous.UID = current.UID
	previous.StatusIntentUID = pendingUID
	if previous.Status == "" {
		previous.Status = "open"
	}
	projection := FoldEvents(nil)
	projection.Issues[current.UID] = previous
	ordered := append([]FoldEvent(nil), accepted...)
	sort.SliceStable(ordered, func(i, j int) bool { return compareClock(clockOf(ordered[i]), clockOf(ordered[j])) < 0 })
	for _, event := range ordered {
		if compareClock(clockOf(event), previous.StatusClock) <= 0 || issueUID(event, PayloadMap(event.Payload)) != current.UID {
			continue
		}
		switch event.Type {
		case "issue.created", "issue.snapshot", "issue.updated", "issue.closed", "issue.reopened":
			projection.apply(event)
		}
	}
	effective := projection.Issues[current.UID]
	if effective.Status != current.Status {
		return ""
	}
	if effective.StatusIntentUID == pendingUID || effective.StatusIntentUID == current.StatusIntentUID {
		return effective.StatusIntentUID
	}
	return ""
}
