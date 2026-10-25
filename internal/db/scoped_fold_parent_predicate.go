package db

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
)

// RecordScopedFoldParentPredicatesTx consumes the already reconciled GROUP
// projection and actual successful inserts, never prepared candidate UIDs.
func RecordScopedFoldParentPredicatesTx(ctx context.Context, tx *sql.Tx, instanceUID string, acceptedProjectID int64, groupProjectIDs []int64, parents map[string]ScopedParentWriter, actualInsertedUIDs []string) error {
	if len(actualInsertedUIDs) == 0 || len(parents) == 0 || len(groupProjectIDs) == 0 {
		return nil
	}
	if !slices.Contains(groupProjectIDs, acceptedProjectID) {
		return ErrScopedSourceConflict
	}
	var writerProjectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, acceptedProjectID).Scan(&writerProjectUID); err != nil {
		return err
	}
	inserted := make(map[string]struct{}, len(actualInsertedUIDs))
	for _, uid := range actualInsertedUIDs {
		inserted[uid] = struct{}{}
	}
	var children []string
	for child, writer := range parents {
		if _, ok := inserted[writer.Clock.EventUID]; ok {
			children = append(children, child)
		}
	}
	sort.Strings(children)
	for _, child := range children {
		writer := parents[child]
		var childID, childProjectID int64
		var childProjectUID string
		err := tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, child).Scan(&childID, &childProjectID, &childProjectUID)
		if errors.Is(err, sql.ErrNoRows) {
			// Preserve existing incomplete baseline/deferred endpoint behavior.
			continue
		}
		if err != nil {
			return err
		}
		if !slices.Contains(groupProjectIDs, childProjectID) {
			continue
		}
		var stored bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events e WHERE e.uid=$1 AND e.project_id=$2 AND e.origin_instance_uid=$3 AND e.hlc_physical_ms=$4 AND e.hlc_counter=$5)`, writer.Clock.EventUID, acceptedProjectID, writer.Clock.OriginInstanceUID, writer.Clock.HLCPhysicalMS, writer.Clock.HLCCounter).Scan(&stored); err != nil {
			return err
		}
		if !stored {
			return ErrScopedSourceConflict
		}
		var actualParent string
		var peerProjectID int64
		err = tx.QueryRowContext(ctx, `SELECT peer.uid,peer.project_id FROM links l JOIN issues peer ON peer.id=l.to_issue_id JOIN projects p ON p.id=peer.project_id WHERE l.from_issue_id=$1 AND l.type='parent' AND peer.deleted_at IS NULL AND p.deleted_at IS NULL`, childID).Scan(&actualParent, &peerProjectID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		matched := actualParent == writer.ParentUID && (actualParent == "" || slices.Contains(groupProjectIDs, peerProjectID))
		if matched {
			if err := recordScopedPredicateWriterTx(ctx, tx, instanceUID, childProjectUID, child, "issue.parent", writerProjectUID, &writer.Clock); err != nil {
				return err
			}
		} else {
			// Never force an unapplied/deferred GROUP parent into current state.
			// Only a real native digest delta may advance this existing issuer.
			if err := recordScopedNativePredicateTx(ctx, tx, instanceUID, childProjectUID, child, "issue.parent"); err != nil {
				return err
			}
		}
	}
	return nil
}
