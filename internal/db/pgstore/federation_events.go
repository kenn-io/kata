package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/db"
)

var federationPushEventTypeList = "(" + db.FederationPushEventTypesSQL() + ")"

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
	if len(output) == limit && len(output) > 0 && db.IsFederationSnapshotEvent(output[len(output)-1].Type) {
		runStartAfterID := afterID
		for _, o := range slices.Backward(output) {
			if !db.IsFederationSnapshotEvent(o.Type) {
				runStartAfterID = o.ID
				break
			}
		}
		extra, err := s.queryPendingFederationPushEvents(ctx, eventSelect+`
WHERE e.project_id=$1 AND e.origin_instance_uid=$2 AND e.id>$3 AND e.type IN (`+db.FederationSnapshotEventTypesSQL()+`)
  AND NOT EXISTS (
    SELECT 1 FROM events barrier
     WHERE barrier.project_id=e.project_id
       AND barrier.origin_instance_uid=e.origin_instance_uid
       AND barrier.id>$4 AND barrier.id<e.id
       AND `+pgFederationPushEventTypeCondition("barrier.type")+`
       AND barrier.type NOT IN (`+db.FederationSnapshotEventTypesSQL()+`)
  )
ORDER BY e.id ASC`, projectID, originInstanceUID, output[len(output)-1].ID, runStartAfterID)
		if err != nil {
			return nil, err
		}
		output = append(output, extra...)
	}
	for index, event := range output {
		if !db.IsFederationSnapshotEvent(event.Type) {
			continue
		}
		for next := index + 1; next < len(output); next++ {
			if !db.IsFederationSnapshotEvent(output[next].Type) {
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

// InsertRemoteEvent preserves one portable event. See InsertRemoteEvents.
func (s *Store) InsertRemoteEvent(ctx context.Context, projectID int64, remote db.RemoteEvent) (bool, error) {
	inserted, err := s.InsertRemoteEvents(ctx, projectID, []db.RemoteEvent{remote})
	if err != nil {
		return false, err
	}
	return inserted[0], nil
}

// InsertRemoteEvents preserves a page of portable events in one transaction,
// assigning only their local row identities. One cron validator serves the
// whole page, so run identity history is read once per page rather than once
// per event. It reports, per event, whether the event was new.
func (s *Store) InsertRemoteEvents(ctx context.Context, projectID int64, events []db.RemoteEvent) ([]bool, error) {
	payloads := make([]jsontext.Value, len(events))
	createdAts := make([]string, len(events))
	for i, remote := range events {
		payload, createdAt, err := db.ValidateRemoteEventContentHash(remote)
		if err != nil {
			return nil, err
		}
		if err := db.ValidateFederationEntries(remote.Type, remote.EventUID, payload); err != nil {
			return nil, err
		}
		payloads[i], createdAts[i] = payload, createdAt
	}
	// Each retried attempt overwrites every entry.
	inserted := make([]bool, len(events))
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		validator := db.NewCronReplayValidator(tx, true)
		for i, remote := range events {
			var err error
			inserted[i], err = s.insertRemoteEventTx(ctx, tx, projectID, remote, payloads[i], createdAts[i], validator)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inserted, nil
}

func (s *Store) insertRemoteEventTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	remote db.RemoteEvent,
	payload jsontext.Value,
	createdAt string,
	validator *db.CronReplayValidator,
) (bool, error) {
	if db.EventRequiredFeatures(remote.Type) != "" {
		if err := db.ValidateAcceptedCronEvent(remote); err != nil {
			return false, err
		}
		var targetUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&targetUID); err != nil {
			return false, err
		}
		if targetUID != remote.ProjectUID {
			return false, db.ErrFederationIngestValidation
		}
		if err := validator.Validate(ctx, projectID, remote); err != nil {
			return false, err
		}
	}
	var existingHash string
	err := tx.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE uid=$1 FOR UPDATE`,
		remote.EventUID).Scan(&existingHash)
	if err == nil {
		if existingHash == remote.ContentHash {
			return false, nil
		}
		return false, fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, remote.EventUID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, mapSQLError(err, nil)
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
		return false, err
	}
	return true, nil
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
