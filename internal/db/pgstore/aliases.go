package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

const aliasSelect = `SELECT id, project_id, alias_identity, alias_kind, created_at FROM project_aliases`

// AttachAlias creates a stable workspace identity for a project.
func (s *Store) AttachAlias(ctx context.Context, projectID int64, identity, kind string) (db.ProjectAlias, error) {
	var alias db.ProjectAlias
	err := s.RetryTransient(ctx, func() error {
		var err error
		alias, err = scanAlias(s.QueryRowContext(ctx,
			`INSERT INTO project_aliases(project_id, alias_identity, alias_kind) VALUES($1, $2, $3) `+
				`RETURNING id, project_id, alias_identity, alias_kind, created_at`,
			projectID, identity, kind))
		return mapSQLError(err, map[string]error{
			"project_aliases_alias_identity_key": db.ErrAliasExists,
		})
	})
	return alias, err
}

// AliasByIdentity returns one alias by its unique external identity.
func (s *Store) AliasByIdentity(ctx context.Context, identity string) (db.ProjectAlias, error) {
	return scanAlias(s.QueryRowContext(ctx, aliasSelect+` WHERE alias_identity = $1`, identity))
}

// AliasByID returns one alias by row identity.
func (s *Store) AliasByID(ctx context.Context, id int64) (db.ProjectAlias, error) {
	return scanAlias(s.QueryRowContext(ctx, aliasSelect+` WHERE id = $1`, id))
}

// ReassignAlias moves an alias to another project without changing identity.
func (s *Store) ReassignAlias(ctx context.Context, aliasID, projectID int64) error {
	return s.RetryTransient(ctx, func() error {
		_, err := s.ExecContext(ctx,
			`UPDATE project_aliases SET project_id = $1 WHERE id = $2`, projectID, aliasID)
		return mapSQLError(err, nil)
	})
}

// ProjectAliases returns a project's aliases in creation order.
func (s *Store) ProjectAliases(ctx context.Context, projectID int64) ([]db.ProjectAlias, error) {
	rows, err := s.QueryContext(ctx, aliasSelect+` WHERE project_id = $1 ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var aliases []db.ProjectAlias
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	return aliases, nil
}

// LatestAliasForProject returns the most recently created alias, if any.
func (s *Store) LatestAliasForProject(ctx context.Context, projectID int64) (db.AliasRow, bool, error) {
	var alias db.AliasRow
	err := s.QueryRowContext(ctx,
		`SELECT alias_identity, alias_kind FROM project_aliases WHERE project_id = $1 ORDER BY id DESC LIMIT 1`,
		projectID).Scan(&alias.Identity, &alias.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AliasRow{}, false, nil
	}
	if err != nil {
		return db.AliasRow{}, false, mapSQLError(err, nil)
	}
	return alias, true, nil
}

// HardDeleteProject removes a project row for failed initialization cleanup.
func (s *Store) HardDeleteProject(ctx context.Context, id int64) error {
	return s.RetryTransient(ctx, func() error {
		_, err := s.ExecContext(ctx, `DELETE FROM projects WHERE id = $1`, id)
		return mapSQLError(err, nil)
	})
}

func scanAlias(row rowScanner) (db.ProjectAlias, error) {
	var alias db.ProjectAlias
	var createdAt string
	err := row.Scan(&alias.ID, &alias.ProjectID, &alias.AliasIdentity, &alias.AliasKind, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ProjectAlias{}, db.ErrNotFound
	}
	if err != nil {
		return db.ProjectAlias{}, mapSQLError(err, nil)
	}
	alias.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.ProjectAlias{}, err
	}
	return alias, nil
}
