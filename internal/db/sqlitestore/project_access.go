package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Policy mutations and authorization fences serialize through the same epoch
// row. Taking this lock before reading memberships closes the revoke/write gap.
func lockProjectAccess(ctx context.Context, tx db.Transaction) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET value=value WHERE key='project_access_revision'`)
	return err
}

func bumpProjectAccess(ctx context.Context, tx db.Transaction) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET value=CAST(CAST(value AS BIGINT)+1 AS TEXT) WHERE key='project_access_revision'`)
	return err
}

func (d *Store) withProjectAccessTx(ctx context.Context, op func(*sql.Tx) error) error {
	return d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		if err := op(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (d *Store) projectAccessAudit(ctx context.Context, tx *sql.Tx, kind, actor string, payload any) (db.Event, error) {
	system, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE uid=?`, db.SystemProjectUID))
	if err != nil {
		return db.Event{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return db.Event{}, err
	}
	return d.insertEventTx(ctx, tx, eventInsert{ProjectID: system.ID, ProjectUID: system.UID, ProjectName: system.Name, Type: kind, Actor: actor, Payload: string(body)})
}

func validateProjectAccessAdmin(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("admin actor is required")
	}
	return nil
}

func scanTeam(row rowScanner) (db.Team, error) {
	var team db.Team
	err := row.Scan(&team.UID, &team.Name, &team.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Team{}, db.ErrNotFound
	}
	return team, err
}

// CreateTeam creates a hub-local team and its attributed catalog event.
func (d *Store) CreateTeam(ctx context.Context, name, adminActor string) (db.Team, db.Event, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\r\n\x00") {
		return db.Team{}, db.Event{}, errors.New("invalid team name")
	}
	if err := validateProjectAccessAdmin(adminActor); err != nil {
		return db.Team{}, db.Event{}, err
	}
	teamUID, err := uid.New()
	if err != nil {
		return db.Team{}, db.Event{}, err
	}
	team := db.Team{UID: teamUID, Name: name, Revision: 1}
	var event db.Event
	err = d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		result, createErr := tx.ExecContext(ctx, `INSERT INTO teams(uid,name,revision) VALUES(?,?,1) ON CONFLICT(name) DO NOTHING`, team.UID, team.Name)
		if createErr != nil {
			return createErr
		}
		rows, createErr := result.RowsAffected()
		if createErr != nil {
			return createErr
		}
		if rows == 0 {
			return db.ErrTeamNameExists
		}
		if err := bumpProjectAccess(ctx, tx); err != nil {
			return err
		}
		var err error
		event, err = d.projectAccessAudit(ctx, tx, "team.created", adminActor, team)
		return err
	})
	return team, event, err
}

// TeamByUID reads a hub-local team by its immutable UID.
func (d *Store) TeamByUID(ctx context.Context, teamUID string) (db.Team, error) {
	return scanTeam(d.QueryRowContext(ctx, `SELECT uid,name,revision FROM teams WHERE uid=?`, teamUID))
}

// ListTeams lists hub-local teams in stable order.
func (d *Store) ListTeams(ctx context.Context) ([]db.Team, error) {
	rows, err := d.QueryContext(ctx, `SELECT uid,name,revision FROM teams ORDER BY name,uid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	teams := make([]db.Team, 0)
	for rows.Next() {
		team, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	return teams, rows.Err()
}

// TeamMembers lists the canonical actors enrolled in a team.
func (d *Store) TeamMembers(ctx context.Context, teamUID string) ([]string, error) {
	if _, err := d.TeamByUID(ctx, teamUID); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT actor FROM team_memberships WHERE team_uid=? ORDER BY actor`, teamUID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	members := make([]string, 0)
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			return nil, err
		}
		members = append(members, actor)
	}
	return members, rows.Err()
}

// SetTeamMembership updates membership and the policy revision in one transaction.
func (d *Store) SetTeamMembership(ctx context.Context, teamUID, actor string, present bool, adminActor string) (db.Event, error) {
	if err := db.ValidateTokenActor(actor); err != nil {
		return db.Event{}, err
	}
	if err := validateProjectAccessAdmin(adminActor); err != nil {
		return db.Event{}, err
	}
	actor = strings.TrimSpace(actor)
	var event db.Event
	err := d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		event = db.Event{}
		if _, err := scanTeam(tx.QueryRowContext(ctx, `SELECT uid,name,revision FROM teams WHERE uid=?`, teamUID)); err != nil {
			return err
		}
		var result sql.Result
		var err error
		if present {
			result, err = tx.ExecContext(ctx, `INSERT INTO team_memberships(team_uid,actor) VALUES(?,?) ON CONFLICT DO NOTHING`, teamUID, actor)
		} else {
			result, err = tx.ExecContext(ctx, `DELETE FROM team_memberships WHERE team_uid=? AND actor=?`, teamUID, actor)
		}
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE teams SET revision=revision+1 WHERE uid=?`, teamUID); err != nil {
			return err
		}
		if err := bumpProjectAccess(ctx, tx); err != nil {
			return err
		}
		event, err = d.projectAccessAudit(ctx, tx, "team.membership_changed", adminActor, map[string]any{"team_uid": teamUID, "actor": actor, "present": present})
		return err
	})
	return event, err
}

// DeleteTeam removes a team while preserving restricted project visibility.
func (d *Store) DeleteTeam(ctx context.Context, teamUID, adminActor string) (db.Event, error) {
	if err := validateProjectAccessAdmin(adminActor); err != nil {
		return db.Event{}, err
	}
	var event db.Event
	err := d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanTeam(tx.QueryRowContext(ctx, `SELECT uid,name,revision FROM teams WHERE uid=?`, teamUID)); err != nil {
			return err
		}
		// Deleting the last team must retain the teams visibility, and invalidate
		// readers that admitted the previous policy revision.
		if _, err := tx.ExecContext(ctx, `UPDATE project_access_policies SET revision=revision+1 WHERE project_uid IN (SELECT project_uid FROM project_access_teams WHERE team_uid=?)`, teamUID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM teams WHERE uid=?`, teamUID); err != nil {
			return err
		}
		if err := bumpProjectAccess(ctx, tx); err != nil {
			return err
		}
		var err error
		event, err = d.projectAccessAudit(ctx, tx, "team.deleted", adminActor, map[string]string{"team_uid": teamUID})
		return err
	})
	return event, err
}

// MigrateTeamActor moves memberships between canonical actor identities transactionally.
func (d *Store) MigrateTeamActor(ctx context.Context, fromActor, toActor, adminActor string) (db.Event, error) {
	if err := db.ValidateTokenActor(fromActor); err != nil {
		return db.Event{}, err
	}
	if err := db.ValidateTokenActor(toActor); err != nil {
		return db.Event{}, err
	}
	if err := validateProjectAccessAdmin(adminActor); err != nil {
		return db.Event{}, err
	}
	fromActor = strings.TrimSpace(fromActor)
	toActor = strings.TrimSpace(toActor)
	if fromActor == toActor {
		return db.Event{}, nil
	}
	var event db.Event
	err := d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		event = db.Event{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_memberships(team_uid,actor) SELECT team_uid,? FROM team_memberships WHERE actor=? ON CONFLICT DO NOTHING`, toActor, fromActor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE teams SET revision=revision+1 WHERE uid IN (SELECT team_uid FROM team_memberships WHERE actor=?)`, fromActor); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM team_memberships WHERE actor=?`, fromActor)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return nil
		}
		if err := bumpProjectAccess(ctx, tx); err != nil {
			return err
		}
		event, err = d.projectAccessAudit(ctx, tx, "team.actor_migrated", adminActor, map[string]string{"from_actor": fromActor, "to_actor": toActor})
		return err
	})
	return event, err
}

func readProjectAccessPolicy(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) (db.ProjectAccessPolicy, error) {
	policy := db.ProjectAccessPolicy{ProjectUID: projectUID, TeamUIDs: []string{}}
	err := q.QueryRowContext(ctx, `SELECT COALESCE(a.visibility,'all'),COALESCE(a.revision,1) FROM projects p LEFT JOIN project_access_policies a ON a.project_uid=p.uid WHERE p.uid=? AND p.deleted_at IS NULL AND p.uid<>?`, projectUID, db.SystemProjectUID).Scan(&policy.Visibility, &policy.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ProjectAccessPolicy{}, db.ErrNotFound
	}
	if err != nil {
		return db.ProjectAccessPolicy{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT team_uid FROM project_access_teams WHERE project_uid=? ORDER BY team_uid`, projectUID)
	if err != nil {
		return db.ProjectAccessPolicy{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var teamUID string
		if err := rows.Scan(&teamUID); err != nil {
			return db.ProjectAccessPolicy{}, err
		}
		policy.TeamUIDs = append(policy.TeamUIDs, teamUID)
	}
	return policy, rows.Err()
}

// ProjectAccessPolicy reads a project’s visibility, team UIDs and policy revision.
func (d *Store) ProjectAccessPolicy(ctx context.Context, projectUID string) (db.ProjectAccessPolicy, error) {
	return readProjectAccessPolicy(ctx, d.readQ, projectUID)
}

// SetProjectAccessPolicy updates project visibility with a revision check and attributed event.
func (d *Store) SetProjectAccessPolicy(ctx context.Context, policy db.ProjectAccessPolicy, adminActor string) (db.ProjectAccessPolicy, db.Event, error) {
	if err := validateProjectAccessAdmin(adminActor); err != nil {
		return db.ProjectAccessPolicy{}, db.Event{}, err
	}
	if policy.Visibility != "all" && policy.Visibility != "teams" {
		return db.ProjectAccessPolicy{}, db.Event{}, errors.New("invalid project visibility")
	}
	if policy.Visibility == "all" && len(policy.TeamUIDs) > 0 {
		return db.ProjectAccessPolicy{}, db.Event{}, errors.New("all visibility cannot contain teams")
	}
	policy.TeamUIDs = slices.Clone(policy.TeamUIDs)
	slices.Sort(policy.TeamUIDs)
	for i, teamUID := range policy.TeamUIDs {
		if !uid.Valid(teamUID) || (i > 0 && policy.TeamUIDs[i-1] == teamUID) {
			return db.ProjectAccessPolicy{}, db.Event{}, errors.New("invalid or duplicate team UID")
		}
	}
	var result db.ProjectAccessPolicy
	var event db.Event
	err := d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		current, err := readProjectAccessPolicy(ctx, tx, policy.ProjectUID)
		if err != nil {
			return err
		}
		if policy.Revision != 0 && policy.Revision != current.Revision {
			return db.ErrProjectAccessRevisionConflict
		}
		for _, teamUID := range policy.TeamUIDs {
			if _, err := scanTeam(tx.QueryRowContext(ctx, `SELECT uid,name,revision FROM teams WHERE uid=?`, teamUID)); err != nil {
				return err
			}
		}
		result = policy
		result.Revision = current.Revision + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_access_policies(project_uid,visibility,revision) VALUES(?,?,?) ON CONFLICT(project_uid) DO UPDATE SET visibility=excluded.visibility,revision=excluded.revision`, result.ProjectUID, result.Visibility, result.Revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_access_teams WHERE project_uid=?`, result.ProjectUID); err != nil {
			return err
		}
		for _, teamUID := range result.TeamUIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_access_teams(project_uid,team_uid) VALUES(?,?)`, result.ProjectUID, teamUID); err != nil {
				return err
			}
		}
		if err := bumpProjectAccess(ctx, tx); err != nil {
			return err
		}
		event, err = d.projectAccessAudit(ctx, tx, "project.access_changed", adminActor, result)
		return err
	})
	return result, event, err
}

// AccessibleProjectUIDs applies project policy before a caller paginates or
// ranks. Other token, host and operation restrictions still need intersection.
func (d *Store) AccessibleProjectUIDs(ctx context.Context, actor string) ([]string, error) {
	if actor == "" {
		return []string{}, nil
	}
	if err := db.ValidateTokenActor(actor); err != nil {
		return nil, err
	}
	rows, err := d.readQ.QueryContext(ctx, `SELECT p.uid FROM projects p LEFT JOIN project_access_policies a ON a.project_uid=p.uid WHERE p.deleted_at IS NULL AND p.uid<>? AND (COALESCE(a.visibility,'all')='all' OR EXISTS(SELECT 1 FROM project_access_teams pat JOIN team_memberships tm ON tm.team_uid=pat.team_uid WHERE pat.project_uid=p.uid AND tm.actor=?)) ORDER BY p.uid`, db.SystemProjectUID, strings.TrimSpace(actor))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0)
	for rows.Next() {
		var projectUID string
		if err := rows.Scan(&projectUID); err != nil {
			return nil, err
		}
		result = append(result, projectUID)
	}
	return result, rows.Err()
}

// AnonymousAccessibleProjectUIDs returns the active projects visible without
// an actor identity. Anonymous access never inherits team membership.
func (d *Store) AnonymousAccessibleProjectUIDs(ctx context.Context) ([]string, error) {
	args := []any{db.SystemProjectUID}
	query := `SELECT p.uid FROM projects p LEFT JOIN project_access_policies a ON a.project_uid=p.uid WHERE p.deleted_at IS NULL AND p.uid<>? AND COALESCE(a.visibility,'all')='all' AND ` + authorizedProjectPredicate(ctx, "p.uid", &args) + ` ORDER BY p.uid`
	rows, err := d.readQ.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0)
	for rows.Next() {
		var projectUID string
		if err := rows.Scan(&projectUID); err != nil {
			return nil, err
		}
		result = append(result, projectUID)
	}
	return result, rows.Err()
}

// ProjectAccessRevision reads the hub-local policy revision used to fence cached authority.
func (d *Store) ProjectAccessRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := d.QueryRowContext(ctx, `SELECT CAST(value AS BIGINT) FROM meta WHERE key='project_access_revision'`).Scan(&revision)
	return revision, err
}

// ProjectAccessTransactionFence rechecks actor access while holding the policy lock through commit.
func (d *Store) ProjectAccessTransactionFence(actor string, projectUIDs []string) db.TransactionFence {
	projectUIDs = slices.Clone(projectUIDs)
	return func(ctx context.Context, tx db.Transaction) error {
		if actor == "" && !db.UnrestrictedProjectWrites(ctx) {
			return db.ErrNotFound
		}
		if actor != "" && db.ValidateTokenActor(actor) != nil {
			return db.ErrNotFound
		}
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		for _, projectUID := range projectUIDs {
			var allowed int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects p LEFT JOIN project_access_policies a ON a.project_uid=p.uid WHERE p.uid=? AND p.uid<>? AND p.deleted_at IS NULL AND (COALESCE(a.visibility,'all')='all' OR EXISTS(SELECT 1 FROM project_access_teams pat JOIN team_memberships tm ON tm.team_uid=pat.team_uid WHERE pat.project_uid=p.uid AND tm.actor=? AND tm.actor<>''))`, projectUID, db.SystemProjectUID, strings.TrimSpace(actor)).Scan(&allowed)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("project access fence: %w", err)
			}
		}
		return nil
	}
}

var _ db.ProjectAccessStorage = (*Store)(nil)

// ExportProjectAccess emits owning-hub policy only for complete owner backups.
func (d *Store) ExportProjectAccess(ctx context.Context) iter.Seq2[db.ImportRecord, error] {
	return func(yield func(db.ImportRecord, error) bool) {
		for _, query := range []string{
			`SELECT uid,name,revision FROM teams ORDER BY uid`,
			`SELECT team_uid,actor FROM team_memberships ORDER BY team_uid,actor`,
			`SELECT p.project_uid,p.visibility,p.revision,t.team_uid FROM project_access_policies p LEFT JOIN project_access_teams t ON t.project_uid=p.project_uid ORDER BY p.project_uid,t.team_uid`,
		} {
			rows, err := d.readQ.QueryContext(ctx, query)
			if err != nil {
				yield(nil, err)
				return
			}
			var policy *db.ProjectAccessPolicy
			keepGoing := true
			for rows.Next() {
				switch query {
				case `SELECT uid,name,revision FROM teams ORDER BY uid`:
					team, err := scanTeam(rows)
					if err != nil {
						yield(nil, err)
						_ = rows.Close()
						return
					}
					keepGoing = yield(&team, nil)
				case `SELECT team_uid,actor FROM team_memberships ORDER BY team_uid,actor`:
					var membership db.TeamMembership
					if err := rows.Scan(&membership.TeamUID, &membership.Actor); err != nil {
						yield(nil, err)
						_ = rows.Close()
						return
					}
					keepGoing = yield(&membership, nil)
				default:
					var current db.ProjectAccessPolicy
					var teamUID sql.NullString
					if err := rows.Scan(&current.ProjectUID, &current.Visibility, &current.Revision, &teamUID); err != nil {
						yield(nil, err)
						_ = rows.Close()
						return
					}
					if policy != nil && policy.ProjectUID != current.ProjectUID {
						if !yield(policy, nil) {
							_ = rows.Close()
							return
						}
						policy = nil
					}
					if policy == nil {
						current.TeamUIDs = []string{}
						policy = &current
					}
					if teamUID.Valid {
						policy.TeamUIDs = append(policy.TeamUIDs, teamUID.String)
					}
				}
				if !keepGoing {
					_ = rows.Close()
					return
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				yield(nil, err)
				return
			}
			if policy != nil && !yield(policy, nil) {
				return
			}
		}
	}
}
