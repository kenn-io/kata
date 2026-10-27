package pgstore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) mcpEventSQL() db.MCPEventSQL {
	return db.MCPEventSQL{Query: s, Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
		// Read committed: the quota lock below serializes counting writers, and
		// each statement then sees the previous holder's committed rows.
		return s.withTx(ctx, sql.LevelReadCommitted, run)
	}, LockQuota: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashtext('mcp_event_quota'))`)
		return err
	}}
}

// ActivateMCPEventSubscription creates or replaces a subscription under the quotas.
func (s *Store) ActivateMCPEventSubscription(ctx context.Context, in db.MCPEventActivation) (db.MCPEventSubscription, error) {
	return s.mcpEventSQL().Activate(ctx, in)
}

// RefreshMCPEventSubscription updates TTL and secret columns of an active row.
func (s *Store) RefreshMCPEventSubscription(ctx context.Context, in db.MCPEventRefresh) (db.MCPEventSubscription, error) {
	return s.mcpEventSQL().Refresh(ctx, in)
}

// GetMCPEventSubscription reads one subscription.
func (s *Store) GetMCPEventSubscription(ctx context.Context, id string) (db.MCPEventSubscription, error) {
	return s.mcpEventSQL().Get(ctx, id)
}

// ListMCPEventSubscriptions lists one principal's subscriptions.
func (s *Store) ListMCPEventSubscriptions(ctx context.Context, principalKey string) ([]db.MCPEventSubscription, error) {
	return s.mcpEventSQL().List(ctx, principalKey)
}

// MCPEventConsentExists reports endpoint consent for a principal and URL.
func (s *Store) MCPEventConsentExists(ctx context.Context, principalKey, callbackURL string) (bool, error) {
	return s.mcpEventSQL().ConsentExists(ctx, principalKey, callbackURL)
}

// CountActiveMCPEventSubscriptions counts active subscriptions daemon-wide.
func (s *Store) CountActiveMCPEventSubscriptions(ctx context.Context) (int, error) {
	return s.mcpEventSQL().CountActive(ctx)
}

// ListDueMCPEventSubscriptions lists unclaimed rows due for delivery work.
func (s *Store) ListDueMCPEventSubscriptions(ctx context.Context, now time.Time, limit int) ([]db.MCPEventSubscription, error) {
	return s.mcpEventSQL().ListDue(ctx, now, limit)
}

// ClaimMCPEventSubscription acquires a delivery claim.
func (s *Store) ClaimMCPEventSubscription(ctx context.Context, in db.MCPEventClaim) (bool, error) {
	return s.mcpEventSQL().Claim(ctx, in)
}

// RenewMCPEventClaim extends a held claim.
func (s *Store) RenewMCPEventClaim(ctx context.Context, id, incarnation string, generation int64, token string, until time.Time) (bool, error) {
	return s.mcpEventSQL().Renew(ctx, id, incarnation, generation, token, until)
}

// UpdateMCPEventDelivery applies one claim-fenced delivery transition.
func (s *Store) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	return s.mcpEventSQL().UpdateDelivery(ctx, in)
}

// EndMCPEventSubscription ends an active row outside a claim.
func (s *Store) EndMCPEventSubscription(ctx context.Context, in db.MCPEventEnd) (db.MCPEventSubscription, error) {
	return s.mcpEventSQL().End(ctx, in)
}

// ExpireMCPEventSubscriptions ends expired grants and clears delivery state.
func (s *Store) ExpireMCPEventSubscriptions(ctx context.Context, now time.Time) (int, error) {
	return s.mcpEventSQL().Expire(ctx, now)
}

// PruneEndedMCPEventSubscriptions deletes expired and over-cap ended rows.
func (s *Store) PruneEndedMCPEventSubscriptions(ctx context.Context, now time.Time, caps db.MCPEventRetention) (int, error) {
	return s.mcpEventSQL().Prune(ctx, now, caps)
}

// MCPEventsKeyCheck reads the events key check value.
func (s *Store) MCPEventsKeyCheck(ctx context.Context) (string, bool, error) {
	return s.mcpEventSQL().KeyCheck(ctx)
}

// PutMCPEventsKeyCheckIfAbsent stores the key check value unless one exists.
func (s *Store) PutMCPEventsKeyCheckIfAbsent(ctx context.Context, value string) (string, error) {
	return s.mcpEventSQL().PutKeyCheckIfAbsent(ctx, value)
}

// ImportMCPEventSubscription validates and inserts or replaces one row.
func (s *Store) ImportMCPEventSubscription(ctx context.Context, sub db.MCPEventSubscription) error {
	return s.mcpEventSQL().Import(ctx, sub)
}
