package kata

import (
	"context"
	"errors"
)

// ErrFederationAdmissionLimited asks Kata to reject an authenticated
// federation request with 429 before reading a large request body. Embedding
// hosts may return it when their bounded request admission is full.
var ErrFederationAdmissionLimited = errors.New("kata: federation request admission limited")

// FederationCapability is the transport authority requested by one
// authenticated federation operation.
type FederationCapability string

// Federation transport capabilities.
const (
	FederationCapabilityPull  FederationCapability = "pull"
	FederationCapabilityPush  FederationCapability = "push"
	FederationCapabilityClaim FederationCapability = "claim"
)

// FederationOperation describes the authenticated transport operation. ID is
// a stable Kata operation identifier. Mutation is true when the operation can
// change stored task, cursor, or lease state.
type FederationOperation struct {
	ID       string
	Mutation bool
}

// FederationAccessRequest contains the Kata-authenticated enrollment and
// project facts supplied to a mounting application's additional authorization
// boundary. It never contains the plaintext credential or its stored hash.
type FederationAccessRequest struct {
	Enrollment FederationEnrollment
	Project    Project
	Capability FederationCapability
	Operation  FederationOperation
}

// FederationAccessDecision carries the in-transaction authorization check for
// a federation mutation. Read-only operations may return the zero value.
type FederationAccessDecision struct {
	TransactionFence TransactionFence
}

// FederationAccessController adds host-owned authorization after Kata has
// authenticated a project-scoped enrollment. Returning ErrAccessDenied makes
// the credential unusable without disclosing which outside authority changed.
// Mutating operations require a TransactionFence in the returned decision.
type FederationAccessController interface {
	AuthorizeFederation(
		context.Context,
		FederationAccessRequest,
	) (FederationAccessDecision, error)
}
