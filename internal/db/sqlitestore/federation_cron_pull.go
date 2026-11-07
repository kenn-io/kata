package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

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

// Push batches are unbounded; large issue sets keep the complete query rather
// than growing SQL placeholders. Ordinary issue-only pushes retain their own
// preparation policy, and project-wide or move pages keep full replay.
const federationCronIssueScopeLimit = 500

func cronFederationIssueScope(events []db.FoldEvent) ([]string, bool) {
	var scope []string
	cron := false
	for _, event := range events {
		if db.EventRequiredFeatures(event.Type) == db.CronEventFeature {
			cron = true
			continue
		}
		if !strings.HasPrefix(event.Type, "issue.") || event.Type == "issue.moved" {
			return nil, false
		}
		uid := db.FederationEventIssueUID(&event.IssueUID, string(event.Payload))
		if uid == "" {
			return nil, false
		}
		if !slices.Contains(scope, uid) {
			scope = append(scope, uid)
		}
		if len(scope) > federationCronIssueScopeLimit {
			return nil, false
		}
	}
	return scope, cron && len(scope) > 0
}
