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
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE project_id=?1 AND issue_uid=?2 AND `+db.LocalInstanceOriginSQL("origin_instance_uid", "?3")+` AND id<=?4 AND (origin_instance_uid<>?3 OR actor<>?5) AND type IN ('issue.created','issue.snapshot') ORDER BY id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor).Scan(&creationID)
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

// RetainedCommentEvent records a current-actor copy before an edit is sent.
func (d *Store) RetainedCommentEvent(ctx context.Context, projectID int64, issueUID, commentUID string) (db.Event, int64, error) {
	var sourceID int64
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
		source, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=?1 AND e.issue_uid=?2 AND `+db.LocalInstanceOriginSQL("e.origin_instance_uid", "?3")+` AND e.id<=?4 AND (e.origin_instance_uid<>?3 OR e.actor<>?5) AND e.type='issue.commented' AND json_extract(e.payload,'$.comment_uid')=?6 ORDER BY e.id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor, commentUID))
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, db.ErrNotFound) {
			source, err = scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=?1 AND e.issue_uid=?2 AND `+db.LocalInstanceOriginSQL("e.origin_instance_uid", "?3")+` AND e.id<=?4 AND (e.origin_instance_uid<>?3 OR e.actor<>?5) AND e.type IN ('issue.created','issue.snapshot') AND EXISTS (SELECT 1 FROM json_each(e.payload,'$.comments') WHERE json_extract(value,'$.comment_uid')=?6) ORDER BY e.id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor, commentUID))
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, db.ErrNotFound) {
				return db.Event{}, db.ErrNotFound
			}
		}
		if err != nil {
			return db.Event{}, err
		}
		sourceID = source.ID
		existing, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=?1 AND e.issue_uid=?2 AND e.origin_instance_uid=?3 AND e.id>?4 AND e.actor=?5 AND e.type='issue.commented' AND json_extract(e.payload,'$.comment_uid')=?6 ORDER BY e.id LIMIT 1`, projectID, issueUID, d.InstanceUID(), binding.RetainedThroughEventID, binding.Actor, commentUID))
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, db.ErrNotFound) {
			return db.Event{}, err
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM comments WHERE uid=?`, commentUID).Scan(&body); err != nil {
			return db.Event{}, err
		}
		payload, err := db.RetainedCommentPayload(source.Payload, source.Type, commentUID, binding.Actor, body)
		if err != nil {
			return db.Event{}, err
		}
		if source.RelatedIssueUID == nil {
			var id int64
			var uid string
			err := tx.QueryRowContext(ctx, `SELECT i.id,i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=json_extract(?,'$.reply_to_uid') AND i.uid<>?`, payload, issueUID).Scan(&id, &uid)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return db.Event{}, err
			}
			if err == nil {
				source.RelatedIssueID, source.RelatedIssueUID = &id, &uid
			}
		}
		created, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, IssueID: source.IssueID, IssueUID: &issueUID, RelatedIssueID: source.RelatedIssueID, RelatedIssueUID: source.RelatedIssueUID, Type: "issue.commented", Actor: binding.Actor, Payload: payload})
		if err != nil {
			return db.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.Event{}, err
		}
		return created, nil
	})
	return event, sourceID, err
}
