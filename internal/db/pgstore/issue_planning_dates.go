package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// IssuePlanningDates reads consistent native dates without evaluating readiness.
func (s *Store) IssuePlanningDates(ctx context.Context, in db.IssuePlanningDatesIn) (db.IssuePlanningDates, error) {
	return db.ReadIssuePlanningDatesSQL(ctx, s, in)
}
