package authorname

import (
	"sort"

	"go.kenn.io/kata/pkg/client/generated"
)

// DigestFilters collects the stored actors behind each displayed name.
func DigestFilters(actors []generated.DigestActorEntry) map[string][]string {
	filters := make(map[string][]string, len(actors))
	for _, actor := range actors {
		name := Display(actor.Actor)
		filters[name] = append(filters[name], actor.Actor)
	}
	return filters
}

// GroupDigest combines actors that share a displayed name.
func GroupDigest(actors []generated.DigestActorEntry) []generated.DigestActorEntry {
	groups := make([]generated.DigestActorEntry, 0, len(actors))
	indices := make(map[string]int, len(actors))
	for _, actor := range actors {
		name := Display(actor.Actor)
		i, ok := indices[name]
		if !ok {
			indices[name] = len(groups)
			i = len(groups)
			groups = append(groups, generated.DigestActorEntry{Actor: name})
		}
		group := &groups[i]
		group.Issues = append(group.Issues, actor.Issues...)
		a, b := &group.Totals, actor.Totals
		a.Assigned += b.Assigned
		a.Closed += b.Closed
		a.Commented += b.Commented
		a.Created += b.Created
		a.Deleted += b.Deleted
		a.Edited += b.Edited
		a.Labeled += b.Labeled
		a.Linked += b.Linked
		a.Other += b.Other
		a.PriorityCleared += b.PriorityCleared
		a.PrioritySet += b.PrioritySet
		a.Reopened += b.Reopened
		a.Restored += b.Restored
		a.Unassigned += b.Unassigned
		a.Unblocked += b.Unblocked
		a.Unlabeled += b.Unlabeled
		a.Unlinked += b.Unlinked
	}
	for i := range groups {
		issues := groups[i].Issues
		sort.SliceStable(issues, func(a, b int) bool {
			if issues[a].ProjectID != issues[b].ProjectID {
				return issues[a].ProjectID < issues[b].ProjectID
			}
			return issues[a].IssueUID < issues[b].IssueUID
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Actor < groups[j].Actor })
	return groups
}
