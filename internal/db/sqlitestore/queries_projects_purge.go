package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// PurgeProject permanently deletes an archived project and every project-scoped
// row, reserves an SSE reset cursor, and writes a project_purge_log tombstone.
// Mirrors PurgeIssue's BEGIN IMMEDIATE pattern so count snapshots are stable.
func (d *Store) PurgeProject(ctx context.Context, p db.PurgeProjectParams) (db.ProjectPurgeLog, error) {
	return retryWrite1(ctx, d, func() (db.ProjectPurgeLog, error) {
		return d.purgeProject(ctx, p)
	})
}

func (d *Store) purgeProject(ctx context.Context, p db.PurgeProjectParams) (db.ProjectPurgeLog, error) {
	conn, err := d.Conn(ctx)
	if err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE TRANSACTION"); err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Detached context so rollback runs even if the caller's ctx is
			// already canceled — otherwise the conn returns to the pool with an
			// open transaction after a mid-flight cancellation.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	project, err := scanProject(conn.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.ProjectID))
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	if isSystemProject(project) {
		return db.ProjectPurgeLog{}, db.ErrNotFound
	}
	if project.DeletedAt == nil {
		return db.ProjectPurgeLog{}, db.ErrProjectNotArchived
	}
	role, err := federationBindingRole(ctx, conn, project.ID)
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	if role != "" {
		return db.ProjectPurgeLog{}, &db.ProjectFederatedError{Role: db.FederationRole(role)}
	}

	plID, err := purgeProjectCascade(ctx, conn, project, p.Actor, p.Reason, d.instanceUID)
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	pl, err := scanProjectPurgeLog(ctx, conn, plID)
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return pl, nil
}

// federationBindingRole returns the role of any federation_bindings row for the
// project (hub or spoke), or "" when none exists. Presence of any binding
// blocks purge — federation must be torn down first.
func federationBindingRole(ctx context.Context, q sqlReader, projectID int64) (string, error) {
	var role string
	err := q.QueryRowContext(ctx,
		`SELECT role FROM federation_bindings WHERE project_id = ?`, projectID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check federation binding: %w", err)
	}
	return role, nil
}

type projectPurgeCounts struct {
	issues, events, aliases, comments, links, labels, claims, pendingClaims int64
	minEventID, maxEventID                                                  sql.NullInt64
}

func countProjectPurge(ctx context.Context, c connExec, projectID int64) (projectPurgeCounts, error) {
	var n projectPurgeCounts
	const sub = `(SELECT id FROM issues WHERE project_id = ?)`
	get := func(dst *int64, q string, args ...any) error {
		v, err := scanCount(ctx, c, q, args...)
		*dst = v
		return err
	}
	if err := errors.Join(
		get(&n.issues, `SELECT count(*) FROM issues WHERE project_id = ?`, projectID),
		get(&n.events, `SELECT count(*) FROM events WHERE project_id = ?`, projectID),
		get(&n.aliases, `SELECT count(*) FROM project_aliases WHERE project_id = ?`, projectID),
		get(&n.comments, `SELECT count(*) FROM comments WHERE issue_id IN `+sub, projectID),
		get(&n.links, `SELECT count(*) FROM links WHERE from_issue_id IN `+sub+` OR to_issue_id IN `+sub, projectID, projectID),
		get(&n.labels, `SELECT count(*) FROM issue_labels WHERE issue_id IN `+sub, projectID),
		get(&n.claims, `SELECT count(*) FROM issue_claims WHERE project_id = ? OR issue_id IN `+sub, projectID, projectID),
		get(&n.pendingClaims, `SELECT count(*) FROM pending_claim_requests WHERE project_id = ? OR issue_id IN `+sub, projectID, projectID),
	); err != nil {
		return projectPurgeCounts{}, fmt.Errorf("count project purge rows: %w", err)
	}
	if err := c.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM events WHERE project_id = ?`, projectID).
		Scan(&n.minEventID, &n.maxEventID); err != nil {
		return projectPurgeCounts{}, fmt.Errorf("scan event id range: %w", err)
	}
	return n, nil
}

// deleteProjectScoped removes every project-scoped row in FK-safe order. Events
// physically in the project are deleted; events in OTHER projects that reference
// purged issues are DETACHED (both id and uid columns nulled) so per-project
// resume stays valid. federation_bindings is absent (refused upfront).
// NOTE: purge_log (issue tombstones) is intentionally NOT deleted — it has no FK
// to projects so it survives, preserving prior-purge audit history (spec Finding 3).
// recurrences / issue_sync_bindings / issue_sync_status / import_mappings are not
// listed here: they ON DELETE CASCADE off the final `DELETE FROM projects`.
func deleteProjectScoped(ctx context.Context, c connExec, projectID int64) error {
	const sub = `(SELECT id FROM issues WHERE project_id = ?)`
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM events WHERE project_id = ?`, []any{projectID}},
		{`UPDATE events SET issue_id = NULL, issue_uid = NULL WHERE issue_id IN ` + sub, []any{projectID}},
		{`UPDATE events SET related_issue_id = NULL, related_issue_uid = NULL WHERE related_issue_id IN ` + sub, []any{projectID}},
		{`DELETE FROM comments WHERE issue_id IN ` + sub, []any{projectID}},
		{`DELETE FROM links WHERE from_issue_id IN ` + sub + ` OR to_issue_id IN ` + sub, []any{projectID, projectID}},
		{`DELETE FROM issue_labels WHERE issue_id IN ` + sub, []any{projectID}},
		{`DELETE FROM issue_claims WHERE project_id = ? OR issue_id IN ` + sub, []any{projectID, projectID}},
		{`DELETE FROM pending_claim_requests WHERE project_id = ? OR issue_id IN ` + sub, []any{projectID, projectID}},
		{`DELETE FROM issues WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM project_aliases WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM federation_sync_status WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM federation_quarantine WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM federation_enrollments WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM projects WHERE id = ?`, []any{projectID}},
	}
	for _, s := range stmts {
		if _, err := c.ExecContext(ctx, s.q, s.args...); err != nil {
			return fmt.Errorf("project purge delete (%s): %w", s.q, err)
		}
	}
	return nil
}

func purgeProjectCascade(ctx context.Context, c connExec, project db.Project,
	actor string, reason *string, originInstanceUID string) (int64, error) {
	counts, err := countProjectPurge(ctx, c, project.ID)
	if err != nil {
		return 0, err
	}
	if err := deleteProjectScoped(ctx, c, project.ID); err != nil {
		return 0, err
	}
	reservedCursor, err := reserveEventSequence(ctx, c, counts.minEventID.Valid)
	if err != nil {
		return 0, err
	}
	purgeUID, err := katauid.New()
	if err != nil {
		return 0, fmt.Errorf("generate project purge uid: %w", err)
	}
	res, err := c.ExecContext(ctx,
		`INSERT INTO project_purge_log(
		   uid, origin_instance_uid, project_id, project_uid, project_name,
		   issue_count, event_count, alias_count, comment_count, link_count, label_count,
		   claim_count, pending_claim_request_count,
		   events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		   actor, reason)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		purgeUID, originInstanceUID, project.ID, project.UID, project.Name,
		counts.issues, counts.events, counts.aliases, counts.comments, counts.links, counts.labels,
		counts.claims, counts.pendingClaims,
		counts.minEventID, counts.maxEventID, reservedCursor,
		actor, reason)
	if err != nil {
		return 0, fmt.Errorf("insert project_purge_log: %w", err)
	}
	return res.LastInsertId()
}

func scanProjectPurgeLog(ctx context.Context, r sqlReader, id int64) (db.ProjectPurgeLog, error) {
	const q = `
		SELECT id, uid, origin_instance_uid, project_id, project_uid, project_name,
		       issue_count, event_count, alias_count, comment_count, link_count, label_count,
		       claim_count, pending_claim_request_count,
		       events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		       actor, reason, purged_at
		FROM project_purge_log WHERE id = ?`
	var pl db.ProjectPurgeLog
	err := r.QueryRowContext(ctx, q, id).Scan(
		&pl.ID, &pl.UID, &pl.OriginInstanceUID, &pl.ProjectID, &pl.ProjectUID, &pl.ProjectName,
		&pl.IssueCount, &pl.EventCount, &pl.AliasCount, &pl.CommentCount, &pl.LinkCount, &pl.LabelCount,
		&pl.ClaimCount, &pl.PendingClaimRequestCount,
		&pl.EventsDeletedMinID, &pl.EventsDeletedMaxID, &pl.PurgeResetAfterEventID,
		&pl.Actor, &pl.Reason, &pl.PurgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ProjectPurgeLog{}, db.ErrNotFound
	}
	if err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("scan project_purge_log: %w", err)
	}
	return pl, nil
}
