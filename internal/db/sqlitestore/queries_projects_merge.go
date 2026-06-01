package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// MergeProjects moves every project-scoped row from SourceProjectID into
// TargetProjectID, then deletes the source project. Source-side issues whose
// short_ids collide with target-side short_ids are auto-extended in
// ULID-ascending order (spec §5.2); existing target short_ids stay put. The
// returned ShortIDExtensions list reports each shifted issue's pre/post values.
func (d *Store) MergeProjects(ctx context.Context, p db.MergeProjectsParams) (db.ProjectMergeResult, error) {
	if p.SourceProjectID == p.TargetProjectID {
		return db.ProjectMergeResult{}, db.ErrProjectMergeSameProject
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("begin merge projects: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	source, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.SourceProjectID))
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("load source project: %w", err)
	}
	target, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.TargetProjectID))
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("load target project: %w", err)
	}
	if isSystemProject(source) || isSystemProject(target) {
		return db.ProjectMergeResult{}, db.ErrNotFound
	}
	if source.DeletedAt != nil {
		return db.ProjectMergeResult{}, db.ErrProjectMergeArchivedSource
	}
	if target.DeletedAt != nil {
		return db.ProjectMergeResult{}, db.ErrProjectMergeArchivedTarget
	}
	if err := rejectFederatedProjectMerge(ctx, tx, source.ID, target.ID); err != nil {
		return db.ProjectMergeResult{}, err
	}

	mappingCollisions, err := projectMergeImportMappingCollisions(ctx, tx, p.SourceProjectID, p.TargetProjectID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	if len(mappingCollisions) > 0 {
		return db.ProjectMergeResult{}, &db.ProjectMergeImportMappingCollisionError{Mappings: mappingCollisions}
	}

	// Reconcile short_id collisions BEFORE the bulk UPDATE moves issues onto
	// the target. The UNIQUE(project_id, short_id) index would otherwise reject
	// the move at the database layer. Each source-side issue is rewritten to
	// its smallest non-colliding length across both projects in ULID-ascending
	// order so the result is deterministic (spec §5.2).
	extensions, err := extendCollidingSourceShortIDs(ctx, tx, source.ID, target.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}

	issuesMoved, err := countProjectRows(ctx, tx, "issues", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	aliasesMoved, err := countProjectRows(ctx, tx, "project_aliases", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	eventsMoved, err := countProjectRows(ctx, tx, "events", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	purgeLogsMoved, err := countProjectRows(ctx, tx, "purge_log", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE issues SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move issues: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE links SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move links: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET project_id = ?, project_name = ? WHERE project_id = ?`,
		target.ID, target.Name, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move events: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE purge_log SET project_id = ?, project_name = ? WHERE project_id = ?`,
		target.ID, target.Name, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move purge log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move import mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_aliases SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move aliases: %w", err)
	}

	if p.TargetName != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET name = ? WHERE id = ?`,
			*p.TargetName, target.ID); err != nil {
			return db.ProjectMergeResult{}, fmt.Errorf("update target project: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("delete source project: %w", err)
	}

	mergedTarget, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, target.ID))
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("reload target project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("commit merge projects: %w", err)
	}

	return db.ProjectMergeResult{
		Source:            source,
		Target:            mergedTarget,
		IssuesMoved:       issuesMoved,
		AliasesMoved:      aliasesMoved,
		EventsMoved:       eventsMoved,
		PurgeLogsMoved:    purgeLogsMoved,
		ShortIDExtensions: extensions,
	}, nil
}

func rejectFederatedProjectMerge(ctx context.Context, tx *sql.Tx, sourceID, targetID int64) error {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM federation_bindings WHERE project_id IN (?, ?)`,
		sourceID, targetID).Scan(&n); err != nil {
		return fmt.Errorf("check federation merge binding: %w", err)
	}
	if n > 0 {
		return db.ErrProjectMergeFederationBinding
	}
	return nil
}

// extendCollidingSourceShortIDs rewrites the short_id of every source-side
// issue whose value would collide with an existing target-side short_id on
// move. Iteration is ULID-ascending so replays produce the same result
// (spec §5.2). Each replacement is the shortest length L >= shortid.MinLength
// at which the candidate is free in BOTH source and target — checking both
// projects together avoids transient duplicates on the source side before
// the bulk UPDATE runs.
//
// Target-side purge_log tombstones are honored as collisions too: a source
// issue moving into a target whose purge_log already claims that short_id
// would otherwise silently take a slot a previously-purged issue owned.
func extendCollidingSourceShortIDs(
	ctx context.Context,
	tx *sql.Tx,
	sourceID, targetID int64,
) ([]db.ShortIDExtension, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.uid, s.short_id
		FROM issues s
		WHERE s.project_id = ?
		  AND (
		    EXISTS (SELECT 1 FROM issues t
		             WHERE t.project_id = ? AND t.short_id = s.short_id)
		    OR
		    EXISTS (SELECT 1 FROM purge_log p
		             WHERE p.project_id = ? AND p.short_id = s.short_id)
		  )
		ORDER BY s.uid ASC`, sourceID, targetID, targetID)
	if err != nil {
		return nil, fmt.Errorf("scan source/target short_id collisions: %w", err)
	}
	type collider struct {
		id       int64
		uid      string
		oldShort string
	}
	var colliders []collider
	for rows.Next() {
		var c collider
		if err := rows.Scan(&c.id, &c.uid, &c.oldShort); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan collider row: %w", err)
		}
		colliders = append(colliders, c)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close collider rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collider rows: %w", err)
	}

	var extensions []db.ShortIDExtension
	for _, c := range colliders {
		// Search strictly longer than the colliding length: spec §5.2 forbids
		// shortening on merge, and the current length is known to collide
		// against a target row (otherwise this issue wouldn't be a collider).
		// Without the +1 floor, a source issue that was extended past
		// shortid.MinLength to dodge source-side neighbors which were later
		// purged would be re-keyed down to MinLength here, violating the rule.
		newShortID, err := assignShortIDIn(ctx, tx, []int64{sourceID, targetID}, c.uid, len(c.oldShort)+1)
		if err != nil {
			return nil, fmt.Errorf("auto-extend short_id for %s: %w", c.uid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET short_id = ? WHERE id = ?`,
			newShortID, c.id); err != nil {
			return nil, fmt.Errorf("update extended short_id for %s: %w", c.uid, err)
		}
		extensions = append(extensions, db.ShortIDExtension{
			UID:              c.uid,
			PreMergeShortID:  c.oldShort,
			PostMergeShortID: newShortID,
		})
	}
	return extensions, nil
}

func projectMergeImportMappingCollisions(
	ctx context.Context,
	tx *sql.Tx,
	sourceID, targetID int64,
) ([]db.ProjectMergeImportMappingCollision, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.source, s.external_id, s.object_type
		FROM import_mappings s
		INNER JOIN import_mappings t
		  ON t.project_id = ?
		 AND t.source = s.source
		 AND t.external_id = s.external_id
		 AND t.object_type = s.object_type
		WHERE s.project_id = ?
		ORDER BY s.source, s.object_type, s.external_id
		LIMIT 20`, targetID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("check project merge import mapping collisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.ProjectMergeImportMappingCollision
	for rows.Next() {
		var c db.ProjectMergeImportMappingCollision
		if err := rows.Scan(&c.Source, &c.ExternalID, &c.ObjectType); err != nil {
			return nil, fmt.Errorf("scan project merge import mapping collision: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func countProjectRows(ctx context.Context, tx *sql.Tx, table string, projectID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s rows: %w", table, err)
	}
	return n, nil
}
