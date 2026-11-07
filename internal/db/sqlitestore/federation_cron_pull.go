package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// Cron rows derive only from cron events. A cron-only page still folds their
// complete history for definition clocks, authorship, snapshots and run
// observations, but does not reconcile unrelated issue identities or aliases.
func prepareCronFederationMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, binding db.FederationBinding, fresh []db.FoldEvent, alreadyStored bool) (federationMaterialization, error) {
	events, err := federationCronFoldEvents(ctx, tx, projectID)
	if err != nil {
		return federationMaterialization{}, err
	}
	if !alreadyStored {
		events = append(events, fresh...)
	}
	for _, event := range events {
		if err := db.ValidateFederationEntries(event.Type, event.UID, event.Payload); err != nil {
			return federationMaterialization{}, err
		}
	}
	return federationMaterialization{binding: binding, projection: db.FoldEvents(events), cronAffected: true}, nil
}

func federationCronFoldEvents(ctx context.Context, tx *sql.Tx, projectID int64) ([]db.FoldEvent, error) {
	// Match the existing partial cron index, without the cross-project issue
	// history lookup: cron definitions and observations belong to one project.
	rows, err := tx.QueryContext(ctx, `SELECT e.uid, e.origin_instance_uid, p.uid, e.issue_uid, e.related_issue_uid,
 e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.created_at
 FROM events e JOIN projects p ON p.id=e.project_id
 WHERE e.project_id=$1 AND e.type LIKE 'cron.%' ORDER BY e.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federated cron events: %w", err)
	}
	return scanFederationFoldEvents(rows, nil)
}

// Cron rows derive only from cron events. A cron-only pull still folds their
// complete definition history and the history of runs carried by the page,
// without reconciling unrelated issue identities or aliases.
// Mixed pages and project/move baselines keep the ordinary preparation path.
func prepareCronPullMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, binding db.FederationBinding, prepared []preparedFederationIngestEvent) (federationMaterialization, error) {
	runUIDs := []string{}
	for _, in := range prepared {
		if in.Event.Type == "cron.run.observed" || in.Event.Type == "cron.run.snapshot" {
			var body struct {
				UID string `json:"uid"`
			}
			if err := json.Unmarshal(in.Event.Payload, &body); err != nil {
				return federationMaterialization{}, err
			}
			runUIDs = append(runUIDs, body.UID)
		}
	}
	runJSON, err := json.Marshal(runUIDs)
	if err != nil {
		return federationMaterialization{}, err
	}
	// Sort the union's result rather than each arm's source rows so SQLite
	// seeks by type and run UID even before ANALYZE statistics exist.
	columns := `e.uid, e.origin_instance_uid, p.uid AS project_uid, e.issue_uid, e.related_issue_uid,
 e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.created_at`
	source := ` FROM events e JOIN projects p ON p.id=e.project_id WHERE e.project_id=$1 AND `
	//nolint:gosec // Only fixed SQL and registered event types are concatenated; values are bound.
	query := `SELECT uid, origin_instance_uid, project_uid, issue_uid, related_issue_uid,
 type, actor, payload, hlc_physical_ms, hlc_counter, created_at FROM (
 SELECT e.id, ` + columns + source + `e.type LIKE 'cron.%' AND e.type IN (` + db.CronDefinitionEventTypesSQL() + `)
 UNION ALL
 SELECT e.id, ` + columns + source + db.CronRunHistoryPredicateSQL(false) + `) ORDER BY +id`
	rows, err := tx.QueryContext(ctx, query, projectID, string(runJSON))
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
