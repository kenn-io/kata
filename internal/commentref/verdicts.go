package commentref

import (
	"cmp"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// Verification is the shared wire shape for a derived verdict summary.
type Verification = db.Verification

// Evaluate derives verdicts from an untruncated authorized graph. readable may
// further redact records; nil means every supplied row is readable. Rebuttals
// resolve the original dispute rather than becoming independent close blockers.
func Evaluate(comments []Record, issueUID string, readable func(uid string) bool) Verification {
	v := Verification{Claims: map[string]db.ClaimVerdict{}, Confirmed: []db.ConfirmedClaim{}, OpenRefutations: []db.OpenRefutation{}}
	byUID := make(map[string]Record, len(comments))
	incoming := make(map[string][]Record)
	for _, r := range comments {
		if readable == nil || readable(r.UID) {
			byUID[r.UID] = r
		}
	}
	for _, r := range comments {
		target, exists := byUID[r.ReplyToUID]
		_, visible := byUID[r.UID]
		if !exists || !visible {
			if exists && target.IssueUID == issueUID && (r.ReplyKind == "confirm" || r.ReplyKind == "refute" || r.ReplyKind == "supersede") {
				v.Redacted++
			}
			continue
		}
		incoming[r.ReplyToUID] = append(incoming[r.ReplyToUID], r)
	}
	superseded := func(uid string) bool {
		for _, r := range incoming[uid] {
			if r.ReplyKind == "supersede" {
				return true
			}
		}
		return false
	}
	// Unknown results (cycles or depth exhaustion) propagate to the original
	// dispute, which stays open. This prevents a deep chain hiding a blocker.
	type verdict struct{ open, known bool }
	var dispute func(Record, int, map[string]bool) verdict
	dispute = func(r Record, depth int, path map[string]bool) verdict {
		if superseded(r.UID) || superseded(r.ReplyToUID) {
			return verdict{false, true}
		}
		if depth >= 8 || path[r.UID] {
			return verdict{true, false}
		}
		path[r.UID] = true
		defer delete(path, r.UID)
		unknown := false
		for _, reply := range incoming[r.UID] {
			if reply.ReplyKind == "refute" {
				child := dispute(reply, depth+1, path)
				if child.known && child.open {
					return verdict{false, true}
				}
				unknown = unknown || !child.known
			}
		}
		return verdict{true, !unknown}
	}
	for _, target := range byUID {
		if target.IssueUID != issueUID {
			continue
		}
		claimVerdict := db.ClaimVerdict{}
		confirmations := 0
		hasOpenRefutation := false
		for _, r := range incoming[target.UID] {
			if r.ReplyKind == "confirm" || r.ReplyKind == "refute" {
				v.Activity = true
			}
			if r.ReplyKind == "confirm" && (r.Author != target.Author || r.Teammate != target.Teammate) {
				confirmations++
				claimVerdict.Confirmations = append(claimVerdict.Confirmations, verdictHandle(r, issueUID))
			}
			if r.ReplyKind != "refute" {
				continue
			}
			result := dispute(r, 0, map[string]bool{})
			if result.open || !result.known {
				hasOpenRefutation = true
				claimVerdict.OpenRefutations = append(claimVerdict.OpenRefutations, verdictHandle(r, issueUID))
				if r.Author != target.Author || r.Teammate != target.Teammate {
					claimVerdict.IndependentRefutations = append(claimVerdict.IndependentRefutations, verdictHandle(r, issueUID))
				}
				if target.ReplyKind == "refute" {
					continue
				}
				summary := []rune(strings.Join(strings.Fields(r.Body), " "))
				if len(summary) > 80 {
					summary = summary[:80]
				}
				v.OpenRefutations = append(v.OpenRefutations, db.OpenRefutation{Refutation: verdictHandle(r, issueUID), Target: verdictHandle(target, issueUID), Author: r.Author, Teammate: r.Teammate, Summary: string(summary)})
			}
		}
		slices.Sort(claimVerdict.Confirmations)
		slices.Sort(claimVerdict.OpenRefutations)
		slices.Sort(claimVerdict.IndependentRefutations)
		v.Claims[verdictHandle(target, issueUID)] = claimVerdict
		if confirmations > 0 && !hasOpenRefutation {
			v.Confirmed = append(v.Confirmed, db.ConfirmedClaim{Comment: verdictHandle(target, issueUID), Confirmations: confirmations})
		}
	}
	slices.SortFunc(v.Confirmed, func(a, b db.ConfirmedClaim) int { return cmp.Compare(a.Comment, b.Comment) })
	slices.SortFunc(v.OpenRefutations, func(a, b db.OpenRefutation) int { return cmp.Compare(a.Refutation, b.Refutation) })
	return v
}

func verdictHandle(r Record, issueUID string) string {
	handle := r.Handle
	if handle == "" {
		handle = "c:" + strings.ToLower(r.UID)
	}
	return HandleForIssue(handle, r.IssueUID, r.IssueShortID, issueUID)
}
