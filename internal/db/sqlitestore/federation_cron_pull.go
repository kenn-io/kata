package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// Cron rows derive only from cron events. A cron-only pull still folds their
// complete history for definition clocks, authorship, snapshots and run
// observations, but does not reconcile unrelated issue identities or aliases.
// Mixed pages and project/move baselines keep the ordinary preparation path.
func prepareCronPullMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, binding db.FederationBinding) (federationMaterialization, error) {
	// Match the existing partial cron index, without the cross-project issue
	// history lookup: cron definitions and observations belong to one project.
	rows, err := tx.QueryContext(ctx, `SELECT e.uid, e.origin_instance_uid, p.uid, e.issue_uid, e.related_issue_uid,
 e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.created_at
 FROM events e JOIN projects p ON p.id=e.project_id
 WHERE e.project_id=$1 AND e.type LIKE 'cron.%' ORDER BY e.id`, projectID)
	if err != nil {
		return federationMaterialization{}, fmt.Errorf("list federated cron events: %w", err)
	}
	events, err := scanFederationFoldEvents(rows, nil)
	if err != nil {
		return federationMaterialization{}, err
	}
	for _, event := range events {
		if err := db.ValidateFederationEntries(event.Type, event.UID, event.Payload); err != nil {
			return federationMaterialization{}, err
		}
	}
	return federationMaterialization{binding: binding, projection: db.FoldEvents(events), cronAffected: true}, nil
}
