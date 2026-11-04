package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// ListCronJobVersions reads retained local job history.
func (s *Store) ListCronJobVersions(ctx context.Context, in db.CronVersionList) (db.CronVersionPage, error) {
	return s.cronSQL().DefinitionVersions(ctx, in, true)
}

// ListCronWorkflowVersions reads retained local workflow history.
func (s *Store) ListCronWorkflowVersions(ctx context.Context, in db.CronVersionList) (db.CronVersionPage, error) {
	return s.cronSQL().DefinitionVersions(ctx, in, false)
}
