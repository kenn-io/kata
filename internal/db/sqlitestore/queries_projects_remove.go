package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// RemoveProject archives a project: sets projects.deleted_at, hard-deletes
// every project_aliases row, and emits one project.removed event. Refuses
// with ErrProjectHasOpenIssues when the project still has open, non-deleted
// issues unless Force=true. The project row stays so events/issues keep a
// valid FK target; subsequent ListProjects / ProjectByName calls exclude
// it from the active surface.
func (d *Store) RemoveProject(ctx context.Context, p db.RemoveProjectParams) (db.Project, *db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Project{}, nil, fmt.Errorf("begin remove project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.ProjectID))
	if err != nil {
		return db.Project{}, nil, err
	}
	if isSystemProject(project) {
		return db.Project{}, nil, db.ErrNotFound
	}
	if project.DeletedAt != nil {
		return db.Project{}, nil, db.ErrProjectAlreadyArchived
	}

	openIssues, err := countOpenIssues(ctx, tx, project.ID)
	if err != nil {
		return db.Project{}, nil, err
	}
	if openIssues > 0 && !p.Force {
		return db.Project{}, nil, &db.ProjectHasOpenIssuesError{OpenIssues: openIssues}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		project.ID); err != nil {
		return db.Project{}, nil, fmt.Errorf("archive project: %w", err)
	}

	aliasCount, err := deleteAllAliasesForProject(ctx, tx, project.ID)
	if err != nil {
		return db.Project{}, nil, err
	}

	payload, err := json.Marshal(struct {
		AliasCount int64 `json:"alias_count"`
		OpenIssues int64 `json:"open_issues"`
		Force      bool  `json:"force,omitempty"`
	}{AliasCount: aliasCount, OpenIssues: openIssues, Force: p.Force})
	if err != nil {
		return db.Project{}, nil, fmt.Errorf("marshal project.removed payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		Type:        "project.removed",
		Actor:       p.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.Project{}, nil, err
	}

	updated, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, project.ID))
	if err != nil {
		return db.Project{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return db.Project{}, nil, fmt.Errorf("commit remove project: %w", err)
	}
	return updated, &evt, nil
}

// RestoreProject clears projects.deleted_at and emits one project.restored
// event. Active projects return a retry-safe no-op envelope: the project row,
// nil event, and changed=false. Unknown projects return ErrNotFound.
func (d *Store) RestoreProject(ctx context.Context, projectID int64, actor string) (db.Project, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Project{}, nil, false, fmt.Errorf("begin restore project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, projectID))
	if err != nil {
		return db.Project{}, nil, false, err
	}
	if isSystemProject(project) {
		return db.Project{}, nil, false, db.ErrNotFound
	}
	if project.DeletedAt == nil {
		if err := tx.Commit(); err != nil {
			return db.Project{}, nil, false, fmt.Errorf("commit restore project noop: %w", err)
		}
		return project, nil, false, nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE projects SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`,
		project.ID)
	if err != nil {
		return db.Project{}, nil, false, fmt.Errorf("restore project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Project{}, nil, false, fmt.Errorf("restore project rows affected: %w", err)
	}
	if n == 0 {
		if err := tx.Commit(); err != nil {
			return db.Project{}, nil, false, fmt.Errorf("commit restore project race noop: %w", err)
		}
		updated, err := d.ProjectByID(ctx, project.ID)
		if err != nil {
			return db.Project{}, nil, false, err
		}
		return updated, nil, false, nil
	}

	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		Type:        "project.restored",
		Actor:       actor,
		Payload:     "{}",
	})
	if err != nil {
		return db.Project{}, nil, false, err
	}
	updated, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, project.ID))
	if err != nil {
		return db.Project{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Project{}, nil, false, fmt.Errorf("commit restore project: %w", err)
	}
	return updated, &evt, true, nil
}

// DetachProjectAlias deletes one project_aliases row and emits a
// project.alias_removed event. Refuses with ErrAliasIsLast when this is the
// only alias for its project unless Force=true — the last alias is what
// connects a workspace path to a project, so dropping it without intent
// orphans the project from the filesystem.
//
// Lookup is keyed on (project_id, alias_id) inside the transaction so a
// reassignment between handler preflight and this call cannot drop an
// alias from a different project than the request named.
func (d *Store) DetachProjectAlias(ctx context.Context, p db.DetachAliasParams) (db.ProjectAlias, *db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("begin detach alias: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	alias, err := scanAlias(tx.QueryRowContext(ctx,
		aliasSelect+` WHERE id = ? AND project_id = ?`, p.AliasID, p.ProjectID))
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, alias.ProjectID))
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}

	var siblingCount int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_aliases WHERE project_id = ?`, alias.ProjectID).Scan(&siblingCount); err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("count sibling aliases: %w", err)
	}
	if siblingCount <= 1 && !p.Force {
		return db.ProjectAlias{}, nil, db.ErrAliasIsLast
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_aliases WHERE id = ? AND project_id = ?`,
		alias.ID, alias.ProjectID); err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("delete alias: %w", err)
	}

	payload, err := json.Marshal(struct {
		AliasIdentity string `json:"alias_identity"`
		AliasKind     string `json:"alias_kind"`
		WasLast       bool   `json:"was_last,omitempty"`
		Force         bool   `json:"force,omitempty"`
	}{
		AliasIdentity: alias.AliasIdentity,
		AliasKind:     alias.AliasKind,
		WasLast:       siblingCount <= 1,
		Force:         p.Force,
	})
	if err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("marshal project.alias_removed payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		Type:        "project.alias_removed",
		Actor:       p.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("commit detach alias: %w", err)
	}
	return alias, &evt, nil
}

// countOpenIssues returns the number of open, non-deleted issues belonging
// to projectID. Used by RemoveProject's refusal check.
func countOpenIssues(ctx context.Context, tx *sql.Tx, projectID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues
		 WHERE project_id = ? AND status = 'open' AND deleted_at IS NULL`,
		projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open issues: %w", err)
	}
	return n, nil
}

// deleteAllAliasesForProject hard-deletes every project_aliases row for the
// project and returns the count for the audit event payload.
func deleteAllAliasesForProject(ctx context.Context, tx *sql.Tx, projectID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_aliases WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count aliases for archive: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_aliases WHERE project_id = ?`, projectID); err != nil {
		return 0, fmt.Errorf("delete aliases for archive: %w", err)
	}
	return n, nil
}
