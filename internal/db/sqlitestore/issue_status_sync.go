package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

var _ db.IssueStatusReader = (*Store)(nil)

// statusReadBindingTx takes the existing claim fence before reading private
// mapping authority. No second lease or persisted status table is needed.
func statusReadBindingTx(ctx context.Context, tx *sql.Tx, guard db.IssueSyncImportGuard) (db.IssueSyncBinding, error) {
	binding, err := issueSyncBindingByID(ctx, tx, guard.BindingID)
	if err != nil {
		return db.IssueSyncBinding{}, err
	}
	if err := validateIssueSyncImportGuardTx(ctx, tx, db.ImportBatchParams{ProjectID: binding.ProjectID, Source: binding.SourceKey, IssueSyncGuard: &guard}); err != nil {
		return db.IssueSyncBinding{}, err
	}
	return binding, nil
}

func loadIssueStatusMappingTx(ctx context.Context, tx *sql.Tx, binding db.IssueSyncBinding, id int64) (db.IssueStatusMapping, error) {
	mapping, err := scanImportMapping(tx.QueryRowContext(ctx, importMappingSelect+` WHERE id=$1 AND project_id=$2 AND source=$3 AND object_type='issue'
 AND EXISTS (SELECT 1 FROM issues WHERE issues.id=import_mappings.issue_id AND issues.deleted_at IS NULL)`, id, binding.ProjectID, binding.SourceKey))
	if err != nil {
		return db.IssueStatusMapping{}, err
	}
	var raw, observedAt, pending, locator *string
	if err := tx.QueryRowContext(ctx, `SELECT observed_status, CAST(observed_status_at AS TEXT), pending_event_uid, remote_locator FROM import_mappings WHERE id=$1`, id).Scan(&raw, &observedAt, &pending, &locator); err != nil {
		return db.IssueStatusMapping{}, err
	}
	state, err := db.DecodeIssueStatusColumns(raw, observedAt, pending, locator)
	if err != nil {
		return db.IssueStatusMapping{}, err
	}
	result := db.IssueStatusMapping{Mapping: mapping, State: state}
	if state.PendingEventUID != "" {
		var eventID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM events WHERE uid=$1`, state.PendingEventUID).Scan(&eventID)
		if err != nil {
			return db.IssueStatusMapping{}, fmt.Errorf("%w: pending status event is unavailable", db.ErrImportValidation)
		}
		event, err := scanEvent(tx.QueryRowContext(ctx, eventSelectByID, eventID))
		if err != nil {
			return db.IssueStatusMapping{}, fmt.Errorf("%w: pending status event is unavailable", db.ErrImportValidation)
		}
		var issueUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM issues WHERE id=$1`, *mapping.IssueID).Scan(&issueUID); err != nil {
			return db.IssueStatusMapping{}, err
		}
		if event.ProjectID != binding.ProjectID || event.IssueUID == nil || *event.IssueUID != issueUID || (event.Type != "issue.closed" && event.Type != "issue.reopened") {
			return db.IssueStatusMapping{}, fmt.Errorf("%w: pending status event belongs to a different identity or mutation", db.ErrImportValidation)
		}
		result.PendingEvent = &event
	}
	return result, nil
}

func statusMappingPageTx(ctx context.Context, tx *sql.Tx, binding db.IssueSyncBinding, query db.IssueStatusQuery) (db.IssueStatusPage, error) {
	high := query.ThroughID
	if high == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM import_mappings WHERE project_id=$1 AND source=$2 AND object_type='issue'
 AND EXISTS (SELECT 1 FROM issues WHERE issues.id=import_mappings.issue_id AND issues.deleted_at IS NULL)`, binding.ProjectID, binding.SourceKey).Scan(&high); err != nil {
			return db.IssueStatusPage{}, err
		}
	}
	sqlQuery := `SELECT id FROM import_mappings WHERE project_id=$1 AND source=$2 AND object_type='issue' AND id>$3 AND id<=$4
 AND EXISTS (SELECT 1 FROM issues WHERE issues.id=import_mappings.issue_id AND issues.deleted_at IS NULL)`
	if query.PendingOnly {
		sqlQuery += ` AND pending_event_uid IS NOT NULL`
	}
	rows, err := tx.QueryContext(ctx, sqlQuery+` ORDER BY id LIMIT $5`, binding.ProjectID, binding.SourceKey, query.AfterID, high, query.Limit)
	if err != nil {
		return db.IssueStatusPage{}, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return db.IssueStatusPage{}, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return db.IssueStatusPage{}, err
	}
	page := db.IssueStatusPage{HighWaterID: high, Mappings: make([]db.IssueStatusMapping, 0, len(ids))}
	for _, id := range ids {
		mapping, err := loadIssueStatusMappingTx(ctx, tx, binding, id)
		if errors.Is(err, db.ErrImportValidation) {
			mapping = db.IssueStatusMapping{Mapping: db.ImportMapping{ID: id, ProjectID: binding.ProjectID, Source: binding.SourceKey}, LoadError: err}
		} else if err != nil {
			return db.IssueStatusPage{}, err
		}
		page.Mappings = append(page.Mappings, mapping)
	}
	return page, nil
}

// IssueStatusMappingByID reads one claim-fenced private status mapping.
func (d *Store) IssueStatusMappingByID(ctx context.Context, guard db.IssueSyncImportGuard, id int64) (db.IssueStatusMapping, error) {
	return retryWrite1(ctx, d, func() (db.IssueStatusMapping, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueStatusMapping{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := statusReadBindingTx(ctx, tx, guard)
		if err != nil {
			return db.IssueStatusMapping{}, err
		}
		result, err := loadIssueStatusMappingTx(ctx, tx, binding, id)
		if err != nil {
			return db.IssueStatusMapping{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueStatusMapping{}, err
		}
		return result, nil
	})
}

// ListIssueStatusMappings reads a bounded claim-fenced mapping page.
func (d *Store) ListIssueStatusMappings(ctx context.Context, query db.IssueStatusQuery) (db.IssueStatusPage, error) {
	if query.Limit <= 0 || query.Limit > 100 || query.AfterID < 0 || query.ThroughID < 0 {
		return db.IssueStatusPage{}, fmt.Errorf("%w: status scan must be bounded", db.ErrImportValidation)
	}
	return retryWrite1(ctx, d, func() (db.IssueStatusPage, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueStatusPage{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := statusReadBindingTx(ctx, tx, query.Guard)
		if err != nil {
			return db.IssueStatusPage{}, err
		}
		result, err := statusMappingPageTx(ctx, tx, binding, query)
		if err != nil {
			return db.IssueStatusPage{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueStatusPage{}, err
		}
		return result, nil
	})
}

// enqueueIssueStatusIntentTx records the exact committed explicit mutation.
// Paused two-way bindings retain intent; default/one-way bindings do not.
func enqueueIssueStatusIntentTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	_, err := tx.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=$1
WHERE issue_id=$2 AND project_id=$3 AND object_type='issue'
 AND EXISTS (SELECT 1 FROM issue_sync_bindings b WHERE b.project_id=import_mappings.project_id AND b.source_key=import_mappings.source
  AND b.provider IN ('notion','github','plane','linear') AND json_extract(b.config_json,'$.status_sync')='two-way')
 AND NOT EXISTS (SELECT 1 FROM federation_bindings f WHERE f.project_id=import_mappings.project_id AND f.role='spoke' AND f.enabled=1)`, event.UID, event.IssueID, event.ProjectID)
	return err
}

// Only newly accepted, effective status intent can enqueue a provider write.
// Prior status clocks come from the already loaded event slice, not a second
// persisted clock or another database read of the project's history.
func reconcileFederatedStatusIntentTx(ctx context.Context, tx *sql.Tx, projectID int64, events []db.FoldEvent, acceptedUIDs []string, current db.FoldProjection) error {
	if len(acceptedUIDs) == 0 {
		return nil
	}
	binding, err := issueSyncBindingByProject(ctx, tx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var config struct {
		Mode string `json:"status_sync"`
	}
	if err := json.Unmarshal(binding.Config, &config); err != nil {
		return err
	}
	if config.Mode != "two-way" {
		return nil
	}
	acceptedSet := make(map[string]bool, len(acceptedUIDs))
	for _, uid := range acceptedUIDs {
		acceptedSet[uid] = true
	}
	accepted := map[string][]db.FoldEvent{}
	var history []db.FoldEvent
	for _, event := range events {
		if !acceptedSet[event.UID] {
			history = append(history, event)
			continue
		}
		switch event.Type {
		case "issue.closed", "issue.reopened", "issue.updated", "issue.created", "issue.snapshot":
			accepted[event.IssueUID] = append(accepted[event.IssueUID], event)
		}
	}
	if len(accepted) == 0 {
		return nil
	}
	previous := db.FoldEvents(history)
	for _, issue := range current.SortedIssues() {
		incoming := accepted[issue.UID]
		if len(incoming) == 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.pending_event_uid FROM import_mappings m JOIN issues i ON i.id=m.issue_id
 WHERE m.project_id=$1 AND m.source=$2 AND m.object_type='issue' AND i.uid=$3 ORDER BY m.id`, projectID, binding.SourceKey, issue.UID)
		if err != nil {
			return err
		}
		type checkpoint struct {
			id      int64
			pending sql.NullString
		}
		var mappings []checkpoint
		for rows.Next() {
			var value checkpoint
			if err := rows.Scan(&value.id, &value.pending); err != nil {
				_ = rows.Close()
				return err
			}
			mappings = append(mappings, value)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, mapping := range mappings {
			next := db.ReconcileFoldStatusIntent(previous.Issues[issue.UID], issue, mapping.pending.String, incoming)
			if next == mapping.pending.String {
				continue
			}
			var value any
			if next != "" {
				value = next
			}
			if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=$1 WHERE id=$2`, value, mapping.id); err != nil {
				return err
			}
		}
	}
	return nil
}
