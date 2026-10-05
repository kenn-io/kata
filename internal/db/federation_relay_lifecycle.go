package db

import "context"

// RelayLifecycleStore exposes the same native preflight used at destructive
// commit. Call before external credential teardown; normal pause retains intent.
type RelayLifecycleStore interface {
	ValidateRelayLifecycle(context.Context, int64) error
}
