package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

const projectSelect = `SELECT id, uid, name, metadata, revision, created_at, deleted_at FROM projects`
const storedTimeFormat = "2006-01-02T15:04:05.000Z"
const externalObservationTimeFormat = "2006-01-02T15:04:05.000000000Z"

type rowScanner interface {
	Scan(...any) error
}

// CreateProject inserts a project with a new stable UID.
func (s *Store) CreateProject(ctx context.Context, name string) (db.Project, error) {
	project, _, err := s.CreateProjectAndEvent(ctx, name, db.SystemActor)
	return project, err
}

// CreateProjectAndEvent inserts a project and returns its committed event.
func (s *Store) CreateProjectAndEvent(ctx context.Context, name, actor string) (db.Project, db.Event, error) {
	if actor == "" {
		actor = db.SystemActor
	}
	if name == db.SystemProjectName {
		return db.Project{}, db.Event{}, fmt.Errorf("create project: reserved project name %q", name)
	}
	projectUID, err := katauid.New()
	if err != nil {
		return db.Project{}, db.Event{}, fmt.Errorf("generate project uid: %w", err)
	}
	return s.createProjectWithUID(ctx, name, projectUID, actor)
}

// CreateProjectWithUID inserts a project with a caller-supplied stable UID.
func (s *Store) CreateProjectWithUID(ctx context.Context, name, projectUID string) (db.Project, error) {
	project, _, err := s.CreateProjectWithUIDAndEvent(ctx, name, projectUID, db.SystemActor)
	return project, err
}

// CreateProjectWithUIDAndEvent inserts a stable-UID project and returns its committed event.
func (s *Store) CreateProjectWithUIDAndEvent(ctx context.Context, name, projectUID, actor string) (db.Project, db.Event, error) {
	if actor == "" {
		actor = db.SystemActor
	}
	return s.createProjectWithUID(ctx, name, projectUID, actor)
}

func (s *Store) createProjectWithUID(ctx context.Context, name, projectUID, actor string) (db.Project, db.Event, error) {
	if name == db.SystemProjectName {
		return db.Project{}, db.Event{}, fmt.Errorf("create project: reserved project name %q", name)
	}
	if projectUID == db.SystemProjectUID {
		return db.Project{}, db.Event{}, fmt.Errorf("create project: reserved project uid %q", projectUID)
	}
	if !katauid.Valid(projectUID) {
		return db.Project{}, db.Event{}, fmt.Errorf("invalid project uid %q", projectUID)
	}
	var (
		project db.Project
		event   db.Event
	)
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		project, err = scanProject(tx.QueryRowContext(ctx,
			`INSERT INTO projects(uid, name) VALUES($1, $2) `+
				`RETURNING id, uid, name, metadata, revision, created_at, deleted_at`,
			projectUID, name))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			Name string `json:"name"`
		}{Name: project.Name})
		if err != nil {
			return fmt.Errorf("marshal project created event: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.created", Actor: actor, Payload: string(payload),
		})
		return err
	})
	return project, event, err
}

// ProjectByID returns active or archived projects while hiding the system row.
func (s *Store) ProjectByID(ctx context.Context, id int64) (db.Project, error) {
	return visibleProject(scanProject(s.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, id)))
}

// ProjectByName returns only active projects.
func (s *Store) ProjectByName(ctx context.Context, name string) (db.Project, error) {
	return visibleProject(scanProject(s.QueryRowContext(ctx,
		projectSelect+` WHERE name = $1 AND deleted_at IS NULL`, name)))
}

// ProjectByNameIncludingArchived returns active or archived projects.
func (s *Store) ProjectByNameIncludingArchived(ctx context.Context, name string) (db.Project, error) {
	return visibleProject(scanProject(s.QueryRowContext(ctx, projectSelect+` WHERE name = $1`, name)))
}

// ProjectByUID returns active or archived projects by stable identity.
func (s *Store) ProjectByUID(ctx context.Context, uid string) (db.Project, error) {
	return visibleProject(scanProject(s.QueryRowContext(ctx, projectSelect+` WHERE uid = $1`, uid)))
}

// RenameProject changes only the canonical name.
func (s *Store) RenameProject(ctx context.Context, id int64, name string) (db.Project, error) {
	project, _, _, err := s.RenameProjectAndEvent(ctx, id, name, db.SystemActor)
	return project, err
}

// RenameProjectAndEvent renames a project and returns its committed event.
func (s *Store) RenameProjectAndEvent(ctx context.Context, id int64, name, actor string) (db.Project, *db.Event, bool, error) {
	if actor == "" {
		actor = db.SystemActor
	}
	if name == db.SystemProjectName {
		return db.Project{}, nil, false, fmt.Errorf("rename project: reserved project name %q", name)
	}
	var (
		project db.Project
		event   *db.Event
		changed bool
	)
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		current, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if current.Name == db.SystemProjectName || current.UID == db.SystemProjectUID {
			return db.ErrNotFound
		}
		if current.Name == name {
			project = current
			return nil
		}
		project, err = scanProject(tx.QueryRowContext(ctx,
			`UPDATE projects SET name = $1 WHERE id = $2 `+
				`RETURNING id, uid, name, metadata, revision, created_at, deleted_at`, name, id))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			From string `json:"from"`
			To   string `json:"to"`
		}{From: current.Name, To: project.Name})
		if err != nil {
			return fmt.Errorf("marshal project renamed event: %w", err)
		}
		inserted, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.renamed", Actor: actor, Payload: string(payload),
		})
		if err == nil {
			event = &inserted
			changed = true
		}
		return err
	})
	project, err = visibleProject(project, err)
	return project, event, changed, err
}

// ListProjects returns active projects in row identity order.
func (s *Store) ListProjects(ctx context.Context) ([]db.Project, error) {
	return s.listProjects(ctx, false)
}

// ListProjectsIncludingArchived returns active and archived projects.
func (s *Store) ListProjectsIncludingArchived(ctx context.Context) ([]db.Project, error) {
	return s.listProjects(ctx, true)
}

func (s *Store) listProjects(ctx context.Context, includeArchived bool) ([]db.Project, error) {
	query := projectSelect + ` WHERE name <> $1`
	if !includeArchived {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.QueryContext(ctx, query, db.SystemProjectName)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var projects []db.Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	return projects, nil
}

func visibleProject(project db.Project, err error) (db.Project, error) {
	if err != nil {
		return db.Project{}, err
	}
	if project.Name == db.SystemProjectName || project.UID == db.SystemProjectUID {
		return db.Project{}, db.ErrNotFound
	}
	return project, nil
}

func scanProject(row rowScanner) (db.Project, error) {
	var (
		project   db.Project
		metadata  string
		createdAt string
		deletedAt sql.NullString
	)
	err := row.Scan(
		&project.ID,
		&project.UID,
		&project.Name,
		&metadata,
		&project.Revision,
		&createdAt,
		&deletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Project{}, db.ErrNotFound
	}
	if err != nil {
		return db.Project{}, mapSQLError(err, nil)
	}
	project.Metadata = db.JSONBlob(metadata)
	project.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.Project{}, fmt.Errorf("parse project created_at: %w", err)
	}
	if deletedAt.Valid {
		value, err := parseStoredTime(deletedAt.String)
		if err != nil {
			return db.Project{}, fmt.Errorf("parse project deleted_at: %w", err)
		}
		project.DeletedAt = &value
	}
	return project, nil
}

func parseStoredTime(value string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid stored timestamp %q", value)
}

func formatStoredTime(value time.Time) string { return value.UTC().Format(storedTimeFormat) }

func formatExternalObservationTime(value time.Time) string {
	return value.UTC().Format(externalObservationTimeFormat)
}

func nowStoredTimestamp() string { return formatStoredTime(time.Now()) }
