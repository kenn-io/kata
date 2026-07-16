package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

const federationPushEventTypeList = `(
  'project.metadata_updated',
  'issue.created', 'issue.snapshot', 'issue.updated', 'issue.closed', 'issue.reopened',
  'issue.soft_deleted', 'issue.restored', 'issue.commented', 'issue.comment_edited',
  'issue.assigned', 'issue.unassigned', 'issue.priority_set', 'issue.priority_cleared',
  'issue.labeled', 'issue.unlabeled',
  'issue.linked', 'issue.unlinked', 'issue.links_changed', 'issue.metadata_updated'
)`

func pgFederationPushEventTypeCondition(column string) string {
	return column + ` IN ` + federationPushEventTypeList
}

// PendingFederationPushEvents returns supported local-origin events above one acknowledgement cursor.
func (s *Store) PendingFederationPushEvents(
	ctx context.Context,
	projectID int64,
	originInstanceUID string,
	afterID int64,
	limit int,
) ([]db.Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	output, err := s.queryPendingFederationPushEvents(ctx, eventSelect+`
WHERE e.project_id=$1 AND e.origin_instance_uid=$2 AND e.id>$3
  AND `+pgFederationPushEventTypeCondition("e.type")+`
ORDER BY e.id ASC LIMIT $4`, projectID, originInstanceUID, afterID, limit)
	if err != nil {
		return nil, err
	}
	if len(output) == limit && len(output) > 0 && output[len(output)-1].Type == "issue.snapshot" {
		runStartAfterID := afterID
		for index := len(output) - 1; index >= 0; index-- {
			if output[index].Type != "issue.snapshot" {
				runStartAfterID = output[index].ID
				break
			}
		}
		extra, err := s.queryPendingFederationPushEvents(ctx, eventSelect+`
WHERE e.project_id=$1 AND e.origin_instance_uid=$2 AND e.id>$3 AND e.type='issue.snapshot'
  AND NOT EXISTS (
    SELECT 1 FROM events barrier
     WHERE barrier.project_id=e.project_id
       AND barrier.origin_instance_uid=e.origin_instance_uid
       AND barrier.id>$4 AND barrier.id<e.id
       AND `+pgFederationPushEventTypeCondition("barrier.type")+`
       AND barrier.type<>'issue.snapshot'
  )
ORDER BY e.id ASC`, projectID, originInstanceUID, output[len(output)-1].ID, runStartAfterID)
		if err != nil {
			return nil, err
		}
		output = append(output, extra...)
	}
	for index, event := range output {
		if event.Type != "issue.snapshot" {
			continue
		}
		for next := index + 1; next < len(output); next++ {
			if output[next].Type != "issue.snapshot" {
				return output[:next], nil
			}
		}
		break
	}
	return output, nil
}

func (s *Store) queryPendingFederationPushEvents(ctx context.Context, query string, args ...any) ([]db.Event, error) {
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := []db.Event{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		output = append(output, event)
	}
	return output, mapSQLError(rows.Err(), nil)
}

// PendingFederationPushStats returns the supported pending count and event high-water mark.
func (s *Store) PendingFederationPushStats(
	ctx context.Context,
	projectID int64,
	originInstanceUID string,
	afterID int64,
) (int64, int64, error) {
	var count int64
	var highWater sql.NullInt64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*),MAX(id) FROM events
WHERE project_id=$1 AND origin_instance_uid=$2 AND id>$3 AND `+pgFederationPushEventTypeCondition("type"),
		projectID, originInstanceUID, afterID).Scan(&count, &highWater)
	if err != nil {
		return 0, 0, mapSQLError(err, nil)
	}
	return count, highWater.Int64, nil
}

// InsertRemoteEvent preserves one portable event while assigning only its local row identity.
func (s *Store) InsertRemoteEvent(ctx context.Context, projectID int64, remote db.RemoteEvent) (bool, error) {
	payload, createdAt, err := db.ValidateRemoteEventContentHash(remote)
	if err != nil {
		return false, err
	}
	inserted := false
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		inserted = false
		var existingHash string
		err := tx.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE uid=$1 FOR UPDATE`,
			remote.EventUID).Scan(&existingHash)
		if err == nil {
			if existingHash == remote.ContentHash {
				return nil
			}
			return fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, remote.EventUID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapSQLError(err, nil)
		}
		clock := db.EventHLCTimestamp{PhysicalMS: remote.HLCPhysicalMS, Counter: remote.HLCCounter}
		_, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: projectID, ProjectUID: remote.ProjectUID, ProjectName: remote.ProjectName,
			IssueUID: remote.IssueUID, RelatedIssueUID: remote.RelatedIssueUID,
			Type: remote.Type, Actor: remote.Actor, Payload: string(payload),
			UID: remote.EventUID, OriginInstanceUID: remote.OriginInstanceUID,
			HLC: &clock, CreatedAt: createdAt, ContentHash: remote.ContentHash,
		})
		if err != nil {
			return err
		}
		inserted = true
		return nil
	})
	return inserted, err
}

// ReconcileLocalFederationEcho validates a pulled event that may already exist locally.
func (s *Store) ReconcileLocalFederationEcho(
	ctx context.Context,
	projectID int64,
	remote db.RemoteEvent,
) (bool, error) {
	payload, _, err := db.ValidateRemoteEventContentHash(remote)
	if err != nil {
		return false, err
	}
	found := false
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		found = false
		existing, err := scanEvent(tx.QueryRowContext(ctx,
			eventSelect+` WHERE e.project_id=$1 AND e.uid=$2 FOR UPDATE OF e`, projectID, remote.EventUID))
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if existing.ContentHash == remote.ContentHash {
			return nil
		}
		matches, err := db.LocalEchoMatchesCanonicalSnapshot(existing, remote)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, remote.EventUID)
		}
		_, err = tx.ExecContext(ctx, `UPDATE events SET payload=$1,content_hash=$2 WHERE id=$3`,
			string(payload), remote.ContentHash, existing.ID)
		return mapSQLError(err, nil)
	})
	return found, err
}
