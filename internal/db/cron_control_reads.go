package db

import (
	"context"
	"strings"
)

// CronJobControlRead keeps a job and its effective project metadata cutoff in
// the same backend snapshot without changing the portable definition record.
type CronJobControlRead struct {
	Job      CronJob
	Passover *CronPassoverState
}

// JobControlRead returns one consistent definition and effective cutoff.
func (a CronSQL) JobControlRead(ctx context.Context, projectID int64, id string) (CronJobControlRead, error) {
	values, err := a.jobControlReads(ctx, projectID, strings.ToUpper(id), true)
	if err != nil {
		return CronJobControlRead{}, err
	}
	if len(values) == 0 {
		return CronJobControlRead{}, ErrNotFound
	}
	return values[0], nil
}

// JobControlReads returns consistent definition and cutoff pairs.
func (a CronSQL) JobControlReads(ctx context.Context, projectID int64, includeDeleted bool) ([]CronJobControlRead, error) {
	return a.jobControlReads(ctx, projectID, "", includeDeleted)
}
func (a CronSQL) jobControlReads(ctx context.Context, projectID int64, id string, includeDeleted bool) ([]CronJobControlRead, error) {
	// One joined statement supplies both records under one database snapshot,
	// including PostgreSQL's default statement isolation and SQLite WAL reads.
	query := "SELECT j." + strings.ReplaceAll(cronDefinitionColumns, ",", ",j.") + ",p.metadata FROM cron_jobs j JOIN projects p ON p.id=j.project_id WHERE j.project_id=$1 AND p.deleted_at IS NULL"
	args := []any{projectID}
	if id != "" {
		query += " AND j.uid=$2"
		args = append(args, id)
	}
	if !includeDeleted {
		query += " AND j.deleted_at IS NULL"
	}
	query += " ORDER BY j.name,j.uid"
	rows, err := a.Query.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []CronJobControlRead{}
	for rows.Next() {
		var metadata JSONBlob
		row, err := scanCron(rows, &metadata)
		if err != nil {
			return nil, err
		}
		job, err := jobFromRow(row)
		if err != nil {
			return nil, err
		}
		values = append(values, CronJobControlRead{Job: job, Passover: CronPassoverFromMetadata(metadata, job.UID)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
