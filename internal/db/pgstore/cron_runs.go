package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// ObserveCronRun records evidence without reserving an occurrence or issue.
func (s *Store) ObserveCronRun(ctx context.Context, in db.ObserveCronRun) (db.CronRunObservationResult, error) {
	return s.cronSQL().ObserveRun(ctx, in)
}

// CronRun reads one project-scoped independent observation.
func (s *Store) CronRun(ctx context.Context, p int64, id string) (db.CronRun, error) {
	return s.cronSQL().Run(ctx, p, id)
}

// ListCronRuns returns a bounded ordinary history page.
func (s *Store) ListCronRuns(ctx context.Context, in db.CronRunList) ([]db.CronRun, error) {
	return s.cronSQL().Runs(ctx, in)
}

// PruneCronRuns removes eligible evidence at the project's authority.
func (s *Store) PruneCronRuns(ctx context.Context, in db.PruneCronRuns) (db.CronRunPruneResult, error) {
	return s.cronSQL().PruneRuns(ctx, in)
}

// CronRunCounts reads retained observations and recorded prunes in one snapshot.
func (s *Store) CronRunCounts(ctx context.Context, in db.CronRunCountsQuery) (db.CronRunCountsResult, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return db.CronRunCountsResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := db.ReadCronRunCounts(ctx, tx, in)
	if err != nil {
		return db.CronRunCountsResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.CronRunCountsResult{}, err
	}
	return result, nil
}
