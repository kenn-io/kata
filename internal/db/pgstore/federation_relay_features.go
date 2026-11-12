package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// RelayWorkflowRequiredFeatures probes retained project history before a sync turn.
func (d *Store) RelayWorkflowRequiredFeatures(ctx context.Context, projectUID string) (string, error) {
	return db.ProjectWorkflowRequiredFeatures(ctx, d.DB, projectUID)
}
