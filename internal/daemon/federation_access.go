package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/db"
)

var errHostFederationAccessUnavailable = errors.New("host federation access unavailable")

// ErrHostFederationAdmissionLimited is the internal adapter form of a host's
// bounded authenticated-request admission signal.
var ErrHostFederationAdmissionLimited = errors.New("host federation request admission limited")

// HostFederationOperation identifies one authenticated transport operation.
type HostFederationOperation struct {
	ID       string
	Mutation bool
}

// federationTransportOperations is the source of truth for the transport-auth
// facts of every route a project-scoped federation enrollment can
// authenticate. Mutation here answers "may this request write, so compute and
// require a transaction fence?" — a narrower question than the capability class
// hostOperationPolicies declares for the same route, which is why this table is
// its own source of truth and not derived from that registry.
var federationTransportOperations = map[string]HostFederationOperation{
	"acquireIssueLease": {ID: "acquireIssueLease", Mutation: true},
	"renewIssueLease":   {ID: "renewIssueLease", Mutation: true},
	"releaseIssueLease": {ID: "releaseIssueLease", Mutation: true},
	// forceReleaseIssueLease never authenticates an enrollment credential
	// (resolveClaimPrincipal receives allowEnrollment=false); the entry exists
	// so its call site names one table like every other lease route.
	"forceReleaseIssueLease": {ID: "forceReleaseIssueLease", Mutation: true},
	// Fenced despite its read policy: handleClaimStatus expires timed claims
	// and emits events, so this GET writes.
	"getIssueLeaseStatus": {ID: "getIssueLeaseStatus", Mutation: true},
	// Fenced despite its read policy: projectFederationBody can call
	// RefreshProjectFederationBaseline, so this GET writes.
	"getFederationProjectMetadata":  {ID: "getFederationProjectMetadata", Mutation: true},
	"pollFederationProjectEvents":   {ID: "pollFederationProjectEvents"},
	"ingestFederationProjectEvents": {ID: "ingestFederationProjectEvents", Mutation: true},
}

// federationTransportOperation returns the transport-auth facts for
// operationID. An unknown ID is a programming error: returning a zero value
// would authorize the request as a non-mutating operation and silently drop its
// transaction fence, so this panics instead, mirroring registerHostOperations'
// duplicate-registration discipline.
func federationTransportOperation(operationID string) HostFederationOperation {
	operation, ok := federationTransportOperations[operationID]
	if !ok {
		panic("unknown federation transport operation: " + operationID)
	}
	return operation
}

// HostFederationAccessRequest carries native credential and project facts to
// the public service adapter.
type HostFederationAccessRequest struct {
	Enrollment db.FederationEnrollment
	Project    db.Project
	Capability string
	Operation  HostFederationOperation
}

// HostFederationAccessDecision contains an optional mutation fence.
type HostFederationAccessDecision struct {
	TransactionFence db.TransactionFence
}

// HostFederationAccessController adapts the public service hook without
// exposing daemon or storage packages to callers.
type HostFederationAccessController interface {
	AuthorizeFederation(
		context.Context,
		HostFederationAccessRequest,
	) (HostFederationAccessDecision, error)
}
