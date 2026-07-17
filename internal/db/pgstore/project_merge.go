package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

// MergeProjects folds all supported source projections into a surviving
// target after fencing namespaces and rejecting ambiguous external bindings.
func (s *Store) MergeProjects(ctx context.Context, params db.MergeProjectsParams) (db.ProjectMergeResult, error) {
	if params.SourceProjectID == params.TargetProjectID {
		return db.ProjectMergeResult{}, db.ErrProjectMergeSameProject
	}
	var result db.ProjectMergeResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		firstID, secondID := params.SourceProjectID, params.TargetProjectID
		if firstID > secondID {
			firstID, secondID = secondID, firstID
		}
		first, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR UPDATE`, firstID))
		if err != nil {
			return err
		}
		second, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR UPDATE`, secondID))
		if err != nil {
			return err
		}
		source, target := first, second
		if source.ID != params.SourceProjectID {
			source, target = target, source
		}
		if source.Name == db.SystemProjectName || source.UID == db.SystemProjectUID ||
			target.Name == db.SystemProjectName || target.UID == db.SystemProjectUID {
			return db.ErrNotFound
		}
		if source.DeletedAt != nil {
			return db.ErrProjectMergeArchivedSource
		}
		if target.DeletedAt != nil {
			return db.ErrProjectMergeArchivedTarget
		}
		for _, projectID := range []int64{firstID, secondID} {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, projectID); err != nil {
				return mapSQLError(err, nil)
			}
		}
		if err := rejectProjectMergeBindingsTx(ctx, tx, source.ID, target.ID); err != nil {
			return err
		}
		collisions, err := projectMergeMappingCollisionsTx(ctx, tx, source.ID, target.ID)
		if err != nil {
			return err
		}
		if len(collisions) > 0 {
			return &db.ProjectMergeImportMappingCollisionError{Mappings: collisions}
		}
		extensions, err := extendMergeShortIDsTx(ctx, tx, source.ID, target.ID)
		if err != nil {
			return err
		}
		issuesMoved, err := countMergeRowsTx(ctx, tx, "issues", source.ID)
		if err != nil {
			return err
		}
		aliasesMoved, err := countMergeRowsTx(ctx, tx, "project_aliases", source.ID)
		if err != nil {
			return err
		}
		eventsMoved, err := countMergeRowsTx(ctx, tx, "events", source.ID)
		if err != nil {
			return err
		}
		purgeLogsMoved, err := countMergeRowsTx(ctx, tx, "purge_log", source.ID)
		if err != nil {
			return err
		}
		statements := []struct {
			query string
			args  []any
		}{
			{`UPDATE recurrences SET project_id = $1 WHERE project_id = $2`, []any{target.ID, source.ID}},
			{`UPDATE issues SET project_id = $1 WHERE project_id = $2`, []any{target.ID, source.ID}},
			{`UPDATE issue_claims SET project_id = $1 WHERE project_id = $2`, []any{target.ID, source.ID}},
			{`UPDATE pending_claim_requests SET project_id = $1 WHERE project_id = $2`, []any{target.ID, source.ID}},
			{`UPDATE purge_log SET project_id = $1, project_uid = $2, project_name = $3 WHERE project_id = $4`, []any{target.ID, target.UID, target.Name, source.ID}},
			{`UPDATE import_mappings SET project_id = $1 WHERE project_id = $2`, []any{target.ID, source.ID}},
			{`UPDATE project_aliases SET project_id = $1 WHERE project_id = $2`, []any{target.ID, source.ID}},
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
				return mapSQLError(err, nil)
			}
		}
		rows, err := tx.QueryContext(ctx,
			`UPDATE events SET project_id = $1, project_name = $2 WHERE project_id = $3 RETURNING id`,
			target.ID, target.Name, source.ID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		movedEventIDs, err := collectEventIDs(rows)
		if err != nil {
			return err
		}
		if err := recomputeEventContentHashesTx(ctx, tx, movedEventIDs); err != nil {
			return err
		}
		if params.TargetName != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET name = $1 WHERE id = $2`, *params.TargetName, target.ID); err != nil {
				return mapSQLError(err, nil)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = $1`, source.ID); err != nil {
			return mapSQLError(err, nil)
		}
		mergedTarget, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, target.ID))
		if err != nil {
			return err
		}
		result = db.ProjectMergeResult{
			Source: source, Target: mergedTarget,
			IssuesMoved: issuesMoved, AliasesMoved: aliasesMoved,
			EventsMoved: eventsMoved, PurgeLogsMoved: purgeLogsMoved,
			ShortIDExtensions: extensions,
		}
		return nil
	})
	return result, err
}

func rejectProjectMergeBindingsTx(ctx context.Context, tx *sql.Tx, sourceID, targetID int64) error {
	var count int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM federation_bindings WHERE project_id IN ($1,$2)`, sourceID, targetID).Scan(&count); err != nil {
		return mapSQLError(err, nil)
	}
	if count > 0 {
		return db.ErrProjectMergeFederationBinding
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issue_sync_bindings WHERE project_id IN ($1,$2)`, sourceID, targetID).Scan(&count); err != nil {
		return mapSQLError(err, nil)
	}
	if count > 0 {
		return db.ErrProjectMergeIssueSyncBinding
	}
	return nil
}

func projectMergeMappingCollisionsTx(
	ctx context.Context,
	tx *sql.Tx,
	sourceID int64,
	targetID int64,
) ([]db.ProjectMergeImportMappingCollision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT source.source, source.external_id, source.object_type
FROM import_mappings source
JOIN import_mappings target
  ON target.project_id = $1
 AND target.source = source.source
 AND target.external_id = source.external_id
 AND target.object_type = source.object_type
WHERE source.project_id = $2
ORDER BY source.source, source.object_type, source.external_id
LIMIT 20`, targetID, sourceID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var collisions []db.ProjectMergeImportMappingCollision
	for rows.Next() {
		var collision db.ProjectMergeImportMappingCollision
		if err := rows.Scan(&collision.Source, &collision.ExternalID, &collision.ObjectType); err != nil {
			return nil, mapSQLError(err, nil)
		}
		collisions = append(collisions, collision)
	}
	return collisions, mapSQLError(rows.Err(), nil)
}

func extendMergeShortIDsTx(ctx context.Context, tx *sql.Tx, sourceID, targetID int64) ([]db.ShortIDExtension, error) {
	rows, err := tx.QueryContext(ctx, `SELECT source.id, source.uid, source.short_id
FROM issues source
WHERE source.project_id = $1
  AND (
    EXISTS(SELECT 1 FROM issues target WHERE target.project_id = $2 AND target.short_id = source.short_id)
    OR EXISTS(SELECT 1 FROM purge_log target WHERE target.project_id = $2 AND target.short_id = source.short_id)
  )
ORDER BY source.uid ASC`, sourceID, targetID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	type collider struct {
		id      int64
		uid     string
		shortID string
	}
	var colliders []collider
	for rows.Next() {
		var value collider
		if err := rows.Scan(&value.id, &value.uid, &value.shortID); err != nil {
			_ = rows.Close()
			return nil, mapSQLError(err, nil)
		}
		colliders = append(colliders, value)
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	extensions := make([]db.ShortIDExtension, 0, len(colliders))
	for _, value := range colliders {
		var replacement string
		for length := len(value.shortID) + 1; length <= shortid.MaxLength; length++ {
			candidate, err := shortid.Derive(value.uid, length)
			if err != nil {
				return nil, err
			}
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM issues WHERE project_id IN ($1,$2) AND short_id = $3
  UNION ALL SELECT 1 FROM purge_log WHERE project_id IN ($1,$2) AND short_id = $3
)`, sourceID, targetID, candidate).Scan(&exists); err != nil {
				return nil, mapSQLError(err, nil)
			}
			if !exists {
				replacement = candidate
				break
			}
		}
		if replacement == "" {
			return nil, fmt.Errorf("short_id auto-extend exhausted for uid %s", value.uid)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET short_id = $1 WHERE id = $2`, replacement, value.id); err != nil {
			return nil, mapSQLError(err, nil)
		}
		extensions = append(extensions, db.ShortIDExtension{
			UID: value.uid, PreMergeShortID: value.shortID, PostMergeShortID: replacement,
		})
	}
	return extensions, nil
}

func countMergeRowsTx(ctx context.Context, tx *sql.Tx, table string, projectID int64) (int64, error) {
	var query string
	switch table {
	case "issues":
		query = `SELECT COUNT(*) FROM issues WHERE project_id = $1`
	case "project_aliases":
		query = `SELECT COUNT(*) FROM project_aliases WHERE project_id = $1`
	case "events":
		query = `SELECT COUNT(*) FROM events WHERE project_id = $1`
	case "purge_log":
		query = `SELECT COUNT(*) FROM purge_log WHERE project_id = $1`
	default:
		return 0, fmt.Errorf("unsupported merge count table %q", table)
	}
	var count int64
	if err := tx.QueryRowContext(ctx, query, projectID).Scan(&count); err != nil {
		return 0, mapSQLError(err, nil)
	}
	return count, nil
}
