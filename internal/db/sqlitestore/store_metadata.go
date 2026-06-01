package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// PatchIssueMetadata applies a per-key patch to issues.metadata inside a single
// transaction. It validates all patch keys against metadata.IssueRegistry before
// opening a transaction, enforces If-Match (revision gate), and emits an
// issue.metadata_updated event whose payload carries the per-key diff plus
// revision_new. No event is emitted and the revision is not bumped when the
// patch produces no actual change (empty diff).
func (d *Store) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	var out db.PatchIssueMetadataOut

	// Validate all patch keys before opening a tx. A bad key/value never starts a tx.
	for key, raw := range in.Patch {
		if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
			return out, fmt.Errorf("validate %q: %w", key, err)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curMetadata string
		curRevision int64
		projectID   int64
		projectName string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT i.metadata, i.revision, i.project_id, p.name
		  FROM issues i JOIN projects p ON p.id = i.project_id
		 WHERE i.id = ? AND i.deleted_at IS NULL`,
		in.IssueID,
	).Scan(&curMetadata, &curRevision, &projectID, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("issue %d not found", in.IssueID)
	}
	if err != nil {
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, projectID); err != nil {
		return out, err
	}

	if in.IfMatchRev != curRevision {
		return out, &db.RevisionConflictError{CurrentRevision: curRevision}
	}

	// Apply the patch onto the current metadata to produce the new blob,
	// then diff old vs new to detect no-ops and build the event payload.
	newBlob, err := applyMetadataPatch(json.RawMessage(curMetadata), in.Patch)
	if err != nil {
		return out, fmt.Errorf("apply patch: %w", err)
	}

	diff, err := metadata.Diff(json.RawMessage(curMetadata), newBlob)
	if err != nil {
		return out, fmt.Errorf("compute diff: %w", err)
	}

	if len(diff) == 0 {
		// No-op: commit (no writes) and return Changed=false. Revision unchanged.
		if err := tx.Commit(); err != nil {
			return out, err
		}
		issue, err := d.IssueByID(ctx, in.IssueID)
		if err != nil {
			return out, err
		}
		out.Issue = issue
		out.Changed = false
		out.NewRevision = curRevision
		return out, nil
	}

	newRev := curRevision + 1
	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx, `
		UPDATE issues
		   SET metadata   = ?,
		       revision   = ?,
		       updated_at = ?
		 WHERE id = ?`,
		string(newBlob), newRev, ts, in.IssueID,
	); err != nil {
		return out, fmt.Errorf("update issue metadata: %w", err)
	}

	// Build a serializable diff for the event payload: {key: {from, to}}.
	type keyDiffPayload struct {
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	diffPayload := make(map[string]keyDiffPayload, len(diff))
	for k, kd := range diff {
		diffPayload[k] = keyDiffPayload{From: kd.From, To: kd.To}
	}
	payload, err := json.Marshal(struct {
		Diff        map[string]keyDiffPayload `json:"diff"`
		RevisionNew int64                     `json:"revision_new"`
		UpdatedAt   string                    `json:"updated_at"`
	}{diffPayload, newRev, ts})
	if err != nil {
		return out, fmt.Errorf("marshal event payload: %w", err)
	}

	ev, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   projectID,
		ProjectName: projectName,
		IssueID:     &in.IssueID,
		Type:        "issue.metadata_updated",
		Actor:       in.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return out, err
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}

	issue, err := d.IssueByID(ctx, in.IssueID)
	if err != nil {
		return out, err
	}
	out.Issue = issue
	out.Event = ev
	out.Changed = true
	out.NewRevision = newRev
	return out, nil
}

// PatchProjectMetadata applies a per-key patch to projects.metadata inside a
// single transaction. It validates all patch keys against metadata.ProjectRegistry
// before opening a transaction, enforces If-Match (revision gate), and emits a
// project.metadata_updated event whose payload carries the per-key diff plus
// revision_new. No event is emitted and the revision is not bumped when the
// patch produces no actual change (empty diff). Soft-deleted projects are rejected.
func (d *Store) PatchProjectMetadata(ctx context.Context, in db.PatchProjectMetadataIn) (db.PatchProjectMetadataOut, error) {
	var out db.PatchProjectMetadataOut

	// Validate all patch keys before opening a tx. A bad key/value never starts a tx.
	for key, raw := range in.Patch {
		if err := metadata.Validate(metadata.ProjectRegistry, key, raw); err != nil {
			return out, fmt.Errorf("validate %q: %w", key, err)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curMetadata string
		curRevision int64
		projectName string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT metadata, revision, name
		  FROM projects
		 WHERE id = ? AND deleted_at IS NULL`,
		in.ProjectID,
	).Scan(&curMetadata, &curRevision, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("project %d not found", in.ProjectID)
	}
	if err != nil {
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.ProjectID); err != nil {
		return out, err
	}

	if in.IfMatchRev != curRevision {
		return out, &db.RevisionConflictError{CurrentRevision: curRevision}
	}

	// Apply the patch onto the current metadata to produce the new blob,
	// then diff old vs new to detect no-ops and build the event payload.
	newBlob, err := applyMetadataPatch(json.RawMessage(curMetadata), in.Patch)
	if err != nil {
		return out, fmt.Errorf("apply patch: %w", err)
	}

	diff, err := metadata.Diff(json.RawMessage(curMetadata), newBlob)
	if err != nil {
		return out, fmt.Errorf("compute diff: %w", err)
	}

	if len(diff) == 0 {
		// No-op: commit (no writes) and return Changed=false. Revision unchanged.
		if err := tx.Commit(); err != nil {
			return out, err
		}
		project, err := d.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return out, err
		}
		out.Project = project
		out.Changed = false
		out.NewRevision = curRevision
		return out, nil
	}

	newRev := curRevision + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE projects
		   SET metadata = ?,
		       revision = ?
		 WHERE id = ?`,
		string(newBlob), newRev, in.ProjectID,
	); err != nil {
		return out, fmt.Errorf("update project metadata: %w", err)
	}

	// Build a serializable diff for the event payload: {key: {from, to}}.
	type keyDiffPayload struct {
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	diffPayload := make(map[string]keyDiffPayload, len(diff))
	for k, kd := range diff {
		diffPayload[k] = keyDiffPayload{From: kd.From, To: kd.To}
	}
	payload, err := json.Marshal(struct {
		Diff        map[string]keyDiffPayload `json:"diff"`
		RevisionNew int64                     `json:"revision_new"`
	}{diffPayload, newRev})
	if err != nil {
		return out, fmt.Errorf("marshal event payload: %w", err)
	}

	ev, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   in.ProjectID,
		ProjectName: projectName,
		Type:        "project.metadata_updated",
		Actor:       in.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return out, err
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}

	project, err := d.ProjectByID(ctx, in.ProjectID)
	if err != nil {
		return out, err
	}
	out.Project = project
	out.Event = ev
	out.Changed = true
	out.NewRevision = newRev
	return out, nil
}

// applyMetadataPatch merges patch keys into oldBlob, producing a new JSON
// object. Null values in the patch delete the corresponding key from the result.
// oldBlob may be empty or "null", treated as {}.
func applyMetadataPatch(oldBlob json.RawMessage, patch map[string]json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if len(oldBlob) > 0 && string(oldBlob) != "null" {
		if err := json.Unmarshal(oldBlob, &m); err != nil {
			return nil, fmt.Errorf("unmarshal current metadata: %w", err)
		}
	}
	if m == nil {
		m = make(map[string]json.RawMessage)
	}
	for k, v := range patch {
		if string(v) == "null" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal new metadata: %w", err)
	}
	return out, nil
}
