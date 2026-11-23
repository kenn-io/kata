package sqlitestore

import (
	"context"
	"database/sql"
	"math"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// relayCheckpointEventPage bounds one page of portable events read while a
// checkpoint is assembled or measured.
const relayCheckpointEventPage = 500

// RelayCheckpointSize measures the signed checkpoint this root would build for
// the project now, from the same records and serialization as creation,
// without writing baseline events or a checkpoint.
func (d *Store) RelayCheckpointSize(ctx context.Context, projectID int64) (db.RelayCheckpointSize, error) {
	var size db.RelayCheckpointSize
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=?`, projectID))
		if err != nil {
			return err
		}
		actor, err := relayCheckpointActorBoundTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		size, err = d.relayCheckpointSizeTx(ctx, tx, project, actor)
		return err
	})
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	return size, nil
}

func (d *Store) relayCheckpointCapacity() int64 {
	return db.RelayCheckpointCapacity(d.relayCheckpointCapacityBytes)
}

// relayCheckpointActorBoundTx returns the live relay account on the project
// whose encoding is largest, so a status measurement bounds the records a
// capture for any of those relays would write.
func relayCheckpointActorBoundTx(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT bound_actor FROM federation_enrollments WHERE project_id=? AND revoked_at IS NULL AND relay_protocol_version=?`, projectID, db.RelayProtocolVersion)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var actors []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			return "", err
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return db.RelayCheckpointActorBound(actors)
}

// relayCheckpointSizeTx serializes retained history plus the baseline a
// capture would add and measures the result against the capacity.
func (d *Store) relayCheckpointSizeTx(ctx context.Context, tx *sql.Tx, project db.Project, actor string) (db.RelayCheckpointSize, error) {
	records, err := d.relayCheckpointRecordsTx(ctx, tx, project, math.MaxInt64)
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	issues, err := d.relayCheckpointIssuesTx(ctx, tx, project.ID)
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	cron, err := db.CronDefinitionSnapshots(ctx, tx, project)
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	baseline, err := db.RelayCheckpointBaselineRecords(project, d.instanceUID, actor, issues, cron)
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	records.Entities = baseline.Entities
	records.Provenance.Receipts = append(records.Provenance.Receipts, baseline.Receipts...)
	snapshot, err := records.Snapshot()
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	return db.RelayCheckpointSize{Bytes: db.RootResetSnapshotBytes(snapshot), Capacity: d.relayCheckpointCapacity()}, nil
}

func (d *Store) relayCheckpointIssuesTx(ctx context.Context, tx *sql.Tx, projectID int64) ([]db.RelayCheckpointIssue, error) {
	issues, err := federationIssuesForSnapshot(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]db.RelayCheckpointIssue, 0, len(issues))
	for _, issue := range issues {
		payload, err := d.federationIssueSnapshotPayload(ctx, tx, issue)
		if err != nil {
			return nil, err
		}
		out = append(out, db.RelayCheckpointIssue{IssueUID: issue.UID, Payload: payload})
	}
	return out, nil
}

// relayCheckpointRecordsTx loads every record a root checkpoint for project
// serializes. Portable events below boundaryID are retained history; events at
// or above it are the current-state records a checkpoint creation just wrote.
func (d *Store) relayCheckpointRecordsTx(ctx context.Context, tx *sql.Tx, project db.Project, boundaryID int64) (db.RelayCheckpointRecords, error) {
	var records db.RelayCheckpointRecords
	crosses := func(event db.RemoteEvent) (bool, error) {
		return relayEventCrossesProjectTx(ctx, tx, project.ID, event)
	}
	for after := int64(0); ; {
		events, err := relayCheckpointEventPageTx(ctx, tx, project.ID, after)
		if err != nil {
			return records, err
		}
		if len(events) == 0 {
			break
		}
		if err := records.AppendEvents(events, boundaryID, crosses); err != nil {
			return records, err
		}
		after = events[len(events)-1].ID
	}
	var err error
	records.Provenance, err = relayCheckpointProvenanceTx(ctx, tx, project.UID)
	if err != nil {
		return records, err
	}
	records.Artifacts, err = relayCheckpointArtifactsTx(ctx, tx, project)
	return records, err
}

// relayCheckpointEventPageTx reads one page of portable events after an id.
// Pages are closed before the records are encoded, so the per-event boundary
// checks never run against an open cursor.
func relayCheckpointEventPageTx(ctx context.Context, tx *sql.Tx, projectID, after int64) ([]db.Event, error) {
	// #nosec G202 -- The event-type predicate is fixed native SQL; all project and cursor values remain bound.
	rows, err := tx.QueryContext(ctx, eventSelect+` WHERE e.project_id=? AND e.id>? AND `+federationPushEventTypeCondition("e.type")+` ORDER BY e.id LIMIT ?`, projectID, after, relayCheckpointEventPage)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []db.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func relayCheckpointProvenanceTx(ctx context.Context, tx *sql.Tx, projectUID string) (db.RootResetProvenance, error) {
	var provenance db.RootResetProvenance
	rows, err := tx.QueryContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys WHERE project_uid=? ORDER BY key_id`, projectUID)
	if err != nil {
		return provenance, err
	}
	for rows.Next() {
		pin, readErr := scanRootPin(rows)
		if readErr != nil {
			err = readErr
			break
		}
		provenance.Keys = append(provenance.Keys, pin)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return provenance, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=? ORDER BY reset_epoch,sequence`, projectUID)
	if err != nil {
		return provenance, err
	}
	for rows.Next() {
		receipt, readErr := readReceipt(rows)
		if readErr != nil {
			err = readErr
			break
		}
		provenance.Receipts = append(provenance.Receipts, receipt)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return provenance, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT ep.project_uid,ep.kind,ep.entity_uid,ep.event_uid FROM federation_entity_provenance ep
WHERE ep.project_uid=? AND (
 (ep.kind='issue' AND EXISTS(SELECT 1 FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=ep.project_uid AND i.uid=ep.entity_uid)) OR
 (ep.kind='comment' AND EXISTS(SELECT 1 FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE p.uid=ep.project_uid AND c.uid=ep.entity_uid)))
ORDER BY ep.kind,ep.entity_uid`, projectUID)
	if err != nil {
		return provenance, err
	}
	for rows.Next() {
		var ref db.EntityProvenance
		if err = rows.Scan(&ref.ProjectUID, &ref.Kind, &ref.EntityUID, &ref.EventUID); err != nil {
			break
		}
		provenance.Entities = append(provenance.Entities, ref)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	return provenance, err
}

// relayCheckpointArtifactsTx lists complete retained manifests that match each
// issue's current content. Bytes stay on the artifact stream.
func relayCheckpointArtifactsTx(ctx context.Context, tx *sql.Tx, project db.Project) ([]embedding.ArtifactManifest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.digest,a.manifest,i.title,i.body FROM federation_embedding_artifacts a JOIN issues i ON i.uid=a.issue_uid AND i.project_id=? WHERE a.project_uid=? AND a.staging_expires_at IS NULL AND i.deleted_at IS NULL ORDER BY a.digest`, project.ID, project.UID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var artifacts []embedding.ArtifactManifest
	for rows.Next() {
		var digest, raw, title, body string
		if err := rows.Scan(&digest, &raw, &title, &body); err != nil {
			return nil, err
		}
		manifest, current, err := db.RelayCheckpointArtifact(project.UID, digest, raw, title, body)
		if err != nil {
			return nil, err
		}
		if current {
			artifacts = append(artifacts, manifest)
		}
	}
	return artifacts, rows.Err()
}
