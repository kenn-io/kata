package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrProjectMergeSameProject is returned when source and target are the
	// same project row.
	ErrProjectMergeSameProject = errors.New("cannot merge a project into itself")

	// ErrProjectMergeImportMappingCollision is returned when moving source import
	// mappings would violate the target's source identity uniqueness.
	ErrProjectMergeImportMappingCollision = errors.New("project merge import mapping collision")

	// ErrProjectMergeArchivedSource is returned when MergeProjects is asked
	// to merge from a project that's been archived via RemoveProject (#24).
	// Merging out of an archive is a restore-then-merge flow that doesn't
	// exist yet; rather than silently undoing the archive we refuse here.
	ErrProjectMergeArchivedSource = errors.New("source project is archived")

	// ErrProjectMergeArchivedTarget is returned when the target project is
	// archived. Folding live work into an archived project would resurrect
	// the archive's identity, which is exactly what archival is meant to
	// prevent.
	ErrProjectMergeArchivedTarget = errors.New("target project is archived")
)

// ProjectMergeImportMappingCollision identifies one import mapping identity
// that already exists on the target project.
type ProjectMergeImportMappingCollision struct {
	Source     string
	ExternalID string
	ObjectType string
}

// ProjectMergeImportMappingCollisionError carries the import mapping
// identities that blocked a merge.
type ProjectMergeImportMappingCollisionError struct {
	Mappings []ProjectMergeImportMappingCollision
}

func (e *ProjectMergeImportMappingCollisionError) Error() string {
	parts := make([]string, 0, len(e.Mappings))
	for _, m := range e.Mappings {
		parts = append(parts, fmt.Sprintf("%s/%s/%s", m.Source, m.ObjectType, m.ExternalID))
	}
	return fmt.Sprintf("%v: %s", ErrProjectMergeImportMappingCollision, strings.Join(parts, ", "))
}

func (e *ProjectMergeImportMappingCollisionError) Unwrap() error {
	return ErrProjectMergeImportMappingCollision
}

// MergeProjectsParams identifies a source project to fold into a surviving
// target project. The target keeps its id.
type MergeProjectsParams struct {
	SourceProjectID int64
	TargetProjectID int64
	TargetName      *string
}

// ShortIDExtension records a single source-side issue whose short_id was
// extended during merge to break a collision with an existing target-side
// short_id. The UID is stable across the shift; PreMergeShortID is the
// value the issue carried on the source project, PostMergeShortID is the
// value stored after the merge.
type ShortIDExtension struct {
	UID              string
	PreMergeShortID  string
	PostMergeShortID string
}

// ProjectMergeResult summarizes the rows moved by MergeProjects.
type ProjectMergeResult struct {
	Source            Project            `json:"source"`
	Target            Project            `json:"target"`
	IssuesMoved       int64              `json:"issues_moved"`
	AliasesMoved      int64              `json:"aliases_moved"`
	EventsMoved       int64              `json:"events_moved"`
	PurgeLogsMoved    int64              `json:"purge_logs_moved"`
	ShortIDExtensions []ShortIDExtension `json:"short_id_extensions,omitempty"`
}

// MergeProjects moves every project-scoped row from SourceProjectID into
// TargetProjectID, then deletes the source project. Source-side issues whose
// short_ids collide with target-side short_ids are auto-extended in
// ULID-ascending order (spec §5.2); existing target short_ids stay put. The
// returned ShortIDExtensions list reports each shifted issue's pre/post values.
func (d *DB) MergeProjects(ctx context.Context, p MergeProjectsParams) (ProjectMergeResult, error) {
	if p.SourceProjectID == p.TargetProjectID {
		return ProjectMergeResult{}, ErrProjectMergeSameProject
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return ProjectMergeResult{}, fmt.Errorf("begin merge projects: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	source, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.SourceProjectID))
	if err != nil {
		return ProjectMergeResult{}, fmt.Errorf("load source project: %w", err)
	}
	target, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.TargetProjectID))
	if err != nil {
		return ProjectMergeResult{}, fmt.Errorf("load target project: %w", err)
	}
	if isSystemProject(source) || isSystemProject(target) {
		return ProjectMergeResult{}, ErrNotFound
	}
	if source.DeletedAt != nil {
		return ProjectMergeResult{}, ErrProjectMergeArchivedSource
	}
	if target.DeletedAt != nil {
		return ProjectMergeResult{}, ErrProjectMergeArchivedTarget
	}

	mappingCollisions, err := projectMergeImportMappingCollisions(ctx, tx, p.SourceProjectID, p.TargetProjectID)
	if err != nil {
		return ProjectMergeResult{}, err
	}
	if len(mappingCollisions) > 0 {
		return ProjectMergeResult{}, &ProjectMergeImportMappingCollisionError{Mappings: mappingCollisions}
	}

	// Reconcile short_id collisions BEFORE the bulk UPDATE moves issues onto
	// the target. The UNIQUE(project_id, short_id) index would otherwise reject
	// the move at the database layer. Each source-side issue is rewritten to
	// its smallest non-colliding length across both projects in ULID-ascending
	// order so the result is deterministic (spec §5.2).
	extensions, err := extendCollidingSourceShortIDs(ctx, tx, source.ID, target.ID)
	if err != nil {
		return ProjectMergeResult{}, err
	}

	issuesMoved, err := countProjectRows(ctx, tx, "issues", source.ID)
	if err != nil {
		return ProjectMergeResult{}, err
	}
	aliasesMoved, err := countProjectRows(ctx, tx, "project_aliases", source.ID)
	if err != nil {
		return ProjectMergeResult{}, err
	}
	eventsMoved, err := countProjectRows(ctx, tx, "events", source.ID)
	if err != nil {
		return ProjectMergeResult{}, err
	}
	purgeLogsMoved, err := countProjectRows(ctx, tx, "purge_log", source.ID)
	if err != nil {
		return ProjectMergeResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE issues SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("move issues: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE links SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("move links: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET project_id = ?, project_name = ? WHERE project_id = ?`,
		target.ID, target.Name, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("move events: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE purge_log SET project_id = ?, project_name = ? WHERE project_id = ?`,
		target.ID, target.Name, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("move purge log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("move import mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_aliases SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("move aliases: %w", err)
	}

	if p.TargetName != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET name = ? WHERE id = ?`,
			*p.TargetName, target.ID); err != nil {
			return ProjectMergeResult{}, fmt.Errorf("update target project: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, source.ID); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("delete source project: %w", err)
	}

	mergedTarget, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, target.ID))
	if err != nil {
		return ProjectMergeResult{}, fmt.Errorf("reload target project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ProjectMergeResult{}, fmt.Errorf("commit merge projects: %w", err)
	}

	return ProjectMergeResult{
		Source:            source,
		Target:            mergedTarget,
		IssuesMoved:       issuesMoved,
		AliasesMoved:      aliasesMoved,
		EventsMoved:       eventsMoved,
		PurgeLogsMoved:    purgeLogsMoved,
		ShortIDExtensions: extensions,
	}, nil
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
) ([]ShortIDExtension, error) {
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

	var extensions []ShortIDExtension
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
		extensions = append(extensions, ShortIDExtension{
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
) ([]ProjectMergeImportMappingCollision, error) {
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
	var out []ProjectMergeImportMappingCollision
	for rows.Next() {
		var c ProjectMergeImportMappingCollision
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
