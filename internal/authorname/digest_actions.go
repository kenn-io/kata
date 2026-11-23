package authorname

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/kata/pkg/client/generated"
)

// actionOrder lists the token kinds in the daemon's renderActions order
// (internal/daemon/handlers_digest.go). Link tokens are "<type>:<id>" and
// unlink tokens "un<type>:<id>", so they take the ranks left over once the
// named kinds are matched.
var actionOrder = []string{
	"created", "commented", "edited", "assigned", "unassigned", "priority",
	"priority_cleared", "labeled", "unlabeled", "", "", "unblocks",
	"reopened", "closed", "deleted", "restored", "other",
}

const (
	linkRank   = 9
	unlinkRank = 10
)

// singleValued are the token kinds renderActions emits at most once per
// summary. Two rows that disagree on one of them cannot be combined.
var singleValued = []string{"assigned:", "priority:", "closed:"}

func actionRank(token string) int {
	kind, _, _ := strings.Cut(token, ":")
	if i := slices.Index(actionOrder, kind); i >= 0 && kind != "" {
		return i
	}
	if strings.HasPrefix(kind, "un") {
		return unlinkRank
	}
	return linkRank
}

// countToken parses "name" (count 1) or "name:N".
func countToken(token, name string) (int, bool) {
	if token == name {
		return 1, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(token, name+":"))
	return n, strings.HasPrefix(token, name+":") && err == nil
}

func actionsConflict(a, b []string) bool {
	for _, kind := range singleValued {
		for _, x := range a {
			for _, y := range b {
				if strings.HasPrefix(x, kind) && strings.HasPrefix(y, kind) && strings.ToLower(x) != strings.ToLower(y) {
					return true
				}
			}
		}
	}
	return false
}

// mergeActions combines two action lists the way the daemon would render one
// summary: counts are summed, other tokens are deduped, and the daemon's
// order is restored. "-" only survives when nothing else does.
func mergeActions(a, b []string) []string {
	var out []string
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, token := range slices.Concat(a, b) {
		if token == "-" {
			continue
		}
		if name, n := countedAction(token); name != "" {
			counts[name] += n
			token = name
		}
		key := strings.ToLower(token)
		if !seen[key] {
			seen[key] = true
			out = append(out, token)
		}
	}
	for i, token := range out {
		if n := counts[token]; n > 1 || token == "other" {
			out[i] = token + ":" + strconv.Itoa(n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return actionRank(out[i]) < actionRank(out[j]) })
	if len(out) == 0 && len(a)+len(b) > 0 {
		return []string{"-"}
	}
	return out
}

// countedAction returns the name and count of a "commented[:N]" or "other:N"
// token, or "" for any other token.
func countedAction(token string) (string, int) {
	for _, name := range []string{"commented", "other"} {
		if n, ok := countToken(token, name); ok {
			return name, n
		}
	}
	return "", 0
}

// mergeIssues folds rows into dst. A row joins the first existing row for the
// same project and issue whose single-valued actions don't conflict with it.
func mergeIssues(dst, rows []generated.DigestIssueActions) []generated.DigestIssueActions {
	for _, row := range rows {
		merged := false
		for i := range dst {
			if dst[i].ProjectID == row.ProjectID && dst[i].IssueUID == row.IssueUID && !actionsConflict(dst[i].Actions, row.Actions) {
				dst[i].Actions = mergeActions(dst[i].Actions, row.Actions)
				merged = true
				break
			}
		}
		if !merged {
			dst = append(dst, row)
		}
	}
	return dst
}

func addTotals(a *generated.DigestTotals, b generated.DigestTotals) {
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
