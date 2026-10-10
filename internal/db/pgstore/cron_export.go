package pgstore

import (
	"context"
	"iter"

	"go.kenn.io/kata/internal/db"
)

// ExportCronJobs streams portable job definitions for export.
func (s *Store) ExportCronJobs(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.CronJobExport, error] {
	return db.ExportCronJobsSQL(ctx, s.exportQueryContext, filter)
}

// ExportCronWorkflows streams portable workflow definitions for export.
func (s *Store) ExportCronWorkflows(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.CronWorkflowExport, error] {
	return db.ExportCronWorkflowsSQL(ctx, s.exportQueryContext, filter)
}

// ExportCronRuns streams attributed run history for export.
func (s *Store) ExportCronRuns(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.CronRunExport, error] {
	return db.ExportCronRunsSQL(ctx, s.exportQueryContext, filter)
}
