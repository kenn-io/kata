package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// RetainedIssueSnapshot records or returns a snapshot for an older local task.
func (d *Store) RetainedIssueSnapshot(ctx context.Context, projectID int64, issueUID string) (db.Event, int64, bool, error) {
	var creationID int64
	var created bool
	event, err := retryWrite1(ctx, d, func() (db.Event, error) {
		created = false
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.Event{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := federationBindingByProject(ctx, tx, projectID)
		if err != nil {
			return db.Event{}, err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE project_id=?1 AND issue_uid=?2 AND `+db.LocalInstanceOriginSQL("origin_instance_uid", "?3")+` AND id<=?4 AND (origin_instance_uid<>?3 OR actor<>?5) AND type IN ('issue.created','issue.snapshot','issue.moved') ORDER BY id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor).Scan(&creationID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.Event{}, db.ErrNotFound
		}
		if err != nil {
			return db.Event{}, err
		}
		existing, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=?1 AND e.issue_uid=?2 AND e.origin_instance_uid=?3 AND e.id>?4 AND e.actor=?5 AND e.type='issue.snapshot' AND (EXISTS(SELECT 1 FROM projects p JOIN federation_bindings b ON b.project_id=p.id JOIN meta m ON m.key=?7 || p.uid || '.' || b.hub_project_uid || '.' || b.hub_url || '.' || e.issue_uid AND m.value=e.uid WHERE p.id=e.project_id) OR NOT EXISTS(SELECT 1 FROM events later WHERE later.project_id=e.project_id AND later.issue_uid=e.issue_uid AND later.id>e.id AND later.id<=?6)) ORDER BY e.id DESC LIMIT 1`, projectID, issueUID, d.InstanceUID(), creationID, binding.Actor, binding.RetainedThroughEventID, db.FederationAcceptedIssuePrefix))
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, db.ErrNotFound) {
			return db.Event{}, err
		}
		issue, err := eventIssueByUID(ctx, tx, issueUID)
		if err != nil || issue.ProjectID != projectID {
			if err == nil {
				err = db.ErrNotFound
			}
			return db.Event{}, err
		}
		payload, err := d.federationIssueSnapshotPayload(ctx, tx, issue)
		if err != nil {
			return db.Event{}, err
		}
		payload, err = db.RetainedSnapshotLocalLinks(ctx, tx, issue, payload)
		if err != nil {
			return db.Event{}, err
		}
		canonical, err := db.CanonicalizeRetainedSnapshotAuthors(payload, binding.Actor)
		if err != nil {
			return db.Event{}, err
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, IssueID: &issue.ID, IssueUID: &issueUID, Type: "issue.snapshot", Actor: binding.Actor, Payload: string(canonical.Payload)})
		if err != nil {
			return db.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.Event{}, err
		}
		created = true
		return event, nil
	})
	return event, creationID, created, err
}
