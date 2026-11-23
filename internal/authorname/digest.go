package authorname

import (
	"sort"
	"strconv"
	"strings"

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
	type issueKey struct {
		group   int
		project int64
		uid     string
	}
	issues := make(map[issueKey]int)
	for _, actor := range actors {
		name := Display(actor.Actor)
		i, ok := indices[name]
		if !ok {
			indices[name] = len(groups)
			i = len(groups)
			groups = append(groups, generated.DigestActorEntry{Actor: name})
		}
		group := &groups[i]
		for _, issue := range actor.Issues {
			key := issueKey{i, issue.ProjectID, issue.IssueUID}
			if index, found := issues[key]; found {
				group.Issues[index].Actions = mergeDigestActions(group.Issues[index].Actions, issue.Actions)
			} else {
				issues[key] = len(group.Issues)
				issue.Actions = append([]string(nil), issue.Actions...)
				group.Issues = append(group.Issues, issue)
			}
		}
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
		sort.Slice(issues, func(a, b int) bool {
			if issues[a].ProjectID != issues[b].ProjectID {
				return issues[a].ProjectID < issues[b].ProjectID
			}
			return issues[a].IssueUID < issues[b].IssueUID
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Actor < groups[j].Actor })
	return groups
}

// mergeDigestActions retains action tokens and adds the counts the daemon summarizes.
func mergeDigestActions(dst, src []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, action := range dst {
		seen[strings.ToLower(action)] = true
	}
	for _, action := range src {
		kind, count := digestActionCount(action)
		if count > 0 {
			merged := false
			for i, existing := range dst {
				existingKind, existingCount := digestActionCount(existing)
				if kind == existingKind && existingCount > 0 {
					delete(seen, strings.ToLower(existing))
					dst[i] = kind + ":" + strconv.Itoa(existingCount+count)
					seen[strings.ToLower(dst[i])] = true
					merged = true
					break
				}
			}
			if merged {
				continue
			}
		} else {
			key := strings.ToLower(action)
			if seen[key] {
				continue
			}
		}
		seen[strings.ToLower(action)] = true
		dst = append(dst, action)
	}
	sort.SliceStable(dst, func(i, j int) bool { return digestActionOrder(dst[i]) < digestActionOrder(dst[j]) })
	return dst
}

// Match the daemon's renderActions order when combining actor summaries.
func digestActionOrder(action string) int {
	kind, _, _ := strings.Cut(strings.ToLower(action), ":")
	switch kind {
	case "created":
		return 0
	case "commented":
		return 1
	case "edited":
		return 2
	case "assigned":
		return 3
	case "unassigned":
		return 4
	case "priority":
		return 5
	case "priority_cleared":
		return 6
	case "labeled":
		return 7
	case "unlabeled":
		return 8
	case "unparent", "unblocked_by", "unrelated":
		return 10
	case "unblocks":
		return 11
	case "reopened":
		return 12
	case "closed":
		return 13
	case "deleted":
		return 14
	case "restored":
		return 15
	case "other":
		return 16
	default:
		return 9
	}
}

func digestActionCount(action string) (string, int) {
	if action == "commented" {
		return "commented", 1
	}
	kind, value, ok := strings.Cut(action, ":")
	if !ok || (kind != "commented" && kind != "other") {
		return "", 0
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 1 {
		return "", 0
	}
	return kind, count
}
