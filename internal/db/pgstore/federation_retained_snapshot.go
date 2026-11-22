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
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE project_id=$1 AND issue_uid=$2 AND `+db.LocalInstanceOriginSQL("origin_instance_uid", "$3")+` AND id<=$4 AND (origin_instance_uid<>$3 OR actor<>$5) AND type IN ('issue.created','issue.snapshot') ORDER BY id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor).Scan(&creationID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return err
		}
		result, err = scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.issue_uid=$2 AND e.origin_instance_uid=$3 AND e.id>$4 AND e.actor=$5 AND e.type='issue.snapshot' ORDER BY e.id LIMIT 1`, projectID, issueUID, s.InstanceUID(), binding.RetainedThroughEventID, binding.Actor))
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		issue, err := eventIssueByUID(ctx, tx, issueUID)
		if err != nil {
			return err
		}
		if issue.ProjectID != projectID {
			return db.ErrNotFound
		}
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
		if err != nil {
			return err
		}
		payload, err := federationIssueSnapshotPayload(ctx, tx, issue, true)
		if err != nil {
			return err
		}
		canonical, err := db.CanonicalizeFederationSnapshotAuthors(db.RemoteEvent{Type: "issue.snapshot", Payload: jsontext.Value(payload)}, binding.Actor)
		if err != nil {
			return err
		}
		result, err = s.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: project.UID, ProjectName: project.Name, IssueID: &issue.ID, IssueUID: &issueUID, Type: "issue.snapshot", Actor: binding.Actor, Payload: string(canonical.Payload)})
		return err
	})
	return result, creationID, err
}
