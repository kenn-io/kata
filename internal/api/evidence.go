//nolint:revive // package-name lint flagged externally; api is the fixed name.
package api

import (
	"encoding/json"
	"fmt"
)

// EvidenceType is one of the typed-union tags carried on a close action.
// The set is intentionally closed; see spec §3.3.
type EvidenceType string

// The closed set of EvidenceType values that may appear in a close
// action's evidence array. See spec §3.3 and the per-reason validation
// matrix in §3.5.
const (
	EvidenceCommit        EvidenceType = "commit"
	EvidencePR            EvidenceType = "pr"
	EvidenceTest          EvidenceType = "test"
	EvidenceReviewedPaths EvidenceType = "reviewed-paths"
	EvidenceExternal      EvidenceType = "external"
	EvidenceNoChangeAudit EvidenceType = "no-change-audit"
	EvidenceDuplicateOf   EvidenceType = "duplicate-of"
	EvidenceSupersededBy  EvidenceType = "superseded-by"
)

// Evidence is a typed-union element of the close action's evidence array.
// Only the fields appropriate to Type are populated; per-reason validation
// in internal/daemon/close_validation.go enforces shape.
type Evidence struct {
	Type EvidenceType `json:"type"`

	SHA       string   `json:"sha,omitempty"`       // commit
	URL       string   `json:"url,omitempty"`       // pr
	Command   string   `json:"command,omitempty"`   // test
	Paths     []string `json:"paths,omitempty"`     // reviewed-paths
	Account   string   `json:"account,omitempty"`   // external
	Rationale string   `json:"rationale,omitempty"` // no-change-audit
	IssueRef  string   `json:"issue_ref,omitempty"` // duplicate-of, superseded-by
}

var validEvidenceTypes = map[EvidenceType]struct{}{
	EvidenceCommit:        {},
	EvidencePR:            {},
	EvidenceTest:          {},
	EvidenceReviewedPaths: {},
	EvidenceExternal:      {},
	EvidenceNoChangeAudit: {},
	EvidenceDuplicateOf:   {},
	EvidenceSupersededBy:  {},
}

// UnmarshalJSON rejects unknown evidence types early so daemon validation
// never has to special-case malformed wire input.
func (e *Evidence) UnmarshalJSON(bs []byte) error {
	type raw Evidence
	var r raw
	if err := json.Unmarshal(bs, &r); err != nil {
		return err
	}
	if _, ok := validEvidenceTypes[r.Type]; !ok {
		return fmt.Errorf("evidence: unknown type %q", r.Type)
	}
	*e = Evidence(r)
	return nil
}
