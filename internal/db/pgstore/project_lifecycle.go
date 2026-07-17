package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// CountOpenIssues returns the number of visible open issues in a project.
func (s *Store) CountOpenIssues(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues
      WHERE project_id = $1 AND status = 'open' AND deleted_at IS NULL`, projectID).Scan(&count)
	return count, mapSQLError(err, nil)
}

// RemoveProject archives a project, drops its aliases, and emits one event.
func (s *Store) RemoveProject(ctx context.Context, params db.RemoveProjectParams) (db.Project, *db.Event, error) {
	var project db.Project
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		project, err = scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR UPDATE`, params.ProjectID))
		if err != nil {
			return err
		}
		if project.Name == db.SystemProjectName || project.UID == db.SystemProjectUID {
			return db.ErrNotFound
		}
		if project.DeletedAt != nil {
			return db.ErrProjectAlreadyArchived
		}
		var openIssues, aliasCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues
          WHERE project_id = $1 AND status = 'open' AND deleted_at IS NULL`, project.ID).Scan(&openIssues); err != nil {
			return mapSQLError(err, nil)
		}
		if openIssues > 0 && !params.Force {
			return &db.ProjectHasOpenIssuesError{OpenIssues: openIssues}
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM project_aliases WHERE project_id = $1`, project.ID).Scan(&aliasCount); err != nil {
			return mapSQLError(err, nil)
		}
		archivedAt := nowStoredTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET deleted_at = $1 WHERE id = $2`, archivedAt, project.ID); err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings SET enabled = 0, updated_at = $1
          WHERE project_id = $2`, archivedAt, project.ID); err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_aliases WHERE project_id = $1`, project.ID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			AliasCount int64 `json:"alias_count"`
			OpenIssues int64 `json:"open_issues"`
			Force      bool  `json:"force,omitempty"`
		}{AliasCount: aliasCount, OpenIssues: openIssues, Force: params.Force})
		if err != nil {
			return err
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.removed", Actor: params.Actor, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		project, err = scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, project.ID))
		return err
	})
	if err != nil {
		return db.Project{}, nil, err
	}
	return project, &event, nil
}

// RestoreProject makes an archived project active again.
func (s *Store) RestoreProject(ctx context.Context, projectID int64, actor string) (db.Project, *db.Event, bool, error) {
	return s.restoreProject(ctx, projectID, actor, s.withSerializableTx)
}

func (s *Store) restoreProject(
	ctx context.Context,
	projectID int64,
	actor string,
	runTx func(context.Context, transactionFunc) error,
) (db.Project, *db.Event, bool, error) {
	var project db.Project
	var event *db.Event
	var changed bool
	err := runTx(ctx, func(tx *sql.Tx) error {
		project, event, changed = db.Project{}, nil, false
		var err error
		project, err = scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR UPDATE`, projectID))
		if err != nil {
			return err
		}
		if project.Name == db.SystemProjectName || project.UID == db.SystemProjectUID {
			return db.ErrNotFound
		}
		if project.DeletedAt == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET deleted_at = NULL WHERE id = $1`, project.ID); err != nil {
			return mapSQLError(err, nil)
		}
		created, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.restored", Actor: actor, Payload: `{}`,
		})
		if err != nil {
			return err
		}
		event = &created
		changed = true
		project, err = scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, project.ID))
		return err
	})
	return project, event, changed, err
}

// DetachProjectAlias removes one project-scoped alias and records the change.
func (s *Store) DetachProjectAlias(ctx context.Context, params db.DetachAliasParams) (db.ProjectAlias, *db.Event, error) {
	var alias db.ProjectAlias
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		alias, err = scanAlias(tx.QueryRowContext(ctx,
			aliasSelect+` WHERE id = $1 AND project_id = $2 FOR UPDATE`, params.AliasID, params.ProjectID))
		if err != nil {
			return err
		}
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR SHARE`, alias.ProjectID))
		if err != nil {
			return err
		}
		var siblings int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM project_aliases WHERE project_id = $1`, alias.ProjectID).Scan(&siblings); err != nil {
			return mapSQLError(err, nil)
		}
		if siblings <= 1 && !params.Force {
			return db.ErrAliasIsLast
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM project_aliases WHERE id = $1 AND project_id = $2`, alias.ID, alias.ProjectID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			AliasIdentity string `json:"alias_identity"`
			AliasKind     string `json:"alias_kind"`
			WasLast       bool   `json:"was_last,omitempty"`
			Force         bool   `json:"force,omitempty"`
		}{AliasIdentity: alias.AliasIdentity, AliasKind: alias.AliasKind, WasLast: siblings <= 1, Force: params.Force})
		if err != nil {
			return err
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.alias_removed", Actor: params.Actor, Payload: string(payload),
		})
		return err
	})
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}
	return alias, &event, nil
}

// BatchProjectStats returns active-project issue counts and event timestamps.
func (s *Store) BatchProjectStats(ctx context.Context) (map[int64]db.ProjectStats, error) {
	rows, err := s.QueryContext(ctx, `WITH issue_counts AS (
      SELECT project_id,
             COUNT(*) FILTER (WHERE status = 'open') AS open_count,
             COUNT(*) FILTER (WHERE status = 'closed') AS closed_count
        FROM issues WHERE deleted_at IS NULL GROUP BY project_id
    ), event_max AS (
      SELECT project_id, MAX(created_at) AS last_event_at FROM events GROUP BY project_id
    )
    SELECT p.id, COALESCE(i.open_count, 0), COALESCE(i.closed_count, 0), e.last_event_at
      FROM projects p
      LEFT JOIN issue_counts i ON i.project_id = p.id
      LEFT JOIN event_max e ON e.project_id = p.id
     WHERE p.deleted_at IS NULL AND p.name <> $1 ORDER BY p.id`, db.SystemProjectName)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int64]db.ProjectStats)
	for rows.Next() {
		var id int64
		var stats db.ProjectStats
		var lastEvent sql.NullString
		if err := rows.Scan(&id, &stats.Open, &stats.Closed, &lastEvent); err != nil {
			return nil, mapSQLError(err, nil)
		}
		if lastEvent.Valid {
			value, err := parseStoredTime(lastEvent.String)
			if err != nil {
				return nil, err
			}
			stats.LastEventAt = &value
		}
		result[id] = stats
	}
	return result, mapSQLError(rows.Err(), nil)
}

type projectPurgeCounts struct {
	issues, events, aliases, comments, links, labels, claims, pendingClaims int64
	minEventID, maxEventID                                                  sql.NullInt64
}

// PurgeProject removes an archived project and leaves a durable audit row.
func (s *Store) PurgeProject(ctx context.Context, params db.PurgeProjectParams) (db.ProjectPurgeLog, error) {
	var result db.ProjectPurgeLog
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR UPDATE`, params.ProjectID))
		if err != nil {
			return err
		}
		if project.Name == db.SystemProjectName || project.UID == db.SystemProjectUID {
			return db.ErrNotFound
		}
		if project.DeletedAt == nil {
			return db.ErrProjectNotArchived
		}
		var role string
		err = tx.QueryRowContext(ctx, `SELECT role FROM federation_bindings WHERE project_id = $1`, project.ID).Scan(&role)
		if err == nil {
			return &db.ProjectFederatedError{Role: db.FederationRole(role)}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapSQLError(err, nil)
		}

		counts, err := countProjectPurgeTx(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		if err := deleteProjectScopedTx(ctx, tx, project.ID); err != nil {
			return err
		}
		var resetCursor sql.NullInt64
		if counts.minEventID.Valid {
			if err := lockEventSequenceTx(ctx, tx); err != nil {
				return err
			}
			value, err := s.reserveIdentityValue(ctx, tx, "events", "id")
			if err != nil {
				return err
			}
			resetCursor = sql.NullInt64{Int64: value, Valid: true}
		}
		purgeUID, err := katauid.New()
		if err != nil {
			return err
		}
		var purgeID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO project_purge_log(
          uid, origin_instance_uid, project_id, project_uid, project_name,
          issue_count, event_count, alias_count, comment_count, link_count, label_count,
          claim_count, pending_claim_request_count, events_deleted_min_id, events_deleted_max_id,
          purge_reset_after_event_id, actor, reason
        ) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id`,
			purgeUID, s.instanceUID, project.ID, project.UID, project.Name,
			counts.issues, counts.events, counts.aliases, counts.comments, counts.links, counts.labels,
			counts.claims, counts.pendingClaims, counts.minEventID, counts.maxEventID, resetCursor,
			params.Actor, params.Reason,
		).Scan(&purgeID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		result, err = scanProjectPurgeLog(tx.QueryRowContext(ctx,
			`SELECT id, uid, origin_instance_uid, project_id, project_uid, project_name,
               issue_count, event_count, alias_count, comment_count, link_count, label_count,
               claim_count, pending_claim_request_count, events_deleted_min_id, events_deleted_max_id,
               purge_reset_after_event_id, actor, reason, purged_at
             FROM project_purge_log WHERE id = $1`, purgeID))
		return err
	})
	return result, err
}

func countProjectPurgeTx(ctx context.Context, tx *sql.Tx, projectID int64) (projectPurgeCounts, error) {
	var counts projectPurgeCounts
	destinations := []*int64{&counts.issues, &counts.events, &counts.aliases, &counts.comments,
		&counts.links, &counts.labels, &counts.claims, &counts.pendingClaims}
	queries := []string{
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`,
		`SELECT COUNT(*) FROM events WHERE project_id = $1`,
		`SELECT COUNT(*) FROM project_aliases WHERE project_id = $1`,
		`SELECT COUNT(*) FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`SELECT COUNT(*) FROM links WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id = $1) OR to_issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`SELECT COUNT(*) FROM issue_labels WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`SELECT COUNT(*) FROM issue_claims WHERE project_id = $1 OR issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`SELECT COUNT(*) FROM pending_claim_requests WHERE project_id = $1 OR issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
	}
	for index, query := range queries {
		if err := tx.QueryRowContext(ctx, query, projectID).Scan(destinations[index]); err != nil {
			return projectPurgeCounts{}, mapSQLError(err, nil)
		}
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM events WHERE project_id = $1`, projectID,
	).Scan(&counts.minEventID, &counts.maxEventID); err != nil {
		return projectPurgeCounts{}, mapSQLError(err, nil)
	}
	return counts, nil
}

func deleteProjectScopedTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id = $1`, projectID); err != nil {
		return mapSQLError(err, nil)
	}
	var detachedEventIDs []int64
	for _, statement := range []string{
		`UPDATE events SET issue_id = NULL, issue_uid = NULL WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1) RETURNING id`,
		`UPDATE events SET related_issue_id = NULL, related_issue_uid = NULL WHERE related_issue_id IN (SELECT id FROM issues WHERE project_id = $1) RETURNING id`,
	} {
		rows, err := tx.QueryContext(ctx, statement, projectID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		ids, err := collectEventIDs(rows)
		if err != nil {
			return err
		}
		detachedEventIDs = append(detachedEventIDs, ids...)
	}
	if err := recomputeEventContentHashesTx(ctx, tx, detachedEventIDs); err != nil {
		return err
	}
	statements := []string{
		`DELETE FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM links WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id = $1) OR to_issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM issue_labels WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM issue_claims WHERE project_id = $1 OR issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM pending_claim_requests WHERE project_id = $1 OR issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM issues WHERE project_id = $1`,
		`DELETE FROM project_aliases WHERE project_id = $1`,
		`DELETE FROM federation_sync_status WHERE project_id = $1`,
		`DELETE FROM federation_quarantine WHERE project_id = $1`,
		`DELETE FROM federation_enrollments WHERE project_id = $1`,
		`DELETE FROM projects WHERE id = $1`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, projectID); err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}

func scanProjectPurgeLog(row rowScanner) (db.ProjectPurgeLog, error) {
	var result db.ProjectPurgeLog
	var purgedAt string
	err := row.Scan(
		&result.ID, &result.UID, &result.OriginInstanceUID, &result.ProjectID, &result.ProjectUID,
		&result.ProjectName, &result.IssueCount, &result.EventCount, &result.AliasCount,
		&result.CommentCount, &result.LinkCount, &result.LabelCount, &result.ClaimCount,
		&result.PendingClaimRequestCount, &result.EventsDeletedMinID, &result.EventsDeletedMaxID,
		&result.PurgeResetAfterEventID, &result.Actor, &result.Reason, &purgedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ProjectPurgeLog{}, db.ErrNotFound
	}
	if err != nil {
		return db.ProjectPurgeLog{}, mapSQLError(err, nil)
	}
	result.PurgedAt, err = parseStoredTime(purgedAt)
	if err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("parse project purge timestamp: %w", err)
	}
	return result, nil
}
