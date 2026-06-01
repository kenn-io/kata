package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// UpsertImportMapping inserts or updates a source identity mapping.
func (d *Store) UpsertImportMapping(ctx context.Context, p db.ImportMappingParams) (db.ImportMapping, error) {
	return upsertImportMapping(ctx, d.DB, p)
}

func upsertImportMapping(ctx context.Context, e execQuerier, p db.ImportMappingParams) (db.ImportMapping, error) {
	_, err := e.ExecContext(ctx, `INSERT INTO import_mappings(
		source, external_id, object_type, project_id, issue_id, comment_id, link_id, label, source_updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(source, external_id, object_type, project_id) DO UPDATE SET
		issue_id=excluded.issue_id,
		comment_id=excluded.comment_id,
		link_id=excluded.link_id,
		label=excluded.label,
		source_updated_at=excluded.source_updated_at,
		imported_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		p.Source, p.ExternalID, p.ObjectType, p.ProjectID, p.IssueID, p.CommentID, p.LinkID, p.Label, p.SourceUpdatedAt)
	if err != nil {
		return db.ImportMapping{}, fmt.Errorf("upsert import mapping: %w", err)
	}
	return importMappingBySource(ctx, e, p.ProjectID, p.Source, p.ObjectType, p.ExternalID)
}

// ImportMappingBySource fetches one mapping by source identity.
func (d *Store) ImportMappingBySource(ctx context.Context, projectID int64, source, objectType, externalID string) (db.ImportMapping, error) {
	return importMappingBySource(ctx, d.DB, projectID, source, objectType, externalID)
}

func importMappingBySource(ctx context.Context, q queryer, projectID int64, source, objectType, externalID string) (db.ImportMapping, error) {
	row := q.QueryRowContext(ctx, importMappingSelect+` WHERE project_id = ? AND source = ? AND object_type = ? AND external_id = ?`,
		projectID, source, objectType, externalID)
	return scanImportMapping(row)
}

// ImportMappingsByProjectSource returns every mapping for a project/source pair.
func (d *Store) ImportMappingsByProjectSource(ctx context.Context, projectID int64, source string) ([]db.ImportMapping, error) {
	rows, err := d.QueryContext(ctx, importMappingSelect+` WHERE project_id = ? AND source = ? ORDER BY id ASC`, projectID, source)
	if err != nil {
		return nil, fmt.Errorf("list import mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.ImportMapping
	for rows.Next() {
		m, err := scanImportMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const importMappingSelect = `SELECT id, source, external_id, object_type, project_id, issue_id, comment_id, link_id, label, source_updated_at, imported_at FROM import_mappings`

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execQuerier interface {
	queryer
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func scanImportMapping(r rowScanner) (db.ImportMapping, error) {
	var m db.ImportMapping
	var issueID, commentID, linkID sql.NullInt64
	var label sql.NullString
	var sourceUpdated sql.NullTime
	err := r.Scan(&m.ID, &m.Source, &m.ExternalID, &m.ObjectType, &m.ProjectID,
		&issueID, &commentID, &linkID, &label, &sourceUpdated, &m.ImportedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ImportMapping{}, db.ErrNotFound
	}
	if err != nil {
		return db.ImportMapping{}, fmt.Errorf("scan import mapping: %w", err)
	}
	if issueID.Valid {
		m.IssueID = &issueID.Int64
	}
	if commentID.Valid {
		m.CommentID = &commentID.Int64
	}
	if linkID.Valid {
		m.LinkID = &linkID.Int64
	}
	if label.Valid {
		m.Label = &label.String
	}
	if sourceUpdated.Valid {
		m.SourceUpdatedAt = &sourceUpdated.Time
	}
	return m, nil
}
