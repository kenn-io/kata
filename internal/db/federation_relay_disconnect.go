package db

import "context"

// RelaySelfRevoker permits only removal of the exact project-and-peer grant
// identified by its transport secret. It grants no read or write authority and
// intentionally permits cleanup after parent expiry or membership revocation.
type RelaySelfRevoker interface {
	RevokeOwnRelayEnrollment(context.Context, string, int64, string) error
}
