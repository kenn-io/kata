package sqlitestore

import (
	"context"

	"go.kenn.io/kata/internal/cron"
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

// CronRunStats reads bounded evidence for statistics.
func (s *Store) CronRunStats(ctx context.Context, in db.CronRunStatsQuery) ([]cron.RunStatRow, bool, error) {
	return s.cronSQL().RunStats(ctx, in)
}
