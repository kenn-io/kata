package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

func RecordScopedNativePredicateTx(ctx context.Context, tx *sql.Tx, instanceUID string, event Event) error {
	if event.OriginInstanceUID != instanceUID {
		return nil
	}
	if event.IssueUID != nil {
		// Existing issuer rows alone participate. Read the actual post-write
		// native revision: title/proof-only writes that leave it unchanged must
		// not invent a close guard transition, while owner/metadata changes do.
		if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, event.ProjectUID, *event.IssueUID, "issue.close"); err != nil {
			return err
		}
	}
	var subject, kind string
	switch event.Type {
	case "issue.metadata_updated", "issue.soft_deleted", "issue.restored":
		if event.IssueUID == nil {
			return nil
		}
		subject, kind = *event.IssueUID, "issue.metadata"
	case "project.metadata_updated":
		subject, kind = event.ProjectUID, "project.metadata"
	case "project.removed", "project.restored":
		return RecordScopedNativeProjectParentPredicatesTx(ctx, tx, instanceUID, event.ProjectUID)
	case "issue.links_changed":
		if event.IssueUID == nil {
			return nil
		}
		subject, kind = *event.IssueUID, "issue.parent"
	default:
		return nil
	}
	if kind != "" {
		if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, event.ProjectUID, subject, kind); err != nil {
			return err
		}
	}
	if event.Type == "issue.soft_deleted" || event.Type == "issue.restored" {
		// Own parent eligibility is independent of an own metadata issuer row.
		if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, event.ProjectUID, subject, "issue.parent"); err != nil {
			return err
		}
	}
	if event.Type != "issue.soft_deleted" && event.Type != "issue.restored" {
		return nil
	}
	return recordScopedNativeParentDependentsTx(ctx, tx, instanceUID, event.ProjectUID, subject, false)
}

func RecordScopedNativeProjectParentPredicatesTx(ctx context.Context, tx *sql.Tx, instanceUID, projectUID string) error {
	if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, projectUID, projectUID, "project.metadata"); err != nil {
		return err
	}
	if err := recordScopedNativeParentDependentsTx(ctx, tx, instanceUID, projectUID, "", true); err != nil {
		return err
	}
	return recordScopedNativeProjectVersionPredicatesTx(ctx, tx, instanceUID, projectUID)
}

func recordScopedNativeProjectVersionPredicatesTx(ctx context.Context, tx *sql.Tx, instanceUID, projectUID string) error {
	// Only already-retained issue metadata, close and definition issuers participate.
	// The recorder independently checks actual root ownership/pins and reads the
	// SAME native current version, including its project's deletion state.
	rows, err := tx.QueryContext(ctx, `SELECT subject_uid,predicate_kind FROM federation_predicate_incarnations WHERE project_uid=$1 AND predicate_kind IN ('issue.metadata','issue.close','job.definition','workflow.definition') ORDER BY subject_uid,predicate_kind`, projectUID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type subjectKind struct{ subject, kind string }
	var retained []subjectKind
	for rows.Next() {
		var row subjectKind
		if err := rows.Scan(&row.subject, &row.kind); err != nil {
			return err
		}
		retained = append(retained, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Close the owning cursor before the same transaction reads native rows.
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range retained {
		if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, projectUID, row.subject, row.kind); err != nil {
			return err
		}
	}
	return nil
}

func recordScopedNativeParentDependentsTx(ctx context.Context, tx *sql.Tx, instanceUID, projectUID, subject string, peerProjectLifetime bool) error {
	// A project original has no issue UID. Parent edges identify affected
	// children; existing own-project issuers also cover unparented subjects.
	peerMatch := "peer.uid=$1 AND peer_project.uid=$2"
	args := []any{subject, projectUID}
	if peerProjectLifetime {
		peerMatch = "peer_project.uid=$1"
		args = []any{projectUID}
	}
	childrenQuery := `SELECT DISTINCT child_project.uid,child.uid
		FROM links l
		JOIN issues peer ON peer.id=l.to_issue_id
		JOIN projects peer_project ON peer_project.id=peer.project_id
		JOIN issues child ON child.id=l.from_issue_id
		JOIN projects child_project ON child_project.id=child.project_id
		WHERE l.type='parent' AND ` + peerMatch
	if peerProjectLifetime {
		childrenQuery = `SELECT own_project.uid,own_child.uid
			FROM issues own_child
			JOIN projects own_project ON own_project.id=own_child.project_id
			JOIN federation_predicate_incarnations retained
			  ON retained.project_uid=own_project.uid AND retained.subject_uid=own_child.uid
			 AND retained.predicate_kind='issue.parent'
			WHERE own_project.uid=$1
			UNION ` + childrenQuery
	}
	childrenQuery += ` ORDER BY 1,2`
	rows, err := tx.QueryContext(ctx, childrenQuery, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type parentChild struct{ projectUID, issueUID string }
	var children []parentChild
	for rows.Next() {
		var child parentChild
		if err := rows.Scan(&child.projectUID, &child.issueUID); err != nil {
			return err
		}
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, child := range children {
		if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, child.projectUID, child.issueUID, "issue.parent"); err != nil {
			return err
		}
	}
	return nil
}

func RecordScopedNativeParentPredicateTx(ctx context.Context, tx Transaction, instanceUID string, childID int64) error {
	var projectUID, childUID string
	err := tx.QueryRowContext(ctx, `SELECT p.uid,i.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=$1`, childID).Scan(&projectUID, &childUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return recordScopedNativePredicateTx(ctx, tx, instanceUID, projectUID, childUID, "issue.parent")
}

func recordScopedNativePredicateTx(ctx context.Context, tx Transaction, instanceUID, projectUID, subject, kind string) error {
	return recordScopedPredicateIncarnationTx(ctx, tx, instanceUID, projectUID, subject, kind, nil)
}

func recordScopedPredicateIncarnationTx(ctx context.Context, tx Transaction, instanceUID, projectUID, subject, kind string, acceptedIssueWriter *FoldClock) error {
	if acceptedIssueWriter != nil && kind != "issue.metadata" {
		return ErrScopedSourceConflict
	}
	return recordScopedPredicateWriterTx(ctx, tx, instanceUID, projectUID, subject, kind, projectUID, acceptedIssueWriter)
}

func recordScopedPredicateWriterTx(ctx context.Context, tx Transaction, instanceUID, projectUID, subject, kind, writerProjectUID string, acceptedWriter *FoldClock) error {
	if acceptedWriter != nil {
		if kind != "issue.metadata" && kind != "issue.parent" && kind != "project.metadata" {
			return ErrScopedSourceConflict
		}
		if writerProjectUID == "" || ((kind == "issue.metadata" || kind == "project.metadata") && writerProjectUID != projectUID) {
			return ErrScopedSourceConflict
		}
		if kind == "project.metadata" && subject != projectUID {
			return ErrScopedSourceConflict
		}
	}
	var incarnation int64
	var retained string
	err := tx.QueryRowContext(ctx, `SELECT i.incarnation,i.canonical_digest FROM federation_predicate_incarnations i JOIN federation_scoped_projects sp ON sp.project_uid=i.project_uid JOIN federation_root_keys k ON k.project_uid=i.project_uid AND k.active=1 WHERE i.project_uid=$1 AND i.subject_uid=$2 AND i.predicate_kind=$3 AND sp.protocol_floor=$4 AND sp.owner_view_uid IS NULL AND k.authority_uid=$5`, projectUID, subject, kind, ScopedRelayProtocolVersion, instanceUID).Scan(&incarnation, &retained)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	digest, err := ScopedPredicateCurrentTx(ctx, tx, projectUID, subject, kind)
	if err != nil {
		return err
	}
	if acceptedWriter != nil {
		var stored bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events e JOIN projects p ON p.id=e.project_id WHERE e.uid=$1 AND p.uid=$2 AND e.origin_instance_uid=$3 AND e.hlc_physical_ms=$4 AND e.hlc_counter=$5)`, acceptedWriter.EventUID, writerProjectUID, acceptedWriter.OriginInstanceUID, acceptedWriter.HLCPhysicalMS, acceptedWriter.HLCCounter).Scan(&stored); err != nil {
			return err
		}
		if !stored {
			return ErrScopedSourceConflict
		}
	}
	if digest == retained && acceptedWriter == nil {
		return nil
	}
	if _, err := nextScopedPredicateIncarnation(incarnation); err != nil {
		return err
	}
	// Ordinary native no-ops stay quiet, including at the incarnation cap.
	// A newly stored surviving metadata or GROUP parent writer can invalidate equal B;
	// the old retained digest and incarnation still guard this one update.
	result, err := tx.ExecContext(ctx, `UPDATE federation_predicate_incarnations SET incarnation=incarnation+1,canonical_digest=$4 WHERE project_uid=$1 AND subject_uid=$2 AND predicate_kind=$3 AND incarnation=$5 AND incarnation<$6 AND canonical_digest=$7`, projectUID, subject, kind, digest, incarnation, int64(math.MaxInt64), retained)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrScopedSourceConflict
	}
	return nil
}

func nextScopedPredicateIncarnation(current int64) (int64, error) {
	if current == math.MaxInt64 {
		return 0, errors.New("scoped predicate capacity exhausted")
	}
	return current + 1, nil
}
