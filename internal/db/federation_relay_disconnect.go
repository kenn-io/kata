package db

import "context"

// RelaySelfRevoker permits only removal of the exact project-and-peer grant
// identified by its transport secret. It grants no read or write authority and
// intentionally permits cleanup after parent expiry or membership revocation.
type RelaySelfRevoker interface {
	RevokeOwnRelayEnrollment(context.Context, string, int64, string) error
}

// RelaySelfRevocationAuthenticator resolves only the retained relay grant ID
// for a token and project. It intentionally ignores grant activity, parent
// token activity, project visibility and membership so a signed cleanup retry
// can verify against the same enrollment after revocation.
type RelaySelfRevocationAuthenticator interface {
	RelaySelfRevocationEnrollmentID(context.Context, string, int64) (int64, error)
}
