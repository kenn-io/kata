package db

import "context"

// RelayLifecycleStore exposes the same native preflight used at destructive
// commit. Call before external credential teardown; normal pause retains intent.
type RelayLifecycleStore interface {
	ValidateRelayLifecycle(context.Context, int64) error
}

// RelayDisconnectFenceStore validates retained relay work and disables relay
// writes in the same native transaction before upstream revocation.
type RelayDisconnectFenceStore interface {
	FenceRelayDisconnect(context.Context, int64) error
}
