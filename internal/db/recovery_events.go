package db

import (
	"context"
	"database/sql"
)

type recoveryEventPublisherKey struct{}

// WithRecoveryEventPublisher publishes newly committed retained-history events.
func WithRecoveryEventPublisher(ctx context.Context, publish func(Event)) context.Context {
	return context.WithValue(ctx, recoveryEventPublisherKey{}, publish)
}

// PublishRecoveryEvent reports an event after its transaction has committed.
func PublishRecoveryEvent(ctx context.Context, event Event) {
	if publish, ok := ctx.Value(recoveryEventPublisherKey{}).(func(Event)); ok && publish != nil {
		publish(event)
	}
}

// CommitRecoveryEvent commits tx and then reports event to the publisher.
func CommitRecoveryEvent(ctx context.Context, tx *sql.Tx, event Event) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	PublishRecoveryEvent(ctx, event)
	return nil
}
