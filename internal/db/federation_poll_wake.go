package db

import (
	"context"
	"errors"
)

// ErrFederationPollWakeClosed reports that the store was closed, so no more
// wakeups can be published or received.
var ErrFederationPollWakeClosed = errors.New("federation poll wake bus is closed")

// FederationPollWakeBus carries federation poll wakeups between hub processes
// that share one database. A store used by a single process does not
// implement it; that process wakes its held polls in memory.
type FederationPollWakeBus interface {
	// PublishFederationPollWake sends payload to every listener on this
	// database and schema.
	PublishFederationPollWake(ctx context.Context, payload string) error
	// ListenFederationPollWake calls listening once notifications are being
	// received, then deliver for each payload, until ctx ends or the
	// connection fails. Notifications sent while no listener is connected are
	// lost, so listening is the point to treat every held poll as woken.
	ListenFederationPollWake(ctx context.Context, listening func(), deliver func(payload string)) error
}
