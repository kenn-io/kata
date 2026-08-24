package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

const importMappingSelect = `SELECT id, source, external_id, object_type, project_id,
       issue_id, comment_id, link_id, label, source_updated_at, imported_at
  FROM import_mappings`

// UpsertImportMapping records the current local projection for one external
// object identity while preserving the mapping row's stable ID.
func (s *Store) UpsertImportMapping(ctx context.Context, params db.ImportMappingParams) (db.ImportMapping, error) {
	var mapping db.ImportMapping
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		mapping, err = upsertImportMappingTx(ctx, tx, params)
		return err
	})
	return mapping, err
}

func upsertImportMappingTx(
	ctx context.Context,
	query rowQueryer,
	params db.ImportMappingParams,
) (db.ImportMapping, error) {
	if err := validateBindingScopedCommentMappingTx(ctx, query, params); err != nil {
		return db.ImportMapping{}, err
	}
	if params.ObjectType == "issue" {
		var boundIssueID int64
		err := query.QueryRowContext(ctx, `SELECT b.issue_id
  FROM external_root_bindings b
  JOIN import_mappings m ON m.id = b.root_mapping_id
 WHERE m.source = $1 AND m.external_id = $2 AND m.object_type = $3 AND m.project_id = $4
 LIMIT 1`, params.Source, params.ExternalID, params.ObjectType, params.ProjectID).Scan(&boundIssueID)
		if err == nil && (params.IssueID == nil || boundIssueID != *params.IssueID) {
			return db.ImportMapping{}, fmt.Errorf("%w: external root mapping cannot be retargeted", db.ErrExternalRootAlreadyBound)
		}
		if err == nil && (params.CommentID != nil || params.LinkID != nil || params.Label != nil) {
			return db.ImportMapping{}, fmt.Errorf("%w: external root mapping target must be its bound issue", db.ErrExternalRootValidation)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.ImportMapping{}, mapSQLError(err, nil)
		}
		if params.IssueID != nil {
			var bindingID int64
			err = query.QueryRowContext(ctx, `SELECT b.id
  FROM issue_sync_bindings s
  JOIN external_root_bindings b
    ON b.project_id = s.project_id AND b.issue_id = $1 AND b.active = 1
 WHERE s.project_id = $2 AND s.source_key = $3
 LIMIT 1`, *params.IssueID, params.ProjectID, params.Source).Scan(&bindingID)
			if err == nil {
				return db.ImportMapping{}, db.ErrExternalRootIssueSyncConflict
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return db.ImportMapping{}, mapSQLError(err, nil)
			}
		}
	}
	var sourceUpdatedAt any
	if params.SourceUpdatedAt != nil {
		sourceUpdatedAt = formatExternalObservationTime(*params.SourceUpdatedAt)
	}
	mapping, err := scanImportMapping(query.QueryRowContext(ctx, `INSERT INTO import_mappings(
  source, external_id, object_type, project_id,
  issue_id, comment_id, link_id, label, source_updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT(source, external_id, object_type, project_id) DO UPDATE SET
  issue_id=excluded.issue_id,
  comment_id=excluded.comment_id,
  link_id=excluded.link_id,
  label=excluded.label,
  source_updated_at=excluded.source_updated_at,
  imported_at=to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
RETURNING id, source, external_id, object_type, project_id,
          issue_id, comment_id, link_id, label, source_updated_at, imported_at`,
		params.Source, params.ExternalID, params.ObjectType, params.ProjectID,
		params.IssueID, params.CommentID, params.LinkID, params.Label, sourceUpdatedAt))
	return mapping, mapSQLError(err, nil)
}

func validateBindingScopedCommentMappingTx(
	ctx context.Context,
	query rowQueryer,
	params db.ImportMappingParams,
) error {
	if params.ObjectType != "comment" {
		return nil
	}
	var bindingIssueID int64
	err := query.QueryRowContext(ctx, `SELECT issue_id
  FROM external_root_bindings
 WHERE project_id = $1
   AND $2 = 'connector:' || connector_instance || ':binding:' || uid
 LIMIT 1`, params.ProjectID, params.Source).Scan(&bindingIssueID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	if params.IssueID == nil || params.CommentID == nil || *params.IssueID != bindingIssueID {
		return fmt.Errorf("%w: external comment mapping must target its binding issue", db.ErrExternalRootValidation)
	}
	var commentIssueID int64
	if err := query.QueryRowContext(ctx, `SELECT issue_id FROM comments WHERE id = $1`, *params.CommentID).Scan(&commentIssueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: external comment mapping must target a comment on its binding issue", db.ErrExternalRootValidation)
		}
		return mapSQLError(err, nil)
	}
	if commentIssueID != bindingIssueID {
		return fmt.Errorf("%w: external comment mapping must target a comment on its binding issue", db.ErrExternalRootValidation)
	}
	return nil
}

// ImportMappingBySource resolves one external identity in a project.
func (s *Store) ImportMappingBySource(
	ctx context.Context,
	projectID int64,
	source string,
	objectType string,
	externalID string,
) (db.ImportMapping, error) {
	return importMappingBySourceTx(ctx, s, projectID, source, objectType, externalID)
}

func importMappingBySourceTx(
	ctx context.Context,
	query rowQueryer,
	projectID int64,
	source string,
	objectType string,
	externalID string,
) (db.ImportMapping, error) {
	return scanImportMapping(query.QueryRowContext(ctx, importMappingSelect+`
WHERE project_id = $1 AND source = $2 AND object_type = $3 AND external_id = $4`,
		projectID, source, objectType, externalID))
}

func adoptImportMappingTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	source string,
	objectType string,
	externalID string,
	legacyExternalIDs []string,
) (db.ImportMapping, bool, error) {
	mapping, err := importMappingBySourceTx(ctx, tx, projectID, source, objectType, externalID)
	if err == nil {
		return mapping, true, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.ImportMapping{}, false, err
	}
	for _, legacy := range legacyExternalIDs {
		if legacy == "" || legacy == externalID {
			continue
		}
		legacyMapping, err := importMappingBySourceTx(ctx, tx, projectID, source, objectType, legacy)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return db.ImportMapping{}, false, err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM import_mappings WHERE id=$1 FOR UPDATE`,
			legacyMapping.ID,
		).Scan(&legacyMapping.ID); err != nil {
			return db.ImportMapping{}, false, mapSQLError(err, nil)
		}
		var bindingID int64
		bindingErr := tx.QueryRowContext(ctx,
			`SELECT id FROM external_root_bindings WHERE root_mapping_id=$1 LIMIT 1`,
			legacyMapping.ID,
		).Scan(&bindingID)
		if bindingErr == nil {
			return db.ImportMapping{}, false, fmt.Errorf(
				"%w: external root mapping identity cannot be changed", db.ErrExternalRootAlreadyBound,
			)
		}
		if !errors.Is(bindingErr, sql.ErrNoRows) {
			return db.ImportMapping{}, false, mapSQLError(bindingErr, nil)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE import_mappings SET external_id=$1 WHERE id=$2`, externalID, legacyMapping.ID); err != nil {
			return db.ImportMapping{}, false, fmt.Errorf("adopt legacy import mapping: %w", mapSQLError(err, nil))
		}
		legacyMapping.ExternalID = externalID
		return legacyMapping, true, nil
	}
	return db.ImportMapping{}, false, nil
}

// ImportMappingsByProjectSource returns stable insertion-order mappings for a
// project/source pair.
func (s *Store) ImportMappingsByProjectSource(
	ctx context.Context,
	projectID int64,
	source string,
) ([]db.ImportMapping, error) {
	rows, err := s.QueryContext(ctx, importMappingSelect+`
WHERE project_id = $1 AND source = $2 ORDER BY id ASC`, projectID, source)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var mappings []db.ImportMapping
	for rows.Next() {
		mapping, err := scanImportMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, mapSQLError(rows.Err(), nil)
}

// ImportCommentMappingsByIssue lists comment mappings for one issue.
func (s *Store) ImportCommentMappingsByIssue(ctx context.Context, issueID int64) ([]db.ImportMapping, error) {
	rows, err := s.QueryContext(ctx, importMappingSelect+`
WHERE issue_id = $1 AND object_type = 'comment' AND comment_id IS NOT NULL ORDER BY id ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var mappings []db.ImportMapping
	for rows.Next() {
		mapping, err := scanImportMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, mapSQLError(rows.Err(), nil)
}

func scanImportMapping(row rowScanner) (db.ImportMapping, error) {
	var mapping db.ImportMapping
	var issueID, commentID, linkID sql.NullInt64
	var label, sourceUpdatedAt sql.NullString
	var importedAt string
	err := row.Scan(
		&mapping.ID, &mapping.Source, &mapping.ExternalID, &mapping.ObjectType, &mapping.ProjectID,
		&issueID, &commentID, &linkID, &label, &sourceUpdatedAt, &importedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ImportMapping{}, db.ErrNotFound
	}
	if err != nil {
		return db.ImportMapping{}, mapSQLError(err, nil)
	}
	if issueID.Valid {
		mapping.IssueID = &issueID.Int64
	}
	if commentID.Valid {
		mapping.CommentID = &commentID.Int64
	}
	if linkID.Valid {
		mapping.LinkID = &linkID.Int64
	}
	if label.Valid {
		mapping.Label = &label.String
	}
	if sourceUpdatedAt.Valid {
		value, err := parseStoredTime(sourceUpdatedAt.String)
		if err != nil {
			return db.ImportMapping{}, fmt.Errorf("parse import source_updated_at: %w", err)
		}
		mapping.SourceUpdatedAt = &value
	}
	mapping.ImportedAt, err = parseStoredTime(importedAt)
	if err != nil {
		return db.ImportMapping{}, fmt.Errorf("parse import imported_at: %w", err)
	}
	return mapping, nil
}
