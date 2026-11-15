package sqlitestore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) mcpEventSQL() db.MCPEventSQL {
	return db.MCPEventSQL{Query: d, Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
		return d.RetryTransient(ctx, func() error {
			tx, err := d.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := run(tx); err != nil {
				return err
			}
			return tx.Commit()
		})
	}}
}

// ActivateMCPEventSubscription creates or replaces a subscription under the quotas.
func (d *Store) ActivateMCPEventSubscription(ctx context.Context, in db.MCPEventActivation) (db.MCPEventSubscription, error) {
	return d.mcpEventSQL().Activate(ctx, in)
}

// RefreshMCPEventSubscription updates TTL and secret columns of an active row.
func (d *Store) RefreshMCPEventSubscription(ctx context.Context, in db.MCPEventRefresh) (db.MCPEventSubscription, error) {
	return d.mcpEventSQL().Refresh(ctx, in)
}

// GetMCPEventSubscription reads one subscription.
func (d *Store) GetMCPEventSubscription(ctx context.Context, id string) (db.MCPEventSubscription, error) {
	return d.mcpEventSQL().Get(ctx, id)
}

// ListMCPEventSubscriptions lists one principal's subscriptions.
func (d *Store) ListMCPEventSubscriptions(ctx context.Context, principalKey string) ([]db.MCPEventSubscription, error) {
	return d.mcpEventSQL().List(ctx, principalKey)
}

// MCPEventConsentExists reports endpoint consent for a principal and URL.
func (d *Store) MCPEventConsentExists(ctx context.Context, principalKey, callbackURL string) (bool, error) {
	return d.mcpEventSQL().ConsentExists(ctx, principalKey, callbackURL)
}

// CountActiveMCPEventSubscriptions counts active subscriptions daemon-wide.
func (d *Store) CountActiveMCPEventSubscriptions(ctx context.Context) (int, error) {
	return d.mcpEventSQL().CountActive(ctx)
}

// ListDueMCPEventSubscriptions lists unclaimed rows due for delivery work.
func (d *Store) ListDueMCPEventSubscriptions(ctx context.Context, now time.Time, limit int) ([]db.MCPEventSubscription, error) {
	return d.mcpEventSQL().ListDue(ctx, now, limit)
}

// MarkMCPEventSubscriptionsDirty records changes for active subscriptions.
func (d *Store) MarkMCPEventSubscriptionsDirty(ctx context.Context, ids []string) error {
	return d.mcpEventSQL().MarkDirty(ctx, ids)
}

// MCPEventsRematch reads the pending rematch record.
func (d *Store) MCPEventsRematch(ctx context.Context) (int64, int64, string, bool, error) {
	return d.mcpEventSQL().Rematch(ctx)
}

// AcknowledgeMCPEventsRematch records rematched applied events.
func (d *Store) AcknowledgeMCPEventsRematch(ctx context.Context, token string, matchedThrough int64) (bool, error) {
	return d.mcpEventSQL().AcknowledgeRematch(ctx, token, matchedThrough)
}

// UpdateMCPEventCatalogs records visible-project fingerprints.
func (d *Store) UpdateMCPEventCatalogs(ctx context.Context, versions map[string]string, observedThrough int64) (bool, error) {
	return d.mcpEventSQL().UpdateCatalogs(ctx, versions, observedThrough)
}

// ForgetMCPEventCatalogs marks visible-project fingerprints unknown.
func (d *Store) ForgetMCPEventCatalogs(ctx context.Context, ids []string) error {
	return d.mcpEventSQL().ForgetCatalogs(ctx, ids)
}

// MCPEventsScanPosition reads the delivery worker's scan position.
func (d *Store) MCPEventsScanPosition(ctx context.Context) (int64, bool, error) {
	return d.mcpEventSQL().ScanPosition(ctx)
}

// InitializeMCPEventsScanPosition stores the first scan position.
func (d *Store) InitializeMCPEventsScanPosition(ctx context.Context, head int64) (bool, error) {
	return d.mcpEventSQL().InitializeScanPosition(ctx, head)
}

// MCPEventsAccessChangesAfter reads access audit events for the worker.
func (d *Store) MCPEventsAccessChangesAfter(ctx context.Context, after, through int64, limit int) ([]db.Event, error) {
	return d.mcpEventSQL().AccessChangesAfter(ctx, after, through, limit)
}

// AdvanceMCPEventsScanPosition moves the scan position conditionally.
func (d *Store) AdvanceMCPEventsScanPosition(ctx context.Context, from, to int64) (bool, error) {
	return d.mcpEventSQL().AdvanceScanPosition(ctx, from, to)
}

// ClaimMCPEventSubscription acquires a delivery claim.
func (d *Store) ClaimMCPEventSubscription(ctx context.Context, in db.MCPEventClaim) (bool, error) {
	return d.mcpEventSQL().Claim(ctx, in)
}

// RenewMCPEventClaim extends a held claim.
func (d *Store) RenewMCPEventClaim(ctx context.Context, id, incarnation string, generation int64, token string, until time.Time) (bool, error) {
	return d.mcpEventSQL().Renew(ctx, id, incarnation, generation, token, until)
}

// UpdateMCPEventDelivery applies one claim-fenced delivery transition.
func (d *Store) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	return d.mcpEventSQL().UpdateDelivery(ctx, in)
}

// EndMCPEventSubscription ends an active row outside a claim.
func (d *Store) EndMCPEventSubscription(ctx context.Context, in db.MCPEventEnd) (db.MCPEventSubscription, error) {
	return d.mcpEventSQL().End(ctx, in)
}

// ExpireMCPEventSubscriptions ends expired grants and clears delivery state.
func (d *Store) ExpireMCPEventSubscriptions(ctx context.Context, now time.Time) (int, error) {
	return d.mcpEventSQL().Expire(ctx, now)
}

// PruneEndedMCPEventSubscriptions deletes expired and over-cap ended rows.
func (d *Store) PruneEndedMCPEventSubscriptions(ctx context.Context, now time.Time, caps db.MCPEventRetention) (int, error) {
	return d.mcpEventSQL().Prune(ctx, now, caps)
}

// MCPEventsKeyCheck reads the events key check value.
func (d *Store) MCPEventsKeyCheck(ctx context.Context) (string, bool, error) {
	return d.mcpEventSQL().KeyCheck(ctx)
}

// PutMCPEventsKeyCheckIfAbsent stores the key check value unless one exists.
func (d *Store) PutMCPEventsKeyCheckIfAbsent(ctx context.Context, value string) (string, error) {
	return d.mcpEventSQL().PutKeyCheckIfAbsent(ctx, value)
}

// ImportMCPEventSubscription validates and inserts or replaces one row.
func (d *Store) ImportMCPEventSubscription(ctx context.Context, s db.MCPEventSubscription) error {
	return d.mcpEventSQL().Import(ctx, s)
}
