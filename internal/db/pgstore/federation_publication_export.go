package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"iter"

	"go.kenn.io/kata/internal/db"
)

var _ db.FederationPublicationSnapshotReader = (*Store)(nil)

// FederationPublicationCapacity returns the maximum accepted publication size.
func (s *Store) FederationPublicationCapacity() int64 { return s.relayCheckpointCapacity() }

// FederationPublicationCheckpointSize measures the reset baseline within the export snapshot.
func (s *Store) FederationPublicationCheckpointSize(ctx context.Context, projectID int64) (db.RelayCheckpointSize, error) {
	tx, ok := s.exportQ.(*sql.Tx)
	if !ok {
		return db.RelayCheckpointSize{}, fmt.Errorf("%w: checkpoint measurement requires export snapshot", db.ErrFederationPublicationInvalid)
	}
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	actor, err := relayCheckpointActorBoundTx(ctx, tx, projectID)
	if err != nil {
		return db.RelayCheckpointSize{}, err
	}
	return s.relayCheckpointSizeTx(ctx, tx, project, actor)
}

// ExportFederationPublicationEvents uses the repeatable export snapshot rather
// than the live pool and never repairs or scrubs original event content.
func (s *Store) ExportFederationPublicationEvents(ctx context.Context, projectID int64) iter.Seq2[db.EventExport, error] {
	return streamExportRows(ctx, s, "publication events", `SELECT e.id,e.uid,e.origin_instance_uid,e.project_id,p.uid,e.project_name,
	 e.issue_id,e.issue_uid,e.related_issue_id,e.related_issue_uid,e.type,e.actor,e.payload,e.hlc_physical_ms,e.hlc_counter,e.content_hash,e.created_at
	 FROM events e JOIN projects p ON p.id=e.project_id WHERE e.project_id=$1 ORDER BY e.id`, []any{projectID}, func(rows *sql.Rows) (db.EventExport, error) {
		var r db.EventExport
		var payload string
		err := rows.Scan(&r.ID, &r.UID, &r.OriginInstanceUID, &r.ProjectID, &r.ProjectUID, &r.ProjectName, &r.IssueID, &r.IssueUID, &r.RelatedIssueID, &r.RelatedIssueUID, &r.Type, &r.Actor, &payload, &r.HLCPhysicalMS, &r.HLCCounter, &r.ContentHash, &r.CreatedAt)
		r.Payload = jsontext.Value(payload)
		return r, err
	})
}
