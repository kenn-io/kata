package db

import "slices"

// EventIssueUIDs lists, once each and in first-seen order, the issue and
// related-issue UIDs of events that do not yet name the local issue row.
// Federated events are stored by UID; materialization creates the rows they
// name afterwards.
func EventIssueUIDs(events []Event) []string {
	var uids []string
	add := func(id *int64, uid *string) {
		if id == nil && uid != nil && *uid != "" && !slices.Contains(uids, *uid) {
			uids = append(uids, *uid)
		}
	}
	for _, event := range events {
		add(event.IssueID, event.IssueUID)
		add(event.RelatedIssueID, event.RelatedIssueUID)
	}
	return uids
}

// ResolveEventIssueIDs fills each event's missing IssueID and RelatedIssueID
// from local issue IDs keyed by UID, so consumers such as hooks can resolve
// the issue an event created or changed. An ID already present, or a UID with
// no local issue, is left as it is.
func ResolveEventIssueIDs(events []Event, local map[string]int64) {
	resolve := func(id **int64, uid *string) {
		if *id != nil || uid == nil {
			return
		}
		if found, ok := local[*uid]; ok {
			*id = &found
		}
	}
	for i := range events {
		resolve(&events[i].IssueID, events[i].IssueUID)
		resolve(&events[i].RelatedIssueID, events[i].RelatedIssueUID)
	}
}
