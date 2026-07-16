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
	err := s.RetryTransient(ctx, func() error {
		var err error
		mapping, err = upsertImportMappingTx(ctx, s, params)
		return err
	})
	return mapping, err
}

func upsertImportMappingTx(
	ctx context.Context,
	query rowQueryer,
	params db.ImportMappingParams,
) (db.ImportMapping, error) {
	var sourceUpdatedAt any
	if params.SourceUpdatedAt != nil {
		sourceUpdatedAt = formatStoredTime(*params.SourceUpdatedAt)
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
