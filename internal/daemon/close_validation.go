package daemon

import (
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/api"
)

// isHexSHA reports whether s is 7-40 lowercase or uppercase hex characters,
// matching git's abbreviated and full SHA-1 commit-object identifiers. The
// commit-evidence shape check uses this to reject obvious non-SHAs like
// "fixed", "tbd", or a PR URL accidentally passed via --commit. Verifying
// that the SHA actually resolves in a repository requires shelling out to
// git, which the daemon cannot do (it has no workspace), so this stays a
// pure syntax check.
func isHexSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// validateEvidenceShape rejects evidence items whose payload is structurally
// empty for their declared type. The matrix check that follows only counts
// types — without this, payloads like {"type":"commit"} (empty SHA) or
// {"type":"duplicate-of","issue":0} would slip through.
func validateEvidenceShape(index int, e api.Evidence) error {
	switch e.Type {
	case api.EvidenceCommit:
		sha := strings.TrimSpace(e.SHA)
		if sha == "" {
			return fmt.Errorf("evidence[%d] commit requires non-empty sha", index)
		}
		if !isHexSHA(sha) {
			return fmt.Errorf("evidence[%d] commit sha %q is not a valid git SHA "+
				"(expected 7-40 hex characters)", index, sha)
		}
	case api.EvidencePR:
		if strings.TrimSpace(e.URL) == "" {
			return fmt.Errorf("evidence[%d] pr requires non-empty url", index)
		}
	case api.EvidenceTest:
		if strings.TrimSpace(e.Command) == "" {
			return fmt.Errorf("evidence[%d] test requires non-empty command", index)
		}
	case api.EvidenceReviewedPaths:
		if len(e.Paths) == 0 {
			return fmt.Errorf("evidence[%d] reviewed-paths requires non-empty paths", index)
		}
		for j, p := range e.Paths {
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("evidence[%d] reviewed-paths entry %d is empty", index, j)
			}
		}
	case api.EvidenceExternal:
		if strings.TrimSpace(e.Account) == "" {
			return fmt.Errorf("evidence[%d] external requires non-empty account", index)
		}
	case api.EvidenceNoChangeAudit:
		if strings.TrimSpace(e.Rationale) == "" {
			return fmt.Errorf("evidence[%d] no-change-audit requires non-empty rationale", index)
		}
	case api.EvidenceDuplicateOf:
		if strings.TrimSpace(e.IssueRef) == "" {
			return fmt.Errorf("evidence[%d] duplicate-of requires non-empty issue_ref", index)
		}
	case api.EvidenceSupersededBy:
		if strings.TrimSpace(e.IssueRef) == "" {
			return fmt.Errorf("evidence[%d] superseded-by requires non-empty issue_ref", index)
		}
	}
	return nil
}

// messageFloor returns the minimum character count required for a close
// message under the given reason, per spec §3.4.
func messageFloor(reason string) int {
	switch reason {
	case "done", "audit-no-change":
		return 40
	case "wontfix":
		return 60
	case "duplicate", "superseded":
		return 20
	default:
		return 40
	}
}

// TrivialMessages is the exact-match deny-list from spec §3.4. Kept short
// on purpose; if it grows materially, move to config. Exported so the
// repeated-message guard (§3.10) can share the same set.
var TrivialMessages = map[string]struct{}{
	"done": {}, "fixed": {}, "complete": {}, "completed": {},
	"ok": {}, "okay": {}, "yes": {}, "no": {},
	"n/a": {}, "na": {}, "skip": {}, "nope": {},
}

// NormalizeMessage applies the cheap normalization used by both the
// substance check (§3.4) and the repeated-message guard (§3.10).
// Exported so Task 12's repeated-message guard can reuse it.
func NormalizeMessage(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ToLower(s)
	s = strings.TrimRight(s, ".?!")
	return s
}

// ValidateCloseInput enforces the substance check on message and the
// per-reason evidence matrix from spec §3.5. Returns a descriptive error
// on the first violation; the daemon handler maps it to a 400 response.
//
// Trivial-phrase rejection is checked before the length floor so a message
// normalizing to a deny-listed word (even if padded to look long) surfaces
// the more specific "trivial" error rather than "too short".
//
// Target-existence for duplicate-of and superseded-by (that the referenced
// issue actually exists in the project) is intentionally NOT checked here:
// it requires database access, while this function stays pure. The daemon
// handler resolves the target alongside the existing close path.
func ValidateCloseInput(reason, message string, evidence []api.Evidence) error {
	norm := NormalizeMessage(message)
	if _, isTrivial := TrivialMessages[norm]; isTrivial {
		return fmt.Errorf("message rejected as trivial (%q). "+
			"If the work is not actually complete, do not close — use "+
			"`kata edit <ref> --label needs-review` and comment what remains", norm)
	}
	if len(norm) < messageFloor(reason) {
		return fmt.Errorf("message too short for reason=%s (need >=%d chars after normalization, got %d)",
			reason, messageFloor(reason), len(norm))
	}

	for i, e := range evidence {
		if err := validateEvidenceShape(i, e); err != nil {
			return err
		}
	}

	counts := map[api.EvidenceType]int{}
	for _, e := range evidence {
		counts[e.Type]++
	}
	has := func(t api.EvidenceType) bool { return counts[t] > 0 }
	onlyAllow := func(allowed ...api.EvidenceType) error {
		permit := map[api.EvidenceType]struct{}{}
		for _, a := range allowed {
			permit[a] = struct{}{}
		}
		for t := range counts {
			if _, ok := permit[t]; !ok {
				return fmt.Errorf("evidence type %q not allowed for reason=%s", t, reason)
			}
		}
		return nil
	}

	switch reason {
	case "done":
		if err := onlyAllow(api.EvidenceCommit, api.EvidencePR,
			api.EvidenceTest, api.EvidenceReviewedPaths, api.EvidenceExternal); err != nil {
			return err
		}
		if !has(api.EvidenceCommit) && !has(api.EvidencePR) &&
			!has(api.EvidenceTest) && !has(api.EvidenceReviewedPaths) &&
			!has(api.EvidenceExternal) {
			return fmt.Errorf("evidence required for reason=done. " +
				"Accepted: commit:<sha>, pr:<url>, test:<cmd>, reviewed-paths:<path>, " +
				"external:<account>. " +
				"If the work is not actually complete, do not close — use " +
				"`kata edit <ref> --label needs-review` and comment what remains")
		}
	case "wontfix":
		if len(evidence) > 0 {
			return fmt.Errorf("evidence not allowed for reason=wontfix")
		}
	case "duplicate":
		if counts[api.EvidenceDuplicateOf] != 1 {
			return fmt.Errorf("reason=duplicate requires exactly one duplicate-of evidence item")
		}
		if err := onlyAllow(api.EvidenceDuplicateOf); err != nil {
			return err
		}
	case "superseded":
		if counts[api.EvidenceSupersededBy] != 1 {
			return fmt.Errorf("reason=superseded requires exactly one superseded-by evidence item")
		}
		if err := onlyAllow(api.EvidenceSupersededBy); err != nil {
			return err
		}
	case "audit-no-change":
		if counts[api.EvidenceNoChangeAudit] != 1 {
			return fmt.Errorf("reason=audit-no-change requires exactly one no-change-audit evidence item")
		}
		if err := onlyAllow(api.EvidenceNoChangeAudit, api.EvidenceReviewedPaths); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown reason %q", reason)
	}
	return nil
}
