package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// Moved issues take the target's access policy. Refuse a merge that could
// show a team-restricted source's issues outside its exact team set. The
// caller already holds the project access lock for the merge transaction.
func rejectProjectMergeAccessWideningTx(ctx context.Context, tx *sql.Tx, sourceUID, targetUID string) error {
	source, err := readProjectAccessPolicy(ctx, tx, sourceUID)
	if err != nil {
		return fmt.Errorf("read source access policy for merge: %w", err)
	}
	if source.Visibility == "all" {
		return nil
	}
	target, err := readProjectAccessPolicy(ctx, tx, targetUID)
	if err != nil {
		return fmt.Errorf("read target access policy for merge: %w", err)
	}
	if target.Visibility != source.Visibility || !slices.Equal(target.TeamUIDs, source.TeamUIDs) {
		return db.ErrProjectMergeAccessPolicy
	}
	return nil
}
