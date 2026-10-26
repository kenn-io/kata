package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

const cronLatestRunBatchSize = 256

// LatestRuns reads each requested job's last observation by creation time and
// UID. The bounded VALUES table drives one indexed history seek per job, so a
// busy job's older observations are not loaded into the response.
func (a CronSQL) LatestRuns(ctx context.Context, projectID int64, jobUIDs []string) (map[string]CronRun, error) {
	jobs := make([]string, 0, len(jobUIDs))
	seen := make(map[string]bool, len(jobUIDs))
	for _, raw := range jobUIDs {
		id := strings.ToUpper(raw)
		if !cronUID(id) {
			return nil, fmt.Errorf("%w: invalid job UID", cron.ErrInvalid)
		}
		if !seen[id] {
			jobs = append(jobs, id)
			seen[id] = true
		}
	}
	result := make(map[string]CronRun, len(jobs))
	for start := 0; start < len(jobs); start += cronLatestRunBatchSize {
		batch := jobs[start:min(start+cronLatestRunBatchSize, len(jobs))]
		if err := a.latestRunsBatch(ctx, projectID, batch, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (a CronSQL) latestRunsBatch(ctx context.Context, projectID int64, jobs []string, result map[string]CronRun) error {
	args := []any{projectID}
	values := make([]string, len(jobs))
	for i, job := range jobs {
		args = append(args, job)
		values[i] = fmt.Sprintf("($%d)", len(args))
	}
	statement := "WITH requested(job_uid) AS (VALUES " + strings.Join(values, ",") + ") SELECT " + cronRunColumns + `
		FROM cron_runs WHERE project_id=$1 AND uid IN (
			SELECT (SELECT uid FROM cron_runs WHERE project_id=$1 AND job_uid=requested.job_uid
				ORDER BY created_at DESC,uid DESC LIMIT 1)
			FROM requested)`
	rows, err := a.Query.QueryContext(ctx, statement, args...) //nolint:gosec // Only fixed columns and generated placeholders enter SQL; job IDs are bound.
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, err := scanCronRunRow(rows)
		if err != nil {
			return err
		}
		result[*run.JobUID] = CronRun(run)
	}
	return rows.Err()
}

// CountFailedRuns counts observations created in the inclusive time window.
func (a CronSQL) CountFailedRuns(ctx context.Context, projectID int64, since, until time.Time) (int64, error) {
	since, until = since.UTC(), until.UTC()
	if since.Year() < 0 || since.Year() > 9999 || until.Year() < 0 || until.Year() > 9999 {
		return 0, fmt.Errorf("%w: cron count window exceeds supported timestamp range", cron.ErrInvalid)
	}
	if since.After(until) {
		return 0, fmt.Errorf("%w: cron count window ends before it starts", cron.ErrInvalid)
	}
	var count int64
	err := a.Query.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_runs
		WHERE project_id=$1 AND status='failed' AND created_at >= $2 AND created_at <= $3`,
		projectID, since.UTC().Format(CronRunCreatedAtFormat), until.UTC().Format(CronRunCreatedAtFormat)).Scan(&count)
	return count, err
}

func cronRunStatuses(input []string) ([]string, error) {
	statuses := make([]string, 0, len(input))
	seen := map[string]bool{}
	for _, status := range input {
		switch status {
		case "running", "succeeded", "failed", "cancelled", "unknown":
		default:
			return nil, fmt.Errorf("%w: unknown run status", cron.ErrInvalid)
		}
		if !seen[status] {
			statuses = append(statuses, status)
			seen[status] = true
		}
	}
	return statuses, nil
}
