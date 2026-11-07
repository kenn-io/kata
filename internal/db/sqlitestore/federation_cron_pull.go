package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// Cron rows derive only from cron events. A cron-only pull still folds their
// complete definition history and the history of runs carried by the page,
// without reconciling unrelated issue identities or aliases.
// Mixed pages and project/move baselines keep the ordinary preparation path.
func prepareCronPullMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, binding db.FederationBinding, prepared []preparedFederationIngestEvent) (federationMaterialization, error) {
	runUIDs := []string{}
	for _, in := range prepared {
		if in.Event.Type == "cron.run.observed" || in.Event.Type == "cron.run.snapshot" {
			event := in.Event
			body, err := db.ParseCronRunObservation(db.FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, Type: event.Type, Payload: event.Payload, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter})
			if err != nil {
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
