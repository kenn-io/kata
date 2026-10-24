package db

import (
	"errors"
	"go.kenn.io/kata/internal/uid"
)

// Admission is immutable local hop custody evidence, never root source
// acceptance or a substitute for a live authorization check.
type ScopedAdmissionEvidence struct {
	Version         int              `json:"version"`
	OperationUID    string           `json:"operation_uid"`
	OperationDigest string           `json:"operation_digest"`
	Principal       CustodyPrincipal `json:"principal"`
}

func (e ScopedAdmissionEvidence) Validate() error {
	if e.Version != 1 || !uid.Valid(e.OperationUID) || !scopedDigest(e.OperationDigest) {
		return errors.New("invalid scoped admission evidence")
	}
	return e.Principal.Validate()
}

// Stale retained predicates remain valid recovery data. Native use must refuse
// stale incarnations; restore compares equal incarnations' digests exactly.
type ScopedMutationPredicate struct {
	Version         int                       `json:"version"`
	Preparation     ScopedMutationPreparation `json:"preparation"`
	Incarnation     int64                     `json:"incarnation"`
	CanonicalDigest string                    `json:"canonical_digest"`
}

func (p ScopedMutationPredicate) Validate() error {
	if p.Version != 1 || p.Incarnation <= 0 || !scopedDigest(p.CanonicalDigest) {
		return errors.New("invalid scoped mutation predicate")
	}
	return p.Preparation.Validate()
}
