package sqlitestore

import (
	"context"
	"go.kenn.io/kata/internal/db"
)

// ClaimCronOccurrence reserves an occurrence only on its owning root.
func (s *Store) ClaimCronOccurrence(ctx context.Context, c db.CronOccurrenceClaim) (db.CronClaimResult, error) {
	return s.cronSQL().ClaimOccurrence(ctx, c)
}

// CronOccurrenceClaim reads retained owner control state.
func (s *Store) CronOccurrenceClaim(ctx context.Context, p int64, id string) (db.CronOccurrenceClaim, error) {
	return s.cronSQL().Claim(ctx, p, id)
}
