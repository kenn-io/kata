package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

type metadataKeyDiffPayload struct {
	From json.RawMessage `json:"from"`
	To   json.RawMessage `json:"to"`
}

type metadataTxRunner func(context.Context, transactionFunc) error

// PatchIssueMetadata applies a validated per-key patch under the issue's
// revision gate and emits the replayable diff in the same transaction.
func (s *Store) PatchIssueMetadata(ctx context.Context, input db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	return s.patchIssueMetadata(ctx, input, s.withSerializableTx)
}

func (s *Store) patchIssueMetadata(
	ctx context.Context,
	input db.PatchIssueMetadataIn,
	run metadataTxRunner,
) (db.PatchIssueMetadataOut, error) {
	var output db.PatchIssueMetadataOut
	for key, raw := range input.Patch {
		if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
			return output, fmt.Errorf("validate %q: %w", key, err)
		}
	}
	err := run(ctx, func(tx *sql.Tx) error {
		output = db.PatchIssueMetadataOut{}
		current, project, err := lockedIssueTx(ctx, tx, input.IssueID, false)
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, project.ID); err != nil {
			return err
		}
		if input.IfMatchRev != nil && *input.IfMatchRev != current.Revision {
			return &db.RevisionConflictError{CurrentRevision: current.Revision}
		}
		updated, diff, err := patchedMetadata(current.Metadata, input.Patch)
		if err != nil {
			return err
		}
		if len(diff) == 0 {
			output.Issue = current
			output.NewRevision = current.Revision
			return nil
		}
		updatedAt := mutationTimestamp()
		newRevision := current.Revision + 1
		if _, err := tx.ExecContext(ctx, `UPDATE issues
SET metadata = $1, revision = $2, updated_at = $3 WHERE id = $4`,
			string(updated), newRevision, updatedAt, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			Diff        map[string]metadataKeyDiffPayload `json:"diff"`
			RevisionNew int64                             `json:"revision_new"`
			UpdatedAt   string                            `json:"updated_at"`
		}{Diff: diff, RevisionNew: newRevision, UpdatedAt: updatedAt})
		if err != nil {
			return fmt.Errorf("marshal issue metadata event: %w", err)
		}
		output.Event, err = s.insertEventTx(ctx, tx,
			issueEventInput(current, project, "issue.metadata_updated", input.Actor, string(payload)))
		if err != nil {
			return err
		}
		output.Issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		output.Changed = true
		output.NewRevision = newRevision
		return err
	})
	return output, err
}

// PatchProjectMetadata applies a validated per-key patch under the project's
// revision gate and emits the replayable diff in the same transaction.
func (s *Store) PatchProjectMetadata(ctx context.Context, input db.PatchProjectMetadataIn) (db.PatchProjectMetadataOut, error) {
	return s.patchProjectMetadata(ctx, input, s.withSerializableTx)
}

func (s *Store) patchProjectMetadata(
	ctx context.Context,
	input db.PatchProjectMetadataIn,
	run metadataTxRunner,
) (db.PatchProjectMetadataOut, error) {
	var output db.PatchProjectMetadataOut
	for key, raw := range input.Patch {
		if err := metadata.Validate(metadata.ProjectRegistry, key, raw); err != nil {
			return output, fmt.Errorf("validate %q: %w", key, err)
		}
	}
	err := run(ctx, func(tx *sql.Tx) error {
		output = db.PatchProjectMetadataOut{}
		current, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, input.ProjectID))
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, current.ID); err != nil {
			return err
		}
		if input.IfMatchRev != nil && *input.IfMatchRev != current.Revision {
			return &db.RevisionConflictError{CurrentRevision: current.Revision}
		}
		updated, diff, err := patchedMetadata(current.Metadata, input.Patch)
		if err != nil {
			return err
		}
		if len(diff) == 0 {
			output.Project = current
			output.NewRevision = current.Revision
			return nil
		}
		newRevision := current.Revision + 1
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET metadata = $1, revision = $2 WHERE id = $3`,
			string(updated), newRevision, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			Diff        map[string]metadataKeyDiffPayload `json:"diff"`
			RevisionNew int64                             `json:"revision_new"`
		}{Diff: diff, RevisionNew: newRevision})
		if err != nil {
			return fmt.Errorf("marshal project metadata event: %w", err)
		}
		output.Event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: current.ID, ProjectUID: current.UID, ProjectName: current.Name,
			Type: "project.metadata_updated", Actor: input.Actor, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		output.Project, err = scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, current.ID))
		output.Changed = true
		output.NewRevision = newRevision
		return err
	})
	return output, err
}

func patchedMetadata(
	current db.JSONBlob,
	patch map[string]json.RawMessage,
) (json.RawMessage, map[string]metadataKeyDiffPayload, error) {
	updated, err := db.ApplyMetadataPatch(json.RawMessage(current), patch)
	if err != nil {
		return nil, nil, fmt.Errorf("apply metadata patch: %w", err)
	}
	diff, err := metadata.Diff(json.RawMessage(current), updated)
	if err != nil {
		return nil, nil, fmt.Errorf("compute metadata diff: %w", err)
	}
	payload := make(map[string]metadataKeyDiffPayload, len(diff))
	for key, value := range diff {
		payload[key] = metadataKeyDiffPayload{From: value.From, To: value.To}
	}
	return updated, payload, nil
}
