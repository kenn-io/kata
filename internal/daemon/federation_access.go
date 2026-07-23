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
