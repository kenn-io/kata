package sqlitestore

import (
	"context"
	"time"

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

// LatestCronRuns returns each requested job's last creation-ordered observation.
func (s *Store) LatestCronRuns(ctx context.Context, projectID int64, jobUIDs []string) (map[string]db.CronRun, error) {
	return s.cronSQL().LatestRuns(ctx, projectID, jobUIDs)
}

// CountFailedCronRuns counts project failures within an inclusive creation window.
func (s *Store) CountFailedCronRuns(ctx context.Context, projectID int64, since, until time.Time) (int64, error) {
	return s.cronSQL().CountFailedRuns(ctx, projectID, since, until)
}
