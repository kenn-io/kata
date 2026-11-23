package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// RetainedIssueSnapshot records or returns a snapshot for an older local task.
func (s *Store) RetainedIssueSnapshot(ctx context.Context, projectID int64, issueUID string) (db.Event, int64, error) {
	var creationID int64
	var result db.Event
	err := s.withSerializableEventTx(ctx, func(tx *sql.Tx) (*db.Event, error) {
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if err != nil {
			return nil, err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE project_id=$1 AND issue_uid=$2 AND `+db.LocalInstanceOriginSQL("origin_instance_uid", "$3")+` AND id<=$4 AND (origin_instance_uid<>$3 OR actor<>$5) AND type IN ('issue.created','issue.snapshot') ORDER BY id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor).Scan(&creationID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		result, err = scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.issue_uid=$2 AND e.origin_instance_uid=$3 AND e.id>$4 AND e.actor=$5 AND e.type='issue.snapshot' ORDER BY e.id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor))
		if err == nil {
			return nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		issue, err := eventIssueByUID(ctx, tx, issueUID)
		if err != nil {
			return nil, err
		}
		if issue.ProjectID != projectID {
			return nil, db.ErrNotFound
		}
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
		if err != nil {
			return nil, err
		}
		payload, err := federationIssueSnapshotPayload(ctx, tx, issue, true)
		if err != nil {
			return nil, err
		}
		canonical, err := db.CanonicalizeFederationSnapshotAuthors(db.RemoteEvent{Type: "issue.snapshot", Payload: jsontext.Value(payload)}, binding.Actor)
		if err != nil {
			return nil, err
		}
		result, err = s.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: project.UID, ProjectName: project.Name, IssueID: &issue.ID, IssueUID: &issueUID, Type: "issue.snapshot", Actor: binding.Actor, Payload: string(canonical.Payload)})
		return &result, err
	})
	return result, creationID, err
}

// RetainedCommentEvent records a current-actor copy before an edit is sent.
func (s *Store) RetainedCommentEvent(ctx context.Context, projectID int64, issueUID, commentUID string) (db.Event, int64, error) {
	var sourceID int64
	var result db.Event
	err := s.withSerializableEventTx(ctx, func(tx *sql.Tx) (*db.Event, error) {
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if err != nil {
			return nil, err
		}
		source, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.issue_uid=$2 AND `+db.LocalInstanceOriginSQL("e.origin_instance_uid", "$3")+` AND e.id<=$4 AND (e.origin_instance_uid<>$3 OR e.actor<>$5) AND e.type='issue.commented' AND e.payload::jsonb->>'comment_uid'=$6 ORDER BY e.id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor, commentUID))
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, db.ErrNotFound) {
			source, err = scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.issue_uid=$2 AND `+db.LocalInstanceOriginSQL("e.origin_instance_uid", "$3")+` AND e.id<=$4 AND (e.origin_instance_uid<>$3 OR e.actor<>$5) AND e.type IN ('issue.created','issue.snapshot') AND EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(e.payload::jsonb->'comments','[]'::jsonb)) AS comment WHERE comment->>'comment_uid'=$6) ORDER BY e.id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor, commentUID))
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, db.ErrNotFound) {
				return nil, db.ErrNotFound
			}
		}
		if err != nil {
			return nil, err
		}
		sourceID = source.ID
		result, err = scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.issue_uid=$2 AND e.origin_instance_uid=$3 AND e.id>$4 AND e.actor=$5 AND e.type='issue.commented' AND e.payload::jsonb->>'comment_uid'=$6 ORDER BY e.id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor, commentUID))
		if err == nil {
			return nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM comments WHERE uid=$1`, commentUID).Scan(&body); err != nil {
			return nil, err
		}
		payload, err := db.RetainedCommentPayload(source.Payload, source.Type, commentUID, binding.Actor, body)
		if err != nil {
			return nil, err
		}
		if source.RelatedIssueUID == nil {
			var id int64
			var uid string
			err := tx.QueryRowContext(ctx, `SELECT i.id,i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1::jsonb->>'reply_to_uid' AND i.uid<>$2`, payload, issueUID).Scan(&id, &uid)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				source.RelatedIssueID, source.RelatedIssueUID = &id, &uid
			}
		}
		result, err = s.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: source.ProjectUID, ProjectName: source.ProjectName, IssueID: source.IssueID, IssueUID: &issueUID, RelatedIssueID: source.RelatedIssueID, RelatedIssueUID: source.RelatedIssueUID, Type: "issue.commented", Actor: binding.Actor, Payload: payload})
		return &result, err
	})
	return result, sourceID, err
}
