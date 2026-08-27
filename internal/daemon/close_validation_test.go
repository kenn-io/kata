package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/api"
)

// Test messages are sized to meet the per-reason length floors in spec §3.4
// (done/audit-no-change: 40, wontfix: 60, duplicate/superseded: 20) so each
// test exercises the matrix in §3.5 rather than the length check. The short-
// message and trivial-message tests deliberately fall under the floors.

func TestValidateCloseInput_DoneRequiresImplementationEvidence(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests on Safari and Chrome.", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "evidence required")
}

func TestValidateCloseInput_DoneAcceptsCommit(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests on Safari and Chrome.",
		[]api.Evidence{{Type: api.EvidenceCommit, SHA: "abc1234"}})
	assert.NoError(t, err)
}

func TestValidateCloseInput_DoneAcceptsExternal(t *testing.T) {
	err := ValidateCloseInput("done",
		"Arranged the meeting by email and sent the calendar hold.",
		[]api.Evidence{{
			Type:    api.EvidenceExternal,
			Account: "email thread archived; calendar hold sent",
		}})
	assert.NoError(t, err)
}

func TestValidateCloseInput_DoneRejectsEmptyExternalAccount(t *testing.T) {
	err := ValidateCloseInput("done",
		"Arranged the meeting by email and sent the calendar hold.",
		[]api.Evidence{{Type: api.EvidenceExternal, Account: "  "}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "external")
	assert.Contains(t, err.Error(), "account")
}

func TestValidateCloseInput_DoneReportsDisallowedEvidenceBeforeRequired(t *testing.T) {
	err := ValidateCloseInput("done",
		"Reviewed the work carefully and confirmed no code change was needed.",
		[]api.Evidence{{Type: api.EvidenceNoChangeAudit, Rationale: "metadata-only"}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), `evidence type "no-change-audit" not allowed`)
	assert.NotContains(t, err.Error(), "evidence required")
}

func TestValidateCloseInput_DoneRejectsDuplicateOfAlongside(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests on Safari and Chrome.",
		[]api.Evidence{
			{Type: api.EvidenceCommit, SHA: "abc1234"},
			{Type: api.EvidenceDuplicateOf, IssueRef: "abc4"},
		})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate-of")
}

func TestValidateCloseInput_WontfixZeroEvidence(t *testing.T) {
	err := ValidateCloseInput("wontfix",
		"Decided not to fix this; does not match product direction or roadmap priorities.",
		nil)
	assert.NoError(t, err)
}

func TestValidateCloseInput_WontfixRejectsEvidence(t *testing.T) {
	err := ValidateCloseInput("wontfix",
		"Decided not to fix this; does not match product direction or roadmap priorities.",
		[]api.Evidence{{Type: api.EvidenceCommit, SHA: "abc1234"}})
	assert.Error(t, err)
}

func TestValidateCloseInput_DuplicateRequiresExactlyOneDuplicateOf(t *testing.T) {
	err := ValidateCloseInput("duplicate", "Same Safari race; merge there.",
		[]api.Evidence{{Type: api.EvidenceDuplicateOf, IssueRef: "abc4"}})
	assert.NoError(t, err)
}

func TestValidateCloseInput_DuplicateRejectsExtraEvidence(t *testing.T) {
	err := ValidateCloseInput("duplicate", "Same Safari race; merge there.",
		[]api.Evidence{
			{Type: api.EvidenceDuplicateOf, IssueRef: "abc4"},
			{Type: api.EvidenceCommit, SHA: "abc1234"},
		})
	assert.Error(t, err)
}

func TestValidateCloseInput_AuditNoChangeAllowsReviewedPaths(t *testing.T) {
	err := ValidateCloseInput("audit-no-change",
		"Reviewed schema, queries, and tests; no code change required.",
		[]api.Evidence{
			{Type: api.EvidenceNoChangeAudit, Rationale: "metadata-only"},
			{Type: api.EvidenceReviewedPaths, Paths: []string{"a.go", "b.go"}},
		})
	assert.NoError(t, err)
}

func TestValidateCloseInput_MessageTooShortForDone(t *testing.T) {
	err := ValidateCloseInput("done", "Fixed it",
		[]api.Evidence{{Type: api.EvidenceCommit, SHA: "abc1234"}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestValidateCloseInput_MessageTrivialDenied(t *testing.T) {
	// 40-char message that normalizes exactly to "done".
	msg := "   done   "
	for len(msg) < 40 {
		msg += " "
	}
	err := ValidateCloseInput("done", msg,
		[]api.Evidence{{Type: api.EvidenceCommit, SHA: "abc1234"}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "trivial")
}

// Payload-shape tests: each evidence type must reject its degenerate empty
// form even when the per-reason count matrix would otherwise accept it.

func TestValidateCloseInput_CommitWithEmptySHARejected(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests thoroughly today.",
		[]api.Evidence{{Type: api.EvidenceCommit, SHA: ""}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "commit")
	assert.Contains(t, err.Error(), "sha")
}

func TestValidateCloseInput_CommitWithMalformedSHARejected(t *testing.T) {
	// Non-hex characters fail; too short (<7) fails; too long (>40) fails.
	cases := []struct {
		name string
		sha  string
	}{
		{"too short", "abc"},
		{"too short five hex", "12345"},
		{"non-hex chars", "abcXYZ123"},
		{"contains space", "abc 1234"},
		{"plain word", "fixed"},
		{"too long", strings.Repeat("a", 41)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCloseInput("done",
				"Fixed the bug and ran tests thoroughly today.",
				[]api.Evidence{{Type: api.EvidenceCommit, SHA: tc.sha}})
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "commit")
		})
	}
}

func TestValidateCloseInput_CommitWithValidSHAsAccepted(t *testing.T) {
	for _, sha := range []string{
		"abc1234",          // 7 hex (minimum)
		"ABC1234",          // uppercase hex
		"AbCdEf0",          // mixed case
		"abcdef0123456789", // 16 hex
		"abcdef0123456789abcdef0123456789abcdef01", // 40 hex (maximum)
	} {
		t.Run(sha, func(t *testing.T) {
			err := ValidateCloseInput("done",
				"Fixed the bug and ran tests thoroughly today.",
				[]api.Evidence{{Type: api.EvidenceCommit, SHA: sha}})
			assert.NoError(t, err)
		})
	}
}

func TestValidateCloseInput_PRWithEmptyURLRejected(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests thoroughly today.",
		[]api.Evidence{{Type: api.EvidencePR, URL: "   "}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pr")
	assert.Contains(t, err.Error(), "url")
}

func TestValidateCloseInput_TestWithEmptyCommandRejected(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests thoroughly today.",
		[]api.Evidence{{Type: api.EvidenceTest, Command: ""}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "test")
	assert.Contains(t, err.Error(), "command")
}

func TestValidateCloseInput_ReviewedPathsWithEmptySliceRejected(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests thoroughly today.",
		[]api.Evidence{{Type: api.EvidenceReviewedPaths, Paths: nil}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reviewed-paths")
	assert.Contains(t, err.Error(), "paths")
}

func TestValidateCloseInput_ReviewedPathsWithBlankEntryRejected(t *testing.T) {
	err := ValidateCloseInput("done",
		"Fixed the bug and ran tests thoroughly today.",
		[]api.Evidence{{Type: api.EvidenceReviewedPaths, Paths: []string{"ok.go", "  "}}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reviewed-paths")
	assert.Contains(t, err.Error(), "empty")
}

func TestValidateCloseInput_NoChangeAuditWithEmptyRationaleRejected(t *testing.T) {
	err := ValidateCloseInput("audit-no-change",
		"Reviewed schema, queries, and tests; no code change required.",
		[]api.Evidence{{Type: api.EvidenceNoChangeAudit, Rationale: ""}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no-change-audit")
	assert.Contains(t, err.Error(), "rationale")
}

func TestValidateCloseInput_DuplicateOfWithEmptyRefRejected(t *testing.T) {
	err := ValidateCloseInput("duplicate", "Same Safari race; merge there.",
		[]api.Evidence{{Type: api.EvidenceDuplicateOf, IssueRef: ""}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate-of")
	assert.Contains(t, err.Error(), "issue_ref")
}

func TestValidateCloseInput_SupersededByWithEmptyRefRejected(t *testing.T) {
	err := ValidateCloseInput("superseded", "Replaced by the rewrite tracked elsewhere.",
		[]api.Evidence{{Type: api.EvidenceSupersededBy, IssueRef: ""}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "superseded-by")
	assert.Contains(t, err.Error(), "issue_ref")
}
