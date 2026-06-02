package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

// MaxEventID returns the highest events.id, or 0 when the table is empty. The
// SSE handler uses this as the high-water mark snapshot after Subscribe.
func (d *Store) MaxEventID(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := d.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("max event id: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

// EventsAfter returns up to Limit events ordered by id ASC. The issue and
// related_issue short_ids are joined from the live `issues` table so events
// render with display ids that stay current even after `kata projects merge`
// or a future federation merge shifts a peer's short_id. UIDs remain stable.
func (d *Store) EventsAfter(ctx context.Context, p db.EventsAfterParams) ([]db.Event, error) {
	var (
		conds []string
		args  []any
	)
	conds = append(conds, "e.id > ?")
	args = append(args, p.AfterID)
	conds = append(conds, "p.name <> ?")
	args = append(args, db.SystemProjectName)
	if p.ProjectID != 0 {
		conds = append(conds, "e.project_id = ?")
		args = append(args, p.ProjectID)
	}
	if p.ThroughID != 0 {
		conds = append(conds, "e.id <= ?")
		args = append(args, p.ThroughID)
	}
	q := `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
	             e.issue_id, e.issue_uid, i.short_id, e.related_issue_id, e.related_issue_uid, ri.short_id,
	             e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at
	      FROM events e
	      JOIN projects p ON p.id = e.project_id
	      LEFT JOIN issues i ON i.project_id = e.project_id AND (i.id = e.issue_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL AND i.uid = e.issue_uid))
	      LEFT JOIN issues ri ON ri.project_id = e.project_id AND (ri.id = e.related_issue_id OR (e.related_issue_id IS NULL AND e.related_issue_uid IS NOT NULL AND ri.uid = e.related_issue_uid))
	      WHERE ` + strings.Join(conds, " AND ") + ` ORDER BY e.id ASC LIMIT ?`
	args = append(args, p.Limit)
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events after: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsByUIDs returns project events matching uids in insertion order. It is
// used by federation ingest to broadcast only fresh rows after an all-or-
// nothing insert commits.
func (d *Store) EventsByUIDs(ctx context.Context, projectID int64, uids []string) ([]db.Event, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	out := make([]db.Event, 0, len(uids))
	for _, uid := range uids {
		var id int64
		err := d.QueryRowContext(ctx,
			`SELECT id FROM events WHERE project_id = ? AND uid = ?`,
			projectID, uid).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lookup event uid %s: %w", uid, err)
		}
		e, err := scanEvent(d.QueryRowContext(ctx, eventSelectByID, id))
		if err != nil {
			return nil, fmt.Errorf("read event uid %s: %w", uid, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// EventsInWindow returns every event in the requested window. There is no row
// cap: digest is a one-shot read and the caller has already chosen a finite
// window. Callers are expected to pass a sane window (typically <= 7 days).
func (d *Store) EventsInWindow(ctx context.Context, p db.EventsInWindowParams) ([]db.Event, error) {
	var (
		conds []string
		args  []any
	)
	conds = append(conds, "e.created_at >= ?")
	args = append(args, p.Since)
	conds = append(conds, "e.created_at <= ?")
	args = append(args, p.Until)
	conds = append(conds, "p.name <> ?")
	args = append(args, db.SystemProjectName)
	if p.ProjectID != 0 {
		conds = append(conds, "e.project_id = ?")
		args = append(args, p.ProjectID)
	}
	if len(p.Actors) > 0 {
		placeholders := make([]string, len(p.Actors))
		for i, a := range p.Actors {
			placeholders[i] = "?"
			args = append(args, a)
		}
		conds = append(conds, "e.actor IN ("+strings.Join(placeholders, ",")+")")
	}
	q := `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name, e.issue_id, e.issue_uid, i.short_id,
	             e.related_issue_id, e.related_issue_uid, ri.short_id,
	             e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at
	      FROM events e
	      JOIN projects p ON p.id = e.project_id
	      LEFT JOIN issues i ON i.project_id = e.project_id AND (i.id = e.issue_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL AND i.uid = e.issue_uid))
	      LEFT JOIN issues ri ON ri.project_id = e.project_id AND (ri.id = e.related_issue_id OR (e.related_issue_id IS NULL AND e.related_issue_uid IS NOT NULL AND ri.uid = e.related_issue_uid))
	      WHERE ` + strings.Join(conds, " AND ") + ` ORDER BY e.id ASC`
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events in window: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Event
	for rows.Next() {
		var e db.Event
		if err := rows.Scan(&e.ID, &e.UID, &e.OriginInstanceUID, &e.ProjectID, &e.ProjectUID, &e.ProjectName, &e.IssueID, &e.IssueUID, &e.IssueShortID,
			&e.RelatedIssueID, &e.RelatedIssueUID, &e.RelatedIssueShortID,
			&e.Type, &e.Actor, &e.Payload, &e.HLCPhysicalMS, &e.HLCCounter, &e.ContentHash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentSiblingCloses returns issue.closed events emitted by actor on direct
// children of parentIssueID in projectID since the given timestamp, EXCLUDING
// any prior close of excludeIssueID itself. Ordered by created_at DESC so
// callers can render the most recent closures first.
//
// Used by the sibling-close throttle (spec §3.9) and the repeated-message
// guard (§3.10). The exclude filter keeps a reopen→re-close cycle on the
// same issue from matching its own prior close: the guards are intended to
// compare against SIBLING issues, not the issue currently being closed.
//
// The same scoped projection used by EventsInWindow is sufficient here — the
// guards only need id, issue_short_id, actor, payload, and created_at; the
// wider uid/related columns stay zero-valued.
func (d *Store) RecentSiblingCloses(
	ctx context.Context,
	projectID, parentIssueID, excludeIssueID int64,
	actor string,
	since time.Time,
) ([]db.Event, error) {
	const q = `SELECT e.id, e.project_id, e.project_name, e.issue_id,
	                  i.short_id,
	                  e.type, e.actor, e.payload, e.created_at
	           FROM events e
	           JOIN links l ON l.from_issue_id = e.issue_id
	           JOIN issues i ON i.id = e.issue_id
	           WHERE e.project_id = ?
	             AND e.type = 'issue.closed'
	             AND e.actor = ?
	             AND e.created_at >= ?
	             AND l.type = 'parent'
	             AND l.to_issue_id = ?
	             AND l.project_id = ?
	             AND e.issue_id <> ?
	           ORDER BY e.created_at DESC`
	rows, err := d.QueryContext(ctx, q,
		projectID, actor, since.UTC().Format(sqliteTimeFormat),
		parentIssueID, projectID, excludeIssueID)
	if err != nil {
		return nil, fmt.Errorf("recent sibling closes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Event
	for rows.Next() {
		var e db.Event
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ProjectName, &e.IssueID,
			&e.IssueShortID, &e.Type, &e.Actor, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan recent sibling close: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentSameMessageClose returns the most recent issue.closed event from
// RecentSiblingCloses whose payload's normalized message equals
// normalizedMessage and whose reason is "done" or "audit-no-change".
// Used by the repeated-message guard (§3.10) to refuse a second sibling
// close that reuses the same prose under the same parent within a short
// window. Returns (nil, nil) when no match exists.
//
// The reason filter mirrors the spec: wontfix, duplicate, and superseded
// closes can legitimately reuse boilerplate (e.g. "out of scope"), so
// they are exempt; only the open-ended done / audit-no-change reasons
// are policed.
//
// Callers (the daemon) are expected to pre-normalize normalizedMessage
// using the same rules as normalizeMessageDB below — both sides apply
// the same trim/lowercase/punctuation rules so a literal copy-paste
// matches even when the surrounding whitespace differs.
func (d *Store) RecentSameMessageClose(
	ctx context.Context,
	projectID, parentIssueID, excludeIssueID int64,
	actor, normalizedMessage string,
	since time.Time,
) (*db.Event, error) {
	siblings, err := d.RecentSiblingCloses(ctx, projectID, parentIssueID, excludeIssueID, actor, since)
	if err != nil {
		return nil, err
	}
	for i := range siblings {
		var p struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(siblings[i].Payload), &p); err != nil {
			continue
		}
		if p.Reason != "done" && p.Reason != "audit-no-change" {
			continue
		}
		if normalizeMessageDB(p.Message) == normalizedMessage {
			return &siblings[i], nil
		}
	}
	return nil, nil
}

// normalizeMessageDB is the db-side mirror of the daemon's NormalizeMessage
// (close_validation.go). It is intentionally duplicated rather than imported:
// internal/api already imports internal/db, so the db package cannot reach
// daemon without creating an import cycle. Keep these two implementations
// in lockstep — if one changes, update the other.
func normalizeMessageDB(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ToLower(s)
	s = strings.TrimRight(s, ".?!")
	return s
}

// MaxLocalOriginEventID returns the largest events.id row whose project_id
// matches and whose origin_instance_uid is this database's instance. Federation
// uses it to seed the push cursor at "everything we authored so far". Returns 0
// when no matching rows exist.
func (d *Store) MaxLocalOriginEventID(ctx context.Context, projectID int64) (int64, error) {
	var n sql.NullInt64
	if err := d.QueryRowContext(ctx, `
		SELECT MAX(id)
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?`,
		projectID, d.InstanceUID()).Scan(&n); err != nil {
		return 0, fmt.Errorf("max local-origin event id: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

// MaxFederationBaselineEventID returns the largest events.id row of type
// 'issue.snapshot' whose id is at least sinceEventID, scoped to projectID.
// Federation's status report uses this to declare "baseline materialized
// through" the highest snapshot at or above the replay horizon. Returns 0 when
// no matching snapshot exists.
func (d *Store) MaxFederationBaselineEventID(ctx context.Context, projectID, sinceEventID int64) (int64, error) {
	var n sql.NullInt64
	if err := d.QueryRowContext(ctx, `
		SELECT MAX(id)
		  FROM events
		 WHERE project_id = ?
		   AND type = 'issue.snapshot'
		   AND id >= ?`,
		projectID, sinceEventID).Scan(&n); err != nil {
		return 0, fmt.Errorf("max federation baseline event id: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

// PurgeResetCheck returns the maximum purge_reset_after_event_id strictly
// greater than afterID, optionally constrained to a project. Returns 0 when
// no matching purge_log row exists. The strict > semantics align with the
// spec §2.6 reservation: every reserved cursor is greater than every real
// events.id at the moment of the purge, so cursor == reservedID means the
// client is already past it and does not need a reset.
//
// projectID == 0 = cross-project (no filter).
func (d *Store) PurgeResetCheck(ctx context.Context, afterID, projectID int64) (int64, error) {
	q := `SELECT MAX(purge_reset_after_event_id) FROM purge_log
	      WHERE purge_reset_after_event_id IS NOT NULL AND purge_reset_after_event_id > ?`
	args := []any{afterID}
	if projectID != 0 {
		q += ` AND project_id = ?`
		args = append(args, projectID)
	}
	var n sql.NullInt64
	if err := d.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("purge reset check: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}
