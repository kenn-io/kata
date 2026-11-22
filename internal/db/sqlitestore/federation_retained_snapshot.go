package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// RetainedIssueSnapshot records or returns a snapshot for an older local task.
func (d *Store) RetainedIssueSnapshot(ctx context.Context, projectID int64, issueUID string) (db.Event, int64, error) {
	var creationID int64
	event, err := retryWrite1(ctx, d, func() (db.Event, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.Event{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := federationBindingByProject(ctx, tx, projectID)
		if err != nil {
			return db.Event{}, err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE project_id=? AND issue_uid=? AND origin_instance_uid=? AND id<=? AND actor<>? AND type IN ('issue.created','issue.snapshot') ORDER BY id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor).Scan(&creationID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.Event{}, db.ErrNotFound
		}
		if err != nil {
			return db.Event{}, err
		}
		existing, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=? AND e.issue_uid=? AND e.origin_instance_uid=? AND e.id>? AND e.actor=? AND e.type='issue.snapshot' ORDER BY e.id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor))
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
		payload, err := d.federationIssueSnapshotPayload(ctx, tx, issue, true)
		if err != nil {
			return db.Event{}, err
		}
		canonical, err := db.CanonicalizeFederationSnapshotAuthors(db.RemoteEvent{Type: "issue.snapshot", Payload: jsontext.Value(payload)}, binding.Actor)
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
		return event, nil
	})
	return event, creationID, err
}
