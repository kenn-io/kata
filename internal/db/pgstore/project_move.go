package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// MoveIssueProject rehomes one active, non-recurring issue while preserving
// stable UID relationships and allocating a fresh target-local short ID.
func (s *Store) MoveIssueProject(ctx context.Context, input db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	var output db.MoveIssueProjectOut
	if input.FromProjectID == input.ToProjectID {
		return output, fmt.Errorf("source and target projects are the same")
	}
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		current, source, err := lockedIssueTx(ctx, tx, input.IssueID, false)
		if err != nil {
			return err
		}
		if current.ProjectID != input.FromProjectID {
			return db.ErrNotFound
		}
		if err := ensureFederatedMoveAllowedTx(ctx, tx, input.FromProjectID, input.ToProjectID); err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, input.FromProjectID); err != nil {
			return err
		}
		if input.IfMatchRev != current.Revision {
			return &db.RevisionConflictError{CurrentRevision: current.Revision}
		}
		if current.RecurrenceID != nil {
			return &db.RecurrencePinnedError{}
		}
		target, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, input.ToProjectID))
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, target.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, target.ID); err != nil {
			return mapSQLError(err, nil)
		}
		newShortID, err := s.resolveShortIDTx(ctx, tx, target.ID, current.UID, "")
		if err != nil {
			return fmt.Errorf("allocate short_id in target: %w", err)
		}
		newRevision := current.Revision + 1
		updatedAt := mutationTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE issues
SET project_id = $1, short_id = $2, revision = $3, updated_at = $4
WHERE id = $5`, target.ID, newShortID, newRevision, updatedAt, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issue_claims SET project_id = $1 WHERE issue_id = $2`, target.ID, current.ID); err != nil {
			return fmt.Errorf("rehome issue claims: %w", mapSQLError(err, nil))
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE pending_claim_requests SET project_id = $1 WHERE issue_id = $2`, target.ID, current.ID); err != nil {
			return fmt.Errorf("rehome pending claim requests: %w", mapSQLError(err, nil))
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings source
WHERE source.issue_id = $1 AND source.project_id = $2
  AND EXISTS (
    SELECT 1 FROM import_mappings target
     WHERE target.project_id = $3
       AND target.source = source.source
       AND target.external_id = source.external_id
       AND target.object_type = source.object_type
  )`, current.ID, source.ID, target.ID); err != nil {
			return fmt.Errorf("drop colliding import mappings: %w", mapSQLError(err, nil))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE import_mappings
SET project_id = $1 WHERE issue_id = $2 AND project_id = $3`,
			target.ID, current.ID, source.ID); err != nil {
			return fmt.Errorf("rehome import mappings: %w", mapSQLError(err, nil))
		}

		payload, err := json.Marshal(map[string]string{
			"issue_uid": current.UID, "from_project_uid": source.UID,
			"from_short_id": current.ShortID, "to_project_uid": target.UID,
			"to_short_id": newShortID, "updated_at": updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal issue move event: %w", err)
		}
		event, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: target.ID, ProjectUID: target.UID, ProjectName: target.Name,
			IssueID: &current.ID, IssueUID: &current.UID,
			Type: "issue.moved", Actor: input.Actor, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		output.Issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		output.EventID = event.ID
		output.NewShortID = newShortID
		output.NewRevision = newRevision
		return err
	})
	return output, err
}
