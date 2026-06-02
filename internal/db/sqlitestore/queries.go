package sqlitestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/similarity"
	katauid "go.kenn.io/kata/internal/uid"
)

// CreateProject inserts a new projects row.
func (d *Store) CreateProject(ctx context.Context, name string) (db.Project, error) {
	if name == db.SystemProjectName {
		return db.Project{}, fmt.Errorf("create project: reserved project name %q", name)
	}
	projectUID, err := katauid.New()
	if err != nil {
		return db.Project{}, fmt.Errorf("generate project uid: %w", err)
	}
	return d.CreateProjectWithUID(ctx, name, projectUID)
}

// CreateProjectWithUID inserts a project with a caller-supplied stable UID.
// Live local callers should use CreateProject; federation replica setup uses
// this to make the local spoke project carry the hub project UID.
func (d *Store) CreateProjectWithUID(ctx context.Context, name, projectUID string) (db.Project, error) {
	if !katauid.Valid(projectUID) {
		return db.Project{}, fmt.Errorf("invalid project uid %q", projectUID)
	}
	res, err := d.ExecContext(ctx,
		`INSERT INTO projects(uid, name) VALUES(?, ?)`, projectUID, name)
	if err != nil {
		return db.Project{}, fmt.Errorf("insert project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.Project{}, fmt.Errorf("last id: %w", err)
	}
	return d.ProjectByID(ctx, id)
}

// ProjectByID fetches one project by its rowid. Archived (deleted_at != NULL)
// projects are returned as-is so callers like the merge / restore paths can
// see them; surface-level callers (HTTP handlers, CLI) inspect DeletedAt
// themselves.
func (d *Store) ProjectByID(ctx context.Context, id int64) (db.Project, error) {
	row := d.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, id)
	return hideSystemProject(scanProject(row))
}

// ProjectByName fetches one project by its UNIQUE name. Archived projects are
// excluded — resolve flow uses this and an archived project must look gone
// from the active surface. Callers needing the row even when archived can
// follow up with ProjectByNameIncludingArchived.
func (d *Store) ProjectByName(ctx context.Context, name string) (db.Project, error) {
	row := d.QueryRowContext(ctx,
		projectSelect+` WHERE name = ? AND deleted_at IS NULL`, name)
	return hideSystemProject(scanProject(row))
}

// ProjectByNameIncludingArchived returns the project even when archived.
// Used by error-message paths that want to distinguish "no project at all"
// from "project was archived".
func (d *Store) ProjectByNameIncludingArchived(ctx context.Context, name string) (db.Project, error) {
	row := d.QueryRowContext(ctx, projectSelect+` WHERE name = ?`, name)
	return hideSystemProject(scanProject(row))
}

// ProjectByUID fetches one project by its stable UID. Archived
// (deleted_at != NULL) projects are returned as-is so callers can decide
// how to surface the archived state; surface-level handlers should
// inspect DeletedAt themselves. Returns ErrNotFound when no row matches.
func (d *Store) ProjectByUID(ctx context.Context, uid string) (db.Project, error) {
	row := d.QueryRowContext(ctx, projectSelect+` WHERE uid = ?`, uid)
	return hideSystemProject(scanProject(row))
}

// HardDeleteProject permanently removes a project row by id. It exists for the
// init-race orphan-cleanup path (a freshly created project whose alias attach
// then failed); it is NOT the user-facing archival path (see RemoveProject).
func (d *Store) HardDeleteProject(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

// RenameProject updates a project's canonical name without changing aliases or
// issue numbering.
func (d *Store) RenameProject(ctx context.Context, id int64, name string) (db.Project, error) {
	res, err := d.ExecContext(ctx, `UPDATE projects SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return db.Project{}, fmt.Errorf("rename project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Project{}, fmt.Errorf("rename project rows affected: %w", err)
	}
	if n == 0 {
		return db.Project{}, db.ErrNotFound
	}
	return d.ProjectByID(ctx, id)
}

// ListProjects returns every active project ordered by id ASC. Archived
// projects (deleted_at != NULL) are excluded; callers needing them too can
// use ListProjectsIncludingArchived.
func (d *Store) ListProjects(ctx context.Context) ([]db.Project, error) {
	return d.listProjects(ctx, false)
}

// ListProjectsIncludingArchived returns every project including archived
// rows. Used by surfaces that want to render archived state explicitly
// (e.g. operator inspection or restore tooling).
func (d *Store) ListProjectsIncludingArchived(ctx context.Context) ([]db.Project, error) {
	return d.listProjects(ctx, true)
}

func (d *Store) listProjects(ctx context.Context, includeArchived bool) ([]db.Project, error) {
	q := projectSelect
	if !includeArchived {
		q += ` WHERE deleted_at IS NULL AND name <> ?`
	} else {
		q += ` WHERE name <> ?`
	}
	q += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, q, db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BatchProjectStats returns aggregate stats for every active project. The
// result includes projects with zero issues (Open=0, Closed=0) and zero
// events (LastEventAt=nil), driven by LEFT JOINs onto pre-aggregated
// subqueries. Pre-aggregation matters: the naive
// projects⋈issues⋈events GROUP BY shape would multiply each issue row by
// each event row and inflate counts. Spec §6.1.
func (d *Store) BatchProjectStats(ctx context.Context) (map[int64]db.ProjectStats, error) {
	const q = `
WITH
  issue_counts AS (
    SELECT
      project_id,
      SUM(CASE WHEN status = 'open'   THEN 1 ELSE 0 END) AS open_count,
      SUM(CASE WHEN status = 'closed' THEN 1 ELSE 0 END) AS closed_count
    FROM issues
    WHERE deleted_at IS NULL
    GROUP BY project_id
  ),
  event_max AS (
    -- julianday() normalizes both T-separated RFC3339 and space/offset
    -- legacy layouts to a numeric julian day, so MAX picks the
    -- absolute-latest event regardless of which text format was stored.
    -- strftime() formats it back to RFC3339Nano with a 'Z' zone, matching
    -- the layout the rest of the code emits via strftime() on insert.
    SELECT project_id,
           strftime('%Y-%m-%dT%H:%M:%fZ', MAX(julianday(created_at))) AS last_event_at
    FROM events
    GROUP BY project_id
  )
SELECT
  p.id,
  COALESCE(ic.open_count,   0),
  COALESCE(ic.closed_count, 0),
  em.last_event_at
FROM projects p
LEFT JOIN issue_counts ic ON ic.project_id = p.id
LEFT JOIN event_max    em ON em.project_id = p.id
WHERE p.deleted_at IS NULL AND p.name <> ?
ORDER BY p.id`
	rows, err := d.QueryContext(ctx, q, db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("batch project stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]db.ProjectStats{}
	for rows.Next() {
		var (
			id     int64
			open   int
			closed int
			ts     sql.NullString
		)
		if err := rows.Scan(&id, &open, &closed, &ts); err != nil {
			return nil, fmt.Errorf("scan project stats: %w", err)
		}
		s := db.ProjectStats{Open: open, Closed: closed}
		if ts.Valid {
			t, err := parseSQLiteTimestamp(ts.String)
			if err != nil {
				return nil, fmt.Errorf("parse last_event_at %q: %w", ts.String, err)
			}
			s.LastEventAt = &t
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// parseSQLiteTimestamp parses a TIMESTAMP-typed column value returned as a
// driver string. The current schema's strftime('%Y-%m-%dT%H:%M:%fZ','now')
// produces RFC3339 with millisecond precision and a 'Z' zone, but databases
// imported from older snapshots may carry SQLite's other supported text
// layouts: bare ("YYYY-MM-DD HH:MM:SS[.SSS]") or zoned with an explicit
// offset suffix (matching jsonl.parseExportTime). Fall through the layouts
// in order; surface the original error when none match so a corrupt value
// still returns an actionable wrap.
func parseSQLiteTimestamp(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	var firstErr error
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return time.Time{}, firstErr
}

// AttachAlias inserts a project_aliases row.
func (d *Store) AttachAlias(ctx context.Context, projectID int64, identity, kind, rootPath string) (db.ProjectAlias, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO project_aliases(project_id, alias_identity, alias_kind, root_path)
		 VALUES(?, ?, ?, ?)`, projectID, identity, kind, rootPath)
	if err != nil {
		return db.ProjectAlias{}, fmt.Errorf("insert alias: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.ProjectAlias{}, err
	}
	return d.AliasByID(ctx, id)
}

// AliasByIdentity returns the alias for a given alias_identity.
func (d *Store) AliasByIdentity(ctx context.Context, identity string) (db.ProjectAlias, error) {
	row := d.QueryRowContext(ctx, aliasSelect+` WHERE alias_identity = ?`, identity)
	return scanAlias(row)
}

// AliasByID returns the project_aliases row with the given id.
func (d *Store) AliasByID(ctx context.Context, id int64) (db.ProjectAlias, error) {
	row := d.QueryRowContext(ctx, aliasSelect+` WHERE id = ?`, id)
	return scanAlias(row)
}

// ReassignAlias moves an existing alias row to a different project and updates
// its root_path and last_seen_at. Used by the reassign=true branch of alias
// attach.
func (d *Store) ReassignAlias(ctx context.Context, aliasID, projectID int64, rootPath string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE project_aliases
		 SET project_id = ?, root_path = ?, last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`,
		projectID, rootPath, aliasID)
	return err
}

// TouchAlias updates last_seen_at to now and rewrites root_path. Returns
// ErrNotFound when no alias has the given id.
func (d *Store) TouchAlias(ctx context.Context, aliasID int64, rootPath string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE project_aliases
		 SET last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		     root_path    = ?
		 WHERE id = ?`, rootPath, aliasID)
	if err != nil {
		return fmt.Errorf("touch alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch alias rows affected: %w", err)
	}
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// ProjectAliases returns every alias attached to a project ordered by id ASC.
func (d *Store) ProjectAliases(ctx context.Context, projectID int64) ([]db.ProjectAlias, error) {
	rows, err := d.QueryContext(ctx, aliasSelect+` WHERE project_id = ? ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.ProjectAlias
	for rows.Next() {
		a, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// projectSelect is the canonical SELECT list for projects rows.
const projectSelect = `SELECT id, uid, name, metadata, revision, created_at, deleted_at FROM projects`

// rowScanner is the subset of *sql.Row / *sql.Rows used by scan helpers.
type rowScanner interface {
	Scan(...any) error
}

func scanProject(r rowScanner) (db.Project, error) {
	var p db.Project
	err := r.Scan(&p.ID, &p.UID, &p.Name, &p.Metadata, &p.Revision, &p.CreatedAt, &p.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Project{}, db.ErrNotFound
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("scan project: %w", err)
	}
	return p, nil
}

// aliasSelect is the canonical SELECT list for project_aliases rows.
const aliasSelect = `SELECT id, project_id, alias_identity, alias_kind, root_path, created_at, last_seen_at FROM project_aliases`

func scanAlias(r rowScanner) (db.ProjectAlias, error) {
	var a db.ProjectAlias
	err := r.Scan(&a.ID, &a.ProjectID, &a.AliasIdentity, &a.AliasKind, &a.RootPath, &a.CreatedAt, &a.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ProjectAlias{}, db.ErrNotFound
	}
	if err != nil {
		return db.ProjectAlias{}, fmt.Errorf("scan alias: %w", err)
	}
	return a, nil
}

// CreateIssue inserts an issue, applies optional initial labels/links/owner,
// and appends a single issue.created event whose payload describes the initial
// state. All steps run in one TX.
func (d *Store) CreateIssue(ctx context.Context, p db.CreateIssueParams) (db.Issue, db.Event, error) {
	// Normalize: a non-nil pointer to "" is treated as no owner. The payload
	// already drops empty owner via omitempty; making the DB column NULL keeps
	// the two views consistent and matches the unassigned semantic.
	owner := p.Owner
	if owner != nil && *owner == "" {
		owner = nil
	}

	// Dedupe links by (type, to_number) before validation so the validation
	// switch still rejects bad types and downstream insertion + payload both
	// reflect the same deduped slice.
	links := dedupeLinks(p.Links)

	// Link types are validated client-side (small fixed set) so a bad type
	// returns immediately without opening a transaction. Label charset is
	// validated server-side via classifyLabelInsertError because mirroring
	// the schema's GLOB pattern in Go would risk drift; a bad label rolls
	// back the whole TX, which is acceptable for an all-or-nothing create.
	for _, l := range links {
		switch l.Type {
		case "parent":
			if l.Incoming {
				// No inverse parent direction is exposed: a child-side link
				// is filed from the child's POV via type=parent. Reject the
				// nonsensical "this issue is the parent of N" form rather
				// than silently swap directions.
				return db.Issue{}, db.Event{}, db.ErrInitialLinkInvalidType
			}
		case "blocks", "related":
		default:
			return db.Issue{}, db.Event{}, db.ErrInitialLinkInvalidType
		}
	}

	tx, err := d.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		projectName string
		projectUID  string
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT name, uid FROM projects WHERE id = ? AND deleted_at IS NULL`, p.ProjectID).
		Scan(&projectName, &projectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Issue{}, db.Event{}, db.ErrNotFound
		}
		return db.Issue{}, db.Event{}, fmt.Errorf("lookup project for create: %w", err)
	}
	if err := ensureProjectWritableTx(ctx, tx, p.ProjectID); err != nil {
		return db.Issue{}, db.Event{}, err
	}

	issueUID := p.UID
	if issueUID == "" {
		issueUID, err = katauid.New()
		if err != nil {
			return db.Issue{}, db.Event{}, fmt.Errorf("generate issue uid: %w", err)
		}
	} else if !katauid.Valid(issueUID) {
		return db.Issue{}, db.Event{}, fmt.Errorf("invalid issue uid %q", issueUID)
	}

	shortID, err := resolveShortID(ctx, tx, p.ProjectID, issueUID, p.ShortIDOverride)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	createdAt := time.Now().UTC().Format(sqliteTimeFormat)

	// Insert issue + optional owner/priority columns in one statement.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO issues(uid, project_id, short_id, title, body, author, owner, priority, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issueUID, p.ProjectID, shortID, p.Title, p.Body, p.Author, owner, p.Priority, createdAt, createdAt)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("insert issue: %w", err)
	}
	issueID, err := res.LastInsertId()
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}

	// Initial labels — dedupe (preserve first occurrence), then alphabetize
	// for stable payload + storage order.
	labels := dedupeStrings(p.Labels)
	sortStrings(labels)
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
			issueID, label, p.Author); err != nil {
			return db.Issue{}, db.Event{}, classifyLabelInsertError(err)
		}
	}

	// Initial links — resolve to_number → (to_issue_id, to_issue_uid,
	// to_issue_short_id) within the same project, excluding soft-deleted
	// targets. The schema's same-project trigger enforces the cross-project
	// check, but we'd rather surface a typed not-found than a generic
	// constraint failure. The peer UID and short_id are captured here and
	// folded into the issue.created event payload: UID is canonical, short_id
	// is the rendered display value (spec §11).
	resolvedTargets := make([]createdLinkTarget, 0, len(links))
	for _, l := range links {
		var (
			toIssueID      int64
			toIssueUID     string
			toIssueShortID string
		)
		// Initial-link targets are addressed by their issue ID for now; the
		// CLI/daemon will be migrated to short_ids in Tasks 11/14. Until
		// then this lookup intentionally treats ToNumber as a numeric ID.
		err := tx.QueryRowContext(ctx,
			`SELECT id, uid, short_id FROM issues
			 WHERE project_id = ? AND id = ? AND deleted_at IS NULL`,
			p.ProjectID, l.ToNumber).Scan(&toIssueID, &toIssueUID, &toIssueShortID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.Issue{}, db.Event{}, db.ErrInitialLinkTargetNotFound
		}
		if err != nil {
			return db.Issue{}, db.Event{}, fmt.Errorf("resolve initial link target: %w", err)
		}
		resolvedTargets = append(resolvedTargets, createdLinkTarget{UID: toIssueUID, ShortID: toIssueShortID})
		// Canonical ordering is a storage concern: the payload reports the
		// peer's stable identity (UID + short_id), not a numeric ref.
		fromID, toID := issueID, toIssueID
		if l.Incoming && l.Type == "blocks" {
			// "this issue is blocked by N" → link runs FROM N TO new issue.
			fromID, toID = toIssueID, issueID
		}
		if l.Type == "related" && fromID > toID {
			fromID, toID = toID, fromID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
			 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
			p.ProjectID, fromID, toID, fromID, toID, l.Type, p.Author); err != nil {
			return db.Issue{}, db.Event{}, classifyLinkInsertError(err)
		}
	}

	payload, err := buildIssueCreatedPayload(issueCreatedPayload{
		UID:                    issueUID,
		ShortID:                shortID,
		Title:                  p.Title,
		Body:                   p.Body,
		Author:                 p.Author,
		Owner:                  owner,
		Priority:               p.Priority,
		Status:                 "open",
		Metadata:               json.RawMessage(`{}`),
		Labels:                 labels,
		Links:                  createdLinkPayloads(links, resolvedTargets),
		CreatedAt:              createdAt,
		IdempotencyKey:         p.IdempotencyKey,
		IdempotencyFingerprint: p.IdempotencyFingerprint,
	})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}

	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   p.ProjectID,
		ProjectUID:  projectUID,
		ProjectName: projectName,
		IssueID:     &issueID,
		IssueUID:    &issueUID,
		Type:        "issue.created",
		Actor:       p.Author,
		Payload:     payload,
	})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}

	if err := tx.Commit(); err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("commit: %w", err)
	}

	issue, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	return issue, evt, nil
}

// createdLinkTarget captures the (uid, short_id) pair for one resolved
// initial-link peer. The pair is folded into the issue.created event
// payload (spec §11): UIDs are canonical, short_ids are display snapshots.
type createdLinkTarget struct {
	UID     string
	ShortID string
}

type createdLinkOut struct {
	Type       string `json:"type"`
	ToShortID  string `json:"to_short_id,omitempty"`
	ToIssueUID string `json:"to_issue_uid,omitempty"`
	Incoming   bool   `json:"incoming,omitempty"`
}

type issueSnapshotComment struct {
	CommentUID string `json:"comment_uid"`
	Author     string `json:"author"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}

type issueCreatedPayload struct {
	UID                    string                 `json:"uid"`
	ShortID                string                 `json:"short_id"`
	Title                  string                 `json:"title"`
	Body                   string                 `json:"body"`
	Author                 string                 `json:"author"`
	Owner                  *string                `json:"owner,omitempty"`
	Priority               *int64                 `json:"priority,omitempty"`
	Status                 string                 `json:"status"`
	ClosedReason           *string                `json:"closed_reason,omitempty"`
	ClosedAt               *string                `json:"closed_at,omitempty"`
	DeletedAt              *string                `json:"deleted_at,omitempty"`
	Metadata               json.RawMessage        `json:"metadata"`
	Labels                 []string               `json:"labels,omitempty"`
	Links                  []createdLinkOut       `json:"links,omitempty"`
	Comments               []issueSnapshotComment `json:"comments,omitempty"`
	CreatedAt              string                 `json:"created_at"`
	UpdatedAt              string                 `json:"updated_at,omitempty"`
	Revision               int64                  `json:"revision,omitempty"`
	IdempotencyKey         string                 `json:"idempotency_key,omitempty"`
	IdempotencyFingerprint string                 `json:"idempotency_fingerprint,omitempty"`
	RecurrenceUID          string                 `json:"recurrence_uid,omitempty"`
	OccurrenceKey          string                 `json:"occurrence_key,omitempty"`
	Source                 string                 `json:"source,omitempty"`
	ExternalID             string                 `json:"external_id,omitempty"`
}

func createdLinkPayloads(links []db.InitialLink, targets []createdLinkTarget) []createdLinkOut {
	if len(links) == 0 {
		return nil
	}
	out := make([]createdLinkOut, 0, len(links))
	for i, l := range links {
		var t createdLinkTarget
		if i < len(targets) {
			t = targets[i]
		}
		out = append(out, createdLinkOut{
			Type:       l.Type,
			ToShortID:  t.ShortID,
			ToIssueUID: t.UID,
			Incoming:   l.Incoming,
		})
	}
	return out
}

func buildIssueCreatedPayload(p issueCreatedPayload) (string, error) {
	if len(p.Metadata) == 0 {
		p.Metadata = json.RawMessage(`{}`)
	}
	bs, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal issue.created payload: %w", err)
	}
	return string(bs), nil
}

func formatOptionalSQLiteTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := t.UTC().Format(sqliteTimeFormat)
	return &v
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// dedupeLinks removes repeated (type, to_number, incoming) entries while
// preserving first-occurrence order. Used by CreateIssue to avoid hitting
// the schema's links UNIQUE on duplicate initial links and to keep the
// issue.created event payload aligned with what was actually inserted.
//
// Incoming is part of the key because (type=blocks, to=5, incoming=false)
// and (type=blocks, to=5, incoming=true) describe distinct links: the new
// issue blocking #5 vs. the new issue being blocked by #5.
//
// For type=related the link is symmetric and canonical-ordered by storage,
// so an inbound and outbound entry for the same target produce the same
// row. We normalize Incoming → false for related entries before keying so
// (related, 5, false) and (related, 5, true) collapse to one — without
// this, the second insert would hit the schema's UNIQUE and surface as
// a 500 instead of the documented no-op.
func dedupeLinks(in []db.InitialLink) []db.InitialLink {
	type key struct {
		Type     string
		ToNumber int64
		Incoming bool
	}
	seen := make(map[key]struct{}, len(in))
	out := make([]db.InitialLink, 0, len(in))
	for _, l := range in {
		normalized := l
		if l.Type == "related" {
			normalized.Incoming = false
		}
		k := key(normalized)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func sortStrings(in []string) {
	sort.Strings(in)
}

// IssueByID fetches an issue by rowid. Includes soft-deleted rows; callers
// that want only live issues must filter on the returned issue's DeletedAt.
// (The destructive ladder and the idempotency-deleted path both need to see
// soft-deleted rows, which is why the filter isn't pushed into the query.)
func (d *Store) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	row := d.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, id)
	return scanIssue(row)
}

// IssueByShortID resolves a project-scoped short_id. Soft-deleted issues are
// returned only when include == IncludeDeletedYes (spec §6: used by restore,
// idempotent re-delete, purge confirmation, and idempotency-key collision
// detection). Returns ErrNotFound when no row matches the filter.
func (d *Store) IssueByShortID(ctx context.Context, projectID int64, shortID string, include db.IncludeDeleted) (db.Issue, error) {
	q := issueSelect + ` WHERE i.project_id = ? AND i.short_id = ?`
	if include == db.IncludeDeletedNo {
		q += ` AND i.deleted_at IS NULL`
	}
	row := d.QueryRowContext(ctx, q, projectID, shortID)
	return scanIssue(row)
}

// IssueByUID fetches an issue by stable UID. Soft-deleted rows are returned
// only when include == IncludeDeletedYes (spec §6 carveout, matching
// IssueByShortID). Returns ErrNotFound when no row matches the filter.
func (d *Store) IssueByUID(ctx context.Context, issueUID string, include db.IncludeDeleted) (db.Issue, error) {
	q := issueSelect + ` WHERE i.uid = ?`
	if include == db.IncludeDeletedNo {
		q += ` AND i.deleted_at IS NULL`
	}
	row := d.QueryRowContext(ctx, q, issueUID)
	return scanIssue(row)
}

// ShortIDsByUIDs returns the current short_id for each requested issue
// UID inside projectID. UIDs that don't resolve (purged, never existed,
// or live in a different project) are omitted from the result. Used by
// the audit projection to map a close-time parent UID to the parent's
// CURRENT short_id, which is stable across project-merge collision
// reshuffles even though the short_id itself is not.
func (d *Store) ShortIDsByUIDs(
	ctx context.Context, projectID int64, uids []string,
) (map[string]string, error) {
	out := map[string]string{}
	if len(uids) == 0 {
		return out, nil
	}
	const chunk = 500
	for i := 0; i < len(uids); i += chunk {
		end := i + chunk
		if end > len(uids) {
			end = len(uids)
		}
		slice := uids[i:end]
		placeholders := make([]string, len(slice))
		args := make([]any, 0, len(slice)+1)
		args = append(args, projectID)
		for j, u := range slice {
			placeholders[j] = "?"
			args = append(args, u)
		}
		q := `SELECT uid, short_id FROM issues
		      WHERE project_id = ? AND uid IN (` + strings.Join(placeholders, ",") + `)`
		rows, err := d.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("short ids by uids: %w", err)
		}
		for rows.Next() {
			var uid, sid string
			if err := rows.Scan(&uid, &sid); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan short id by uid: %w", err)
			}
			out[uid] = sid
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate short ids by uids: %w", err)
		}
		_ = rows.Close()
	}
	return out, nil
}

// IssueUIDPrefixMatch returns issues whose UID starts with prefix, ordered by
// UID for deterministic ambiguity reporting. Soft-deleted rows are returned
// only when include == IncludeDeletedYes (spec §6 carveout, matching
// IssueByUID).
func (d *Store) IssueUIDPrefixMatch(ctx context.Context, prefix string, limit int, include db.IncludeDeleted) ([]db.Issue, error) {
	if limit <= 0 {
		limit = 20
	}
	q := issueSelect + ` WHERE i.uid LIKE ? || '%'`
	if include == db.IncludeDeletedNo {
		q += ` AND i.deleted_at IS NULL`
	}
	q += ` ORDER BY i.uid ASC LIMIT ?`
	rows, err := d.QueryContext(ctx, q, prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("issue uid prefix match: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	return out, rows.Err()
}

// ListIssues returns issues in the given project, excluding soft-deleted rows.
func (d *Store) ListIssues(ctx context.Context, p db.ListIssuesParams) ([]db.Issue, error) {
	q := issueSelect + ` WHERE i.project_id = ? AND i.deleted_at IS NULL`
	args := []any{p.ProjectID}
	if p.Status != "" {
		q += ` AND i.status = ?`
		args = append(args, p.Status)
	}
	if p.Priority != nil {
		q += ` AND i.priority = ?`
		args = append(args, *p.Priority)
	}
	if p.MaxPriority != nil {
		q += ` AND i.priority IS NOT NULL AND i.priority <= ?`
		args = append(args, *p.MaxPriority)
	}
	// Apply owner filters
	if p.Unowned {
		q += ` AND i.owner IS NULL`
	} else if p.Owner != "" {
		q += ` AND i.owner = ?`
		args = append(args, p.Owner)
	}
	// Apply label filters (must have ALL these labels)
	for _, label := range p.Labels {
		q += ` AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)`
		args = append(args, strings.ToLower(label))
	}
	// Apply exclude label filters (must NOT have any of these labels)
	for _, label := range p.ExcludeLabels {
		q += ` AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)`
		args = append(args, strings.ToLower(label))
	}
	q += ` ORDER BY i.updated_at DESC, i.id DESC`
	if p.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, p.Limit)
	}
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ListAllIssues returns issues across one or every project, excluding
// soft-deleted rows. Ordering is (created_at DESC, id DESC) per #22 — a
// stable "newest first" feed across projects, distinct from the per-project
// endpoint's updated_at-DESC ordering which leads with recent activity.
func (d *Store) ListAllIssues(ctx context.Context, p db.ListAllIssuesParams) ([]db.Issue, error) {
	q := issueSelect + ` WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL`
	var args []any
	if p.ProjectID > 0 {
		q += ` AND i.project_id = ?`
		args = append(args, p.ProjectID)
	}
	if p.Status != "" {
		q += ` AND i.status = ?`
		args = append(args, p.Status)
	}
	if p.Priority != nil {
		q += ` AND i.priority = ?`
		args = append(args, *p.Priority)
	}
	if p.MaxPriority != nil {
		q += ` AND i.priority IS NOT NULL AND i.priority <= ?`
		args = append(args, *p.MaxPriority)
	}
	q += ` ORDER BY i.created_at DESC, i.id DESC`
	if p.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, p.Limit)
	}
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list all issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// CreateComment appends a comment + issue.commented event in one tx, bumping
// issues.updated_at.
func (d *Store) CreateComment(ctx context.Context, p db.CreateCommentParams) (db.Comment, db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Comment{}, db.Event{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return db.Comment{}, db.Event{}, err
	}
	if err := ensureProjectWritableTx(ctx, tx, issue.ProjectID); err != nil {
		return db.Comment{}, db.Event{}, err
	}

	commentUID, err := katauid.New()
	if err != nil {
		return db.Comment{}, db.Event{}, fmt.Errorf("generate comment uid: %w", err)
	}
	createdAt := time.Now().UTC().Format(sqliteTimeFormat)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO comments(uid, issue_id, author, body, created_at) VALUES(?, ?, ?, ?, ?)`,
		commentUID, p.IssueID, p.Author, p.Body, createdAt)
	if err != nil {
		return db.Comment{}, db.Event{}, fmt.Errorf("insert comment: %w", err)
	}
	commentID, err := res.LastInsertId()
	if err != nil {
		return db.Comment{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		createdAt, p.IssueID); err != nil {
		return db.Comment{}, db.Event{}, fmt.Errorf("touch issue: %w", err)
	}

	payloadBytes, err := json.Marshal(struct {
		CommentUID string `json:"comment_uid"`
		Author     string `json:"author"`
		Body       string `json:"body"`
		CreatedAt  string `json:"created_at"`
	}{
		CommentUID: commentUID,
		Author:     p.Author,
		Body:       p.Body,
		CreatedAt:  createdAt,
	})
	if err != nil {
		return db.Comment{}, db.Event{}, fmt.Errorf("marshal comment payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.commented",
		Actor:       p.Author,
		Payload:     string(payloadBytes),
	})
	if err != nil {
		return db.Comment{}, db.Event{}, err
	}

	if err := tx.Commit(); err != nil {
		return db.Comment{}, db.Event{}, err
	}

	var c db.Comment
	if err := d.QueryRowContext(ctx,
		`SELECT id, uid, issue_id, author, body, created_at FROM comments WHERE id = ?`,
		commentID).Scan(&c.ID, &c.UID, &c.IssueID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
		return db.Comment{}, db.Event{}, fmt.Errorf("read comment: %w", err)
	}
	return c, evt, nil
}

// CommentsByIssue returns every comment on issueID in chronological order
// (created_at, then id as a stable tiebreaker).
func (d *Store) CommentsByIssue(ctx context.Context, issueID int64) ([]db.Comment, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, uid, issue_id, author, body, created_at FROM comments WHERE issue_id = ? ORDER BY created_at ASC, id ASC`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []db.Comment
	for rows.Next() {
		var c db.Comment
		if err := rows.Scan(&c.ID, &c.UID, &c.IssueID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloseIssue sets status=closed unless already closed. The message and
// evidence are persisted on the issue.closed event payload (spec §3.3
// storage scope), not on the issue row.
//
// Returns ErrOpenChildren if the issue has open children at commit time.
// Daemon handlers run the user-friendly completeness check first for a
// good error message; this in-transaction re-check exists to close the
// race where a child link is inserted between the read-side guard and the
// close write.
func (d *Store) CloseIssue(
	ctx context.Context,
	issueID int64,
	reason, actor, message string,
	evidence []db.Evidence,
) (db.Issue, *db.Event, bool, error) {
	updated, events, changed, err := d.CloseIssueWithEvents(ctx, issueID, reason, actor, message, evidence)
	if err != nil || len(events) == 0 {
		return updated, nil, changed, err
	}
	return updated, &events[0], changed, nil
}

// CloseIssueWithEvents is CloseIssue plus generated claim audit events that
// callers must deliver after commit. The returned events are ordered by
// insertion id, with issue.closed first and generated claim audit events
// following it.
func (d *Store) CloseIssueWithEvents(
	ctx context.Context,
	issueID int64,
	reason, actor, message string,
	evidence []db.Evidence,
) (db.Issue, []db.Event, bool, error) {
	if reason == "" {
		return db.Issue{}, nil, false, fmt.Errorf("close: reason is required")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if issue.Status == "closed" {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}
	if hasOpen, err := txHasOpenChildren(ctx, tx, issue.ProjectID, issueID); err != nil {
		return db.Issue{}, nil, false, err
	} else if hasOpen {
		return db.Issue{}, nil, false, db.ErrOpenChildren
	}
	closedAt := time.Now().UTC().Format(sqliteTimeFormat)
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET status        = 'closed',
		     closed_reason = ?,
		     closed_at     = ?,
		     updated_at    = ?
		 WHERE id = ?`, reason, closedAt, closedAt, issueID); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("close: %w", err)
	}

	// Freeze the close-time parent identity onto the payload so audit
	// history survives a later reparent / remove-parent AND a
	// project-merge collision rewrite of the parent's short_id. UID is
	// the immutable identity; short_id is the close-time display value
	// kept as a fallback when the parent has since been purged and the
	// UID no longer resolves. Pointer presence distinguishes "no parent
	// at close" (non-nil empty) from "legacy event that predates these
	// fields" (nil) — the audit projection falls back to a live links
	// lookup only for the legacy case.
	parentUID, parentSID, hasParent, err := txParentIdentity(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	parentUIDForPayload, parentSIDForPayload := new(string), new(string)
	if hasParent {
		*parentUIDForPayload = parentUID
		*parentSIDForPayload = parentSID
	}
	payloadBytes, err := json.Marshal(struct {
		Reason        string        `json:"reason"`
		ClosedAt      string        `json:"closed_at"`
		Message       string        `json:"message,omitempty"`
		Evidence      []db.Evidence `json:"evidence,omitempty"`
		ParentUID     *string       `json:"parent_uid,omitempty"`
		ParentShortID *string       `json:"parent_short_id,omitempty"`
	}{
		Reason:        reason,
		ClosedAt:      closedAt,
		Message:       message,
		Evidence:      evidence,
		ParentUID:     parentUIDForPayload,
		ParentShortID: parentSIDForPayload,
	})
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("close payload: %w", err)
	}

	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.closed",
		Actor:       actor,
		Payload:     string(payloadBytes),
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	events := []db.Event{evt}
	auditEvents, err := d.annotateClaimWorkMutationTx(ctx, tx, claimWorkMutationInput{
		ProjectID:         issue.ProjectID,
		ProjectName:       projectName,
		IssueID:           issue.ID,
		IssueUID:          issue.UID,
		EventType:         "issue.closed",
		Actor:             actor,
		HolderInstanceUID: d.InstanceUID(),
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	events = append(events, auditEvents...)
	if reason == "done" && issue.RecurrenceID != nil && issue.OccurrenceKey != nil {
		if _, err := d.materializeNextTx(ctx, tx, *issue.RecurrenceID,
			*issue.OccurrenceKey, actor); err != nil {
			return db.Issue{}, nil, false, fmt.Errorf("materialize next recurrence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, events, true, nil
}

// InsertCloseThrottledEvent records a close.throttled audit event for the
// refused close. The event is attached to the refused issue (issueID) so
// audit/replay tools can render it inline with that issue's other events.
// Returns the inserted event on success.
func (d *Store) InsertCloseThrottledEvent(
	ctx context.Context, issueID int64, actor string, payload db.CloseThrottledPayload,
) (db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Event{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Event{}, err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal close.throttled payload: %w", err)
	}

	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "close.throttled",
		Actor:       actor,
		Payload:     string(payloadBytes),
	})
	if err != nil {
		return db.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.Event{}, err
	}
	return evt, nil
}

// ReopenIssue clears status=closed unless already open.
func (d *Store) ReopenIssue(
	ctx context.Context, issueID int64, actor string,
) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if issue.Status == "open" {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}
	reopenedAt := time.Now().UTC().Format(sqliteTimeFormat)
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET status        = 'open',
		     closed_reason = NULL,
		     closed_at     = NULL,
		     updated_at    = ?
		 WHERE id = ?`, reopenedAt, issueID); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("reopen: %w", err)
	}
	payloadBytes, err := json.Marshal(struct {
		ReopenedAt string `json:"reopened_at"`
		UpdatedAt  string `json:"updated_at"`
	}{ReopenedAt: reopenedAt, UpdatedAt: reopenedAt})
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("reopen payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.reopened",
		Actor:       actor,
		Payload:     string(payloadBytes),
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// EditIssue mutates title/body/owner. ErrNoFields if none are set.
func (d *Store) EditIssue(ctx context.Context, p db.EditIssueParams) (db.Issue, *db.Event, bool, error) {
	if p.Title == nil && p.Body == nil && p.Owner == nil {
		return db.Issue{}, nil, false, db.ErrNoFields
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}

	ts := nowTimestamp()
	sets, args, payload, changed, err := issueFieldUpdatePlan(issue, p.Title, p.Body, p.Owner, ts)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if !changed {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}
	sets = append([]string{`updated_at = ?`}, sets...)
	args = append([]any{ts}, args...)
	args = append(args, p.IssueID)
	// `sets` only contains string literals chosen above; user-provided values
	// are parameterized via `args`. Safe to concatenate.
	q := `UPDATE issues SET ` + joinComma(sets) + ` WHERE id = ?` // #nosec G202
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("update issue: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.updated",
		Actor:       p.Actor,
		Payload:     payload,
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, p.IssueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// nowTimestamp returns the canonical UTC millisecond timestamp string used as
// the single source for a mutation's issues.updated_at and the matching event
// payload "updated_at", so replay reproduces the directly written value instead
// of falling back to the event's independently clocked created_at.
func nowTimestamp() string {
	return time.Now().UTC().Format(sqliteTimeFormat)
}

func issueFieldUpdatePlan(issue db.Issue, title, body, owner *string, ts string) ([]string, []any, string, bool, error) {
	sets := []string{}
	args := []any{}
	payload := map[string]any{}
	if title != nil && *title != issue.Title {
		sets = append(sets, `title = ?`)
		args = append(args, *title)
		payload["title"] = *title
		payload["old_title"] = issue.Title
	}
	if body != nil && *body != issue.Body {
		sets = append(sets, `body = ?`)
		args = append(args, *body)
		payload["body"] = *body
	}
	if owner != nil {
		var newOwner *string
		if *owner != "" {
			v := *owner
			newOwner = &v
		}
		if !ownerEqual(issue.Owner, newOwner) {
			sets = append(sets, `owner = ?`)
			args = append(args, newOwner)
			if newOwner == nil {
				payload["owner"] = nil
			} else {
				payload["owner"] = *newOwner
			}
			if issue.Owner == nil {
				payload["old_owner"] = nil
			} else {
				payload["old_owner"] = *issue.Owner
			}
		}
	}
	if len(sets) == 0 {
		return nil, nil, "", false, nil
	}
	payload["updated_at"] = ts
	bs, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("marshal issue.updated payload: %w", err)
	}
	return sets, args, string(bs), true, nil
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// lookupIssueForEvent fetches the issue + its project's name for event
// snapshotting. Used inside transactions. Soft-deleted issues are excluded so
// lifecycle mutations (close/reopen/edit/comment) cannot operate on hidden
// rows; callers see ErrNotFound for both nonexistent and deleted issues.
func lookupIssueForEvent(ctx context.Context, tx *sql.Tx, issueID int64) (db.Issue, string, error) {
	const q = `
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at, p.name
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		WHERE i.id = ? AND i.deleted_at IS NULL`
	var i db.Issue
	var projectName string
	err := tx.QueryRowContext(ctx, q, issueID).
		Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status, &i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision, &i.RecurrenceID, &i.OccurrenceKey, &i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Issue{}, "", db.ErrNotFound
	}
	if err != nil {
		return db.Issue{}, "", fmt.Errorf("lookup issue: %w", err)
	}
	if err := ensureProjectWritableTx(ctx, tx, i.ProjectID); err != nil {
		return db.Issue{}, "", err
	}
	return i, projectName, nil
}

const issueSelect = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status, i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision, i.recurrence_id, i.occurrence_key, i.created_at, i.updated_at, i.closed_at, i.deleted_at FROM issues i JOIN projects p ON p.id = i.project_id`

func scanIssue(r rowScanner) (db.Issue, error) {
	var i db.Issue
	err := r.Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status, &i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision, &i.RecurrenceID, &i.OccurrenceKey, &i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Issue{}, db.ErrNotFound
	}
	if err != nil {
		return db.Issue{}, fmt.Errorf("scan issue: %w", err)
	}
	return i, nil
}

// eventInsert is the tx-internal payload used by insertEventTx.
type eventInsert struct {
	ProjectID         int64
	ProjectUID        string
	ProjectName       string
	IssueID           *int64
	IssueUID          *string
	RelatedIssueID    *int64
	RelatedIssueUID   *string
	Type              string
	Actor             string
	Payload           string
	UID               string
	OriginInstanceUID string
	HLC               *db.EventHLCTimestamp
	CreatedAt         string
	ContentHash       string
}

// UpdateOwner sets issues.owner to the new value and emits the matching
// assigned/unassigned event. newOwner == nil means unassign. No-op when the
// new value matches the current value (returns nil event, changed=false).
func (d *Store) UpdateOwner(ctx context.Context, issueID int64, newOwner *string, actor string) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	// No-op: same owner.
	if ownerEqual(issue.Owner, newOwner) {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}

	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET owner      = ?,
		     updated_at = ?
		 WHERE id = ?`, newOwner, ts, issueID); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("update owner: %w", err)
	}

	eventType := "issue.unassigned"
	ownerPayload := map[string]any{"owner": nil, "updated_at": ts}
	if newOwner != nil {
		eventType = "issue.assigned"
		ownerPayload["owner"] = *newOwner
	}
	bs, marshalErr := json.Marshal(ownerPayload)
	if marshalErr != nil {
		return db.Issue{}, nil, false, fmt.Errorf("marshal owner payload: %w", marshalErr)
	}
	payload := string(bs)
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        eventType,
		Actor:       actor,
		Payload:     payload,
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// ownerEqual returns true when two *string owners reference the same value
// (both nil = equal; nil vs non-nil = different; otherwise compare strings).
func ownerEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// ClaimOwner atomically claims an issue for the given actor. The conditional
// UPDATE ensures the claim only succeeds if the issue is unowned or owned by
// the same actor (or force is true). If a concurrent claim causes a SQLite
// busy/locked error during the UPDATE, we treat it as a conflict and return
// ErrAlreadyClaimed after fetching the current owner.
//
// Returns ErrAlreadyClaimed if the issue is already owned by a different actor
// and force is false. The ClaimResult.CurrentOwner field is set in this case.
func (d *Store) ClaimOwner(ctx context.Context, issueID int64, actor string, force bool) (db.ClaimResult, error) {
	actor = strings.TrimSpace(actor)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.ClaimResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Read current state to get previous owner and check for no-op
	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.ClaimResult{}, err
	}

	// Already owned by same actor: no-op
	if issue.Owner != nil && *issue.Owner == actor {
		if err := tx.Commit(); err != nil {
			return db.ClaimResult{}, err
		}
		return db.ClaimResult{
			Issue:         issue,
			Event:         nil,
			Changed:       false,
			PreviousOwner: nil,
		}, nil
	}

	// Store previous owner before update
	var previousOwner *string
	if issue.Owner != nil {
		prev := *issue.Owner
		previousOwner = &prev
	}

	// Conditional UPDATE: only succeeds if ownership state matches expectations.
	// The WHERE clause prevents races - if another request claimed between our
	// read and this write, zero rows will be affected.
	ts := nowTimestamp()
	var res sql.Result
	if force {
		res, err = tx.ExecContext(ctx,
			`UPDATE issues
			 SET owner      = ?,
			     updated_at = ?
			 WHERE id = ? AND deleted_at IS NULL`, actor, ts, issueID)
	} else {
		res, err = tx.ExecContext(ctx,
			`UPDATE issues
			 SET owner      = ?,
			     updated_at = ?
			 WHERE id = ? AND deleted_at IS NULL AND (owner IS NULL OR owner = ?)`, actor, ts, issueID, actor)
	}
	if err != nil {
		return db.ClaimResult{}, fmt.Errorf("update owner: %w", err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return db.ClaimResult{}, fmt.Errorf("rows affected: %w", err)
	}

	// Zero rows affected means the conditional WHERE didn't match:
	// someone else claimed the issue between our read and write.
	if rowsAffected == 0 {
		return db.ClaimResult{CurrentOwner: issue.Owner}, db.ErrAlreadyClaimed
	}

	// Re-read the updated issue for response
	issue, _, err = lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.ClaimResult{}, err
	}

	// Emit assigned event
	bs, marshalErr := json.Marshal(map[string]any{"owner": actor, "updated_at": ts})
	if marshalErr != nil {
		return db.ClaimResult{}, fmt.Errorf("marshal assigned payload: %w", marshalErr)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.assigned",
		Actor:       actor,
		Payload:     string(bs),
	})
	if err != nil {
		return db.ClaimResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return db.ClaimResult{}, err
	}

	return db.ClaimResult{
		Issue:         issue,
		Event:         &evt,
		Changed:       true,
		PreviousOwner: previousOwner,
	}, nil
}

// ReadyIssues returns open, non-deleted issues with no open `blocks` predecessor,
// ordered by updated_at DESC. limit==0 means no limit.
func (d *Store) ReadyIssues(ctx context.Context, projectID int64, limit int, filter db.ReadyIssuesFilter) ([]db.Issue, error) {
	q := issueSelect + `
		WHERE i.project_id = ? AND i.status = 'open' AND i.deleted_at IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM links l
		    JOIN issues blocker ON blocker.id = l.from_issue_id
		    WHERE l.type = 'blocks' AND l.to_issue_id = i.id
		      AND blocker.status = 'open' AND blocker.deleted_at IS NULL
		  )`
	args := []any{projectID}

	// Apply owner filters
	if filter.Unowned {
		q += ` AND i.owner IS NULL`
	} else if filter.Owner != "" {
		q += ` AND i.owner = ?`
		args = append(args, filter.Owner)
	}

	// Apply label filters (must have ALL these labels)
	for _, label := range filter.Labels {
		q += ` AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)`
		args = append(args, strings.ToLower(label))
	}

	// Apply exclude label filters (must NOT have any of these labels)
	for _, label := range filter.ExcludeLabels {
		q += ` AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)`
		args = append(args, strings.ToLower(label))
	}

	q += ` ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ready issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ReadyIssuesGlobal returns ready issues across every non-archived project,
// each paired with its project name. "Ready" matches ReadyIssues: open,
// not soft-deleted, and not blocked by an open `blocks` predecessor.
// Issues from archived projects (projects.deleted_at IS NOT NULL) are
// excluded. Ordering matches ReadyIssues so behavior is consistent.
func (d *Store) ReadyIssuesGlobal(ctx context.Context, limit int) ([]db.ReadyGlobalIssue, error) {
	// issueSelect ends with "FROM issues i JOIN projects p ON p.id = i.project_id"
	// We need to add p.name before FROM, so we build the SELECT from scratch.
	q := `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status, i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision, i.recurrence_id, i.occurrence_key, i.created_at, i.updated_at, i.closed_at, i.deleted_at, p.name AS project_name FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.status = 'open' AND i.deleted_at IS NULL
		  AND p.deleted_at IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM links l
		    JOIN issues blocker ON blocker.id = l.from_issue_id
		    WHERE l.type = 'blocks' AND l.to_issue_id = i.id
		      AND blocker.status = 'open' AND blocker.deleted_at IS NULL
		  )
		ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ready issues global: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.ReadyGlobalIssue
	for rows.Next() {
		var r db.ReadyGlobalIssue
		if err := rows.Scan(
			&r.ID, &r.UID, &r.ProjectID, &r.ProjectUID,
			&r.ShortID, &r.Title, &r.Body, &r.Status,
			&r.ClosedReason, &r.Owner, &r.Priority, &r.Author,
			&r.Metadata, &r.Revision, &r.RecurrenceID, &r.OccurrenceKey,
			&r.CreatedAt, &r.UpdatedAt, &r.ClosedAt, &r.DeletedAt,
			&r.ProjectName,
		); err != nil {
			return nil, fmt.Errorf("scan ready global issue: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *Store) insertEventTx(ctx context.Context, tx *sql.Tx, in eventInsert) (db.Event, error) {
	eventUID := in.UID
	var err error
	if eventUID == "" {
		eventUID, err = katauid.New()
		if err != nil {
			return db.Event{}, fmt.Errorf("generate event uid: %w", err)
		}
	}
	originInstanceUID := in.OriginInstanceUID
	if originInstanceUID == "" {
		originInstanceUID = d.instanceUID
	}
	now := time.Now().UTC()
	createdAt := now.Format(sqliteTimeFormat)
	if in.CreatedAt != "" {
		createdAt = in.CreatedAt
	}
	var eventHLC db.EventHLCTimestamp
	if in.HLC != nil {
		eventHLC = *in.HLC
	} else {
		eventHLC, err = nextEventHLC(ctx, tx, now)
		if err != nil {
			return db.Event{}, fmt.Errorf("next event hlc: %w", err)
		}
	}
	projectUID, projectName, err := eventProjectIdentityTx(ctx, tx, in.ProjectID, in.ProjectUID, in.ProjectName)
	if err != nil {
		return db.Event{}, err
	}
	issueUID, err := eventIssueUIDTx(ctx, tx, in.IssueID, in.IssueUID)
	if err != nil {
		return db.Event{}, err
	}
	relatedIssueUID, err := eventIssueUIDTx(ctx, tx, in.RelatedIssueID, in.RelatedIssueUID)
	if err != nil {
		return db.Event{}, err
	}
	contentHash := in.ContentHash
	if contentHash == "" {
		contentHash, err = db.EventContentHash(db.EventHashInput{
			UID:               eventUID,
			OriginInstanceUID: originInstanceUID,
			ProjectUID:        projectUID,
			ProjectName:       projectName,
			IssueUID:          issueUID,
			RelatedIssueUID:   relatedIssueUID,
			Type:              in.Type,
			Actor:             in.Actor,
			HLCPhysicalMS:     eventHLC.PhysicalMS,
			HLCCounter:        eventHLC.Counter,
			CreatedAt:         createdAt,
			Payload:           json.RawMessage(in.Payload),
		})
		if err != nil {
			return db.Event{}, fmt.Errorf("content hash: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events(
		   uid, origin_instance_uid, project_id, project_name,
		   issue_id, issue_uid, related_issue_id, related_issue_uid,
		   type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventUID, originInstanceUID,
		in.ProjectID, projectName,
		in.IssueID, stringPtrValue(issueUID),
		in.RelatedIssueID, stringPtrValue(relatedIssueUID),
		in.Type, in.Actor, in.Payload,
		eventHLC.PhysicalMS, eventHLC.Counter, contentHash, createdAt)
	if err != nil {
		return db.Event{}, fmt.Errorf("insert event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.Event{}, err
	}
	e, err := scanEvent(tx.QueryRowContext(ctx, eventSelectByID, id))
	if err != nil {
		return db.Event{}, fmt.Errorf("read event: %w", err)
	}
	return e, nil
}

type eventScanner interface {
	Scan(dest ...any) error
}

func scanEvent(scanner eventScanner) (db.Event, error) {
	var e db.Event
	err := scanner.Scan(&e.ID, &e.UID, &e.OriginInstanceUID, &e.ProjectID, &e.ProjectUID, &e.ProjectName, &e.IssueID,
		&e.IssueUID, &e.IssueShortID, &e.RelatedIssueID, &e.RelatedIssueUID, &e.RelatedIssueShortID,
		&e.Type, &e.Actor, &e.Payload, &e.HLCPhysicalMS, &e.HLCCounter, &e.ContentHash, &e.CreatedAt)
	return e, err
}

func nextEventHLC(ctx context.Context, tx *sql.Tx, now time.Time) (db.EventHLCTimestamp, error) {
	var last db.EventHLCTimestamp
	err := tx.QueryRowContext(ctx, `
		SELECT hlc_physical_ms, hlc_counter
		  FROM events
		 ORDER BY hlc_physical_ms DESC, hlc_counter DESC
		 LIMIT 1`).Scan(&last.PhysicalMS, &last.Counter)
	if errors.Is(err, sql.ErrNoRows) {
		return db.NextEventHLCValue(db.EventHLCTimestamp{}, now), nil
	}
	if err != nil {
		return db.EventHLCTimestamp{}, err
	}
	return db.NextEventHLCValue(last, now), nil
}

func eventProjectIdentityTx(ctx context.Context, tx *sql.Tx, projectID int64, projectUID, projectName string) (string, string, error) {
	if projectUID != "" && projectName != "" {
		return projectUID, projectName, nil
	}
	var storedUID, storedName string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid, name FROM projects WHERE id = ?`, projectID).
		Scan(&storedUID, &storedName); err != nil {
		return "", "", fmt.Errorf("resolve event project identity: %w", err)
	}
	if projectUID == "" {
		projectUID = storedUID
	}
	if projectName == "" {
		projectName = storedName
	}
	return projectUID, projectName, nil
}

func eventIssueUIDTx(ctx context.Context, tx *sql.Tx, issueID *int64, issueUID *string) (*string, error) {
	if issueUID != nil && *issueUID != "" {
		return issueUID, nil
	}
	if issueID == nil {
		return nil, nil
	}
	var storedUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid FROM issues WHERE id = ?`, *issueID).Scan(&storedUID); err != nil {
		return nil, fmt.Errorf("resolve event issue uid: %w", err)
	}
	return &storedUID, nil
}

// eventSelectByID reads a single event by id with the same shape EventsAfter
// and EventsInWindow produce — the issue and related_issue short_ids are
// LEFT JOINed from the live `issues` table so mutation responses (which
// scan their inserted event through this query) carry the same wire shape
// as events streamed via poll/SSE.
const eventSelectByID = `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
       e.issue_id, e.issue_uid, i.short_id, e.related_issue_id, e.related_issue_uid, ri.short_id,
       e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at
  FROM events e
  JOIN projects p ON p.id = e.project_id
  LEFT JOIN issues i ON i.project_id = e.project_id AND (i.id = e.issue_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL AND i.uid = e.issue_uid))
  LEFT JOIN issues ri ON ri.project_id = e.project_id AND (ri.id = e.related_issue_id OR (e.related_issue_id IS NULL AND e.related_issue_uid IS NOT NULL AND ri.uid = e.related_issue_uid))
 WHERE e.id = ?`

func stringPtrValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// SoftDeleteIssue sets deleted_at on the issue and emits issue.soft_deleted.
// Already-deleted issues are returned as a no-op envelope (nil event,
// changed=false). Unknown issues return ErrNotFound.
func (d *Store) SoftDeleteIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueIncludingDeleted(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if issue.DeletedAt != nil {
		// Already soft-deleted; commit so the read-side state is consistent
		// (no-op tx is harmless) and return the no-op envelope.
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}
	// Conditional UPDATE — gated on deleted_at IS NULL — closes the
	// read-then-write race: a concurrent SoftDeleteIssue between our lookup
	// and our UPDATE would otherwise let both transactions emit events.
	deletedAt := time.Now().UTC().Format(sqliteTimeFormat)
	res, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET deleted_at = ?,
		     updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`, deletedAt, deletedAt, issueID)
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("soft delete issue: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("soft delete rows affected: %w", err)
	}
	if n == 0 {
		// Lost the race — another tx soft-deleted this issue. No event.
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		updated, err := d.IssueByID(ctx, issueID)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		return updated, nil, false, nil
	}
	payload, err := json.Marshal(struct {
		DeletedAt string `json:"deleted_at"`
	}{DeletedAt: deletedAt})
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("soft delete payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.soft_deleted",
		Actor:       actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// RestoreIssue clears deleted_at and emits issue.restored. Not-deleted issues
// are returned as a no-op envelope. Unknown issues return ErrNotFound.
func (d *Store) RestoreIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueIncludingDeleted(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if issue.DeletedAt == nil {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}
	// Conditional UPDATE — gated on deleted_at IS NOT NULL — closes the
	// read-then-write race symmetric to SoftDeleteIssue.
	restoredAt := time.Now().UTC().Format(sqliteTimeFormat)
	res, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET deleted_at = NULL,
		     updated_at = ?
		 WHERE id = ? AND deleted_at IS NOT NULL`, restoredAt, issueID)
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("restore issue: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("restore rows affected: %w", err)
	}
	if n == 0 {
		// Lost the race — another tx restored this issue. No event.
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		updated, err := d.IssueByID(ctx, issueID)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		return updated, nil, false, nil
	}
	payload, err := json.Marshal(struct {
		RestoredAt string `json:"restored_at"`
		UpdatedAt  string `json:"updated_at"`
	}{RestoredAt: restoredAt, UpdatedAt: restoredAt})
	if err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("restore payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.restored",
		Actor:       actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// sqlReader is the subset of *sql.Conn / *sql.Tx used by helpers that need to
// run the same SELECT under either a connection-scoped manual transaction
// (BEGIN IMMEDIATE) or a database/sql-managed *sql.Tx.
type sqlReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// PurgeIssue runs the seven-step transaction from spec §3.5: cascade-deletes
// every dependent (events, comments, links, labels), reserves an SSE cursor by
// bumping sqlite_sequence above the deleted events' ids, writes a purge_log
// audit row, and finally removes the issues row (which fires the FTS deletion
// trigger). Uses BEGIN IMMEDIATE so the count snapshots in step 3 are stable
// against concurrent writers — no other writer can slip a comment/link/label
// in between counting and deleting.
//
// No issue.purged event is persisted; purge_log is the only audit record.
// Returns ErrNotFound if the issue does not exist (whether or not it had been
// soft-deleted first).
func (d *Store) PurgeIssue(ctx context.Context, issueID int64, actor string, reason *string) (db.PurgeLog, error) {
	conn, err := d.Conn(ctx)
	if err != nil {
		return db.PurgeLog{}, fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE TRANSACTION"); err != nil {
		return db.PurgeLog{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Use a detached context so rollback runs even if the caller's
			// ctx is already canceled — otherwise the conn may return to the
			// pool with an open tx after a mid-flight cancellation.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	issue, projectName, err := lookupIssueIncludingDeleted(ctx, conn, issueID)
	if err != nil {
		return db.PurgeLog{}, err
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, conn, issue.ProjectID); err != nil {
		return db.PurgeLog{}, err
	}

	purgeLogID, err := purgeCascade(ctx, conn, issue, projectName, actor, reason, d.instanceUID)
	if err != nil {
		return db.PurgeLog{}, err
	}

	pl, err := scanPurgeLog(ctx, conn, purgeLogID)
	if err != nil {
		return db.PurgeLog{}, fmt.Errorf("re-fetch purge_log: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return db.PurgeLog{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return pl, nil
}

// connExec is the subset of *sql.Conn / *sql.Tx that purgeCascade needs:
// both reads and writes inside a manual transaction.
type connExec interface {
	sqlReader
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// purgeCascade is steps 2-7 of PurgeIssue. It runs inside the BEGIN IMMEDIATE
// transaction held by the caller and returns the purge_log row id of the audit
// row it inserted. Split out of PurgeIssue to keep the public method's body
// readable and to bound its cyclomatic complexity.
func purgeCascade(
	ctx context.Context,
	c connExec,
	issue db.Issue,
	projectName string,
	actor string,
	reason *string,
	originInstanceUID string,
) (int64, error) {
	// Step 2: capture the events.id range about to be cascade-deleted so the
	// audit row records what the SSE reset cursor is reserving past. The
	// where clause matches per-link events (issue.linked / unlinked) via
	// issue_id / related_issue_id. Aggregated events from the PATCH path
	// (issue.links_changed) and root issue.created events on OTHER issues
	// that incidentally reference this issue in their payload are NOT
	// deleted: per Jesse's design call on kata#1, purging this issue must
	// not erase the historical context that another issue was once linked
	// to it. Mutation events that ARE about this issue (issue_id matches)
	// are still removed.
	eventsWhere, eventsArgs := purgeEventsCleanupWhere(issue)
	var minEventID, maxEventID sql.NullInt64
	if err := c.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM events WHERE `+eventsWhere,
		eventsArgs...).Scan(&minEventID, &maxEventID); err != nil {
		return 0, fmt.Errorf("scan event id range: %w", err)
	}

	// Step 3: count snapshots — stable under BEGIN IMMEDIATE.
	commentCount, err := scanCount(ctx, c,
		`SELECT count(*) FROM comments WHERE issue_id = ?`, issue.ID)
	if err != nil {
		return 0, fmt.Errorf("count comments: %w", err)
	}
	linkCount, err := scanCount(ctx, c,
		`SELECT count(*) FROM links WHERE from_issue_id = ? OR to_issue_id = ?`,
		issue.ID, issue.ID)
	if err != nil {
		return 0, fmt.Errorf("count links: %w", err)
	}
	labelCount, err := scanCount(ctx, c,
		`SELECT count(*) FROM issue_labels WHERE issue_id = ?`, issue.ID)
	if err != nil {
		return 0, fmt.Errorf("count labels: %w", err)
	}
	eventCount, err := scanCount(ctx, c,
		`SELECT count(*) FROM events WHERE `+eventsWhere, eventsArgs...)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}

	// Step 4: cascade-delete dependents. Order is bounded by foreign keys —
	// events (which can reference issues via issue_id/related_issue_id) and
	// the relationship rows (comments, links, labels) all reference issues, so
	// they must go before the issues row in step 7. Mutual ordering between
	// the four below is otherwise free.
	if _, err := c.ExecContext(ctx,
		`DELETE FROM events WHERE `+eventsWhere, eventsArgs...); err != nil {
		return 0, fmt.Errorf("delete events: %w", err)
	}
	// Detach surviving aggregated issue.links_changed events from the
	// purged issue: NULL the envelope's related_issue_id (and its UID
	// counterpart) so the FK constraint passes when the issues row is
	// deleted in step 7. The payload retains the peer's UID as an
	// orphan reference — that is the intentional preservation per
	// kata#1's design call. Iteration-16 set related_issue_id for
	// single-peer aggregated events; without this UPDATE the FK would
	// block purge.
	if _, err := c.ExecContext(ctx,
		`UPDATE events
		    SET related_issue_id  = NULL,
		        related_issue_uid = NULL
		  WHERE related_issue_id = ? AND type = 'issue.links_changed'`,
		issue.ID); err != nil {
		return 0, fmt.Errorf("detach aggregated event peer refs: %w", err)
	}
	if _, err := c.ExecContext(ctx,
		`DELETE FROM comments WHERE issue_id = ?`, issue.ID); err != nil {
		return 0, fmt.Errorf("delete comments: %w", err)
	}
	if _, err := c.ExecContext(ctx,
		`DELETE FROM links WHERE from_issue_id = ? OR to_issue_id = ?`,
		issue.ID, issue.ID); err != nil {
		return 0, fmt.Errorf("delete links: %w", err)
	}
	if _, err := c.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = ?`, issue.ID); err != nil {
		return 0, fmt.Errorf("delete labels: %w", err)
	}
	if _, err := c.ExecContext(ctx,
		`DELETE FROM pending_claim_requests WHERE issue_id = ?`, issue.ID); err != nil {
		return 0, fmt.Errorf("delete pending claim requests: %w", err)
	}
	if _, err := c.ExecContext(ctx,
		`DELETE FROM issue_claims WHERE issue_id = ?`, issue.ID); err != nil {
		return 0, fmt.Errorf("delete issue claims: %w", err)
	}

	// Step 5: reserve an SSE cursor by bumping sqlite_sequence past the
	// max events.id we just deleted. Skip when no events were attached —
	// there's nothing for subscribers to skip past.
	reservedCursor, err := reserveEventSequence(ctx, c, minEventID.Valid)
	if err != nil {
		return 0, err
	}

	purgeUID, err := katauid.New()
	if err != nil {
		return 0, fmt.Errorf("generate purge uid: %w", err)
	}
	// Step 6: write the audit row. sql.NullInt64 carries through as either
	// INTEGER or NULL; database/sql handles the marshaling. short_id is
	// snapshotted so assignShortIDIn can tombstone the slot against future
	// creates whose ULID suffix would otherwise collide.
	res, err := c.ExecContext(ctx,
		`INSERT INTO purge_log(
		   uid, origin_instance_uid,
		   project_id, purged_issue_id, issue_uid, project_uid, project_name,
		   short_id, issue_title, issue_author, comment_count, link_count, label_count,
		   event_count, events_deleted_min_id, events_deleted_max_id,
		   purge_reset_after_event_id, actor, reason)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		purgeUID, originInstanceUID,
		issue.ProjectID, issue.ID, issue.UID, issue.ProjectUID, projectName,
		issue.ShortID, issue.Title, issue.Author, commentCount, linkCount, labelCount,
		eventCount, minEventID, maxEventID, reservedCursor, actor, reason)
	if err != nil {
		return 0, fmt.Errorf("insert purge_log: %w", err)
	}
	purgeLogID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("purge_log last id: %w", err)
	}

	// Step 7: remove the issues row. The issues_ad_fts trigger fires here and
	// drops the matching FTS row.
	if _, err := c.ExecContext(ctx,
		`DELETE FROM issues WHERE id = ?`, issue.ID); err != nil {
		return 0, fmt.Errorf("delete issue: %w", err)
	}
	return purgeLogID, nil
}

// scanCount runs a `SELECT count(*) ...` statement and returns the result.
func scanCount(ctx context.Context, r sqlReader, query string, args ...any) (int64, error) {
	var n int64
	if err := r.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// reserveEventSequence advances sqlite_sequence for events past the current
// seq, returning the reserved value as a NullInt64 (Valid=true) for the
// purge_log row's purge_reset_after_event_id column. If hadEvents is false,
// returns NullInt64{} so the column stores NULL (no SSE reset needed).
func reserveEventSequence(ctx context.Context, c connExec, hadEvents bool) (sql.NullInt64, error) {
	if !hadEvents {
		return sql.NullInt64{}, nil
	}
	var seq int64
	if err := c.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&seq); err != nil {
		return sql.NullInt64{}, fmt.Errorf("read events seq: %w", err)
	}
	seq++
	if _, err := c.ExecContext(ctx,
		`UPDATE sqlite_sequence SET seq = ? WHERE name = 'events'`, seq); err != nil {
		return sql.NullInt64{}, fmt.Errorf("bump events seq: %w", err)
	}
	return sql.NullInt64{Int64: seq, Valid: true}, nil
}

// scanPurgeLog re-reads the purge_log row inserted by purgeCascade so the
// caller receives a typed PurgeLog with nullable fields decoded as *int64.
// Returns ErrNotFound when no row matches; callers in PurgeIssue see this only
// if the just-inserted row is missing (which would indicate a DB-level bug).
func scanPurgeLog(ctx context.Context, r sqlReader, id int64) (db.PurgeLog, error) {
	const q = `
		SELECT id, uid, origin_instance_uid, project_id, purged_issue_id, issue_uid, project_uid,
		       project_name, short_id, issue_title, issue_author, comment_count, link_count, label_count,
		       event_count, events_deleted_min_id, events_deleted_max_id,
		       purge_reset_after_event_id, actor, reason, purged_at
		FROM purge_log WHERE id = ?`
	var pl db.PurgeLog
	err := r.QueryRowContext(ctx, q, id).Scan(
		&pl.ID, &pl.UID, &pl.OriginInstanceUID, &pl.ProjectID, &pl.PurgedIssueID, &pl.IssueUID,
		&pl.ProjectUID, &pl.ProjectName, &pl.ShortID, &pl.IssueTitle, &pl.IssueAuthor, &pl.CommentCount,
		&pl.LinkCount, &pl.LabelCount, &pl.EventCount,
		&pl.EventsDeletedMinID, &pl.EventsDeletedMaxID,
		&pl.PurgeResetAfterEventID, &pl.Actor, &pl.Reason, &pl.PurgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PurgeLog{}, db.ErrNotFound
	}
	if err != nil {
		return db.PurgeLog{}, fmt.Errorf("scan purge_log: %w", err)
	}
	return pl, nil
}

// lookupIssueIncludingDeleted fetches an issue + its project's name for
// event snapshotting. Unlike lookupIssueForEvent (queries.go), this version
// does NOT filter out soft-deleted rows — it's the right primitive for the
// destructive ladder verbs that need to operate on deleted issues.
func lookupIssueIncludingDeleted(ctx context.Context, r sqlReader, issueID int64) (db.Issue, string, error) {
	const q = `
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at, p.name
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		WHERE i.id = ?`
	var (
		i           db.Issue
		projectName string
	)
	err := r.QueryRowContext(ctx, q, issueID).
		Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status,
			&i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision,
			&i.RecurrenceID, &i.OccurrenceKey,
			&i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Issue{}, "", db.ErrNotFound
	}
	if err != nil {
		return db.Issue{}, "", fmt.Errorf("lookup issue including deleted: %w", err)
	}
	if err := ensureProjectWritableTx(ctx, r, i.ProjectID); err != nil {
		return db.Issue{}, "", err
	}
	return i, projectName, nil
}

// purgeEventsCleanupWhere returns the SQL fragment + bound args that match
// every event the issue's purge should delete: events whose issue_id IS
// this issue, plus per-link events (issue.linked / issue.unlinked) whose
// related_issue_id pointed at this issue.
//
// Aggregated issue.links_changed events are excluded from the
// related_issue_id delete path even though iteration-16 sets
// related_issue_id for single-peer edits. Without that exclusion a
// `kata edit subject --blocks target` would lose subject's link history
// when target is purged, while the same edit batched with a second peer
// (multi-peer → NULL related_issue_id) would survive — a batch-size-
// dependent inconsistency. Per Jesse's design call on kata#1, purging
// an issue must not erase the historical context that another issue
// was once linked to it; payload-only references and single-peer
// envelope references are equally protected.
//
// Other-issue events that merely reference this issue in their payload
// (issue.created with an initial-link to this issue, issue.links_changed
// with this issue in a *_uids slice) are likewise PRESERVED.
func purgeEventsCleanupWhere(issue db.Issue) (string, []any) {
	clause := `(issue_id = ? OR (related_issue_id = ? AND type != 'issue.links_changed'))`
	args := []any{issue.ID, issue.ID}
	return clause, args
}

// EditIssueAtomic applies field updates, priority change, and link delta in
// one transaction. Either every requested mutation succeeds or none do.
//
// Events emitted (post-commit broadcast is the caller's responsibility):
//   - issue.updated  if changed of Title/Body/Owner actually changed
//   - issue.priority_set or issue.priority_cleared if priority actually changed
//   - issue.links_changed if changed link op actually changed (single aggregated)
//
// Idempotent no-ops do not emit their event.
func (d *Store) EditIssueAtomic(ctx context.Context, p db.EditIssueAtomicParams) (db.EditIssueAtomicResult, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}

	var (
		events    []db.Event
		changes   db.AtomicEditChanges
		anyChange bool
	)

	// A single timestamp for the whole atomic edit: each sub-mutation's row
	// bump and event payload share it so replay reproduces one updated_at.
	ts := nowTimestamp()

	// 1. Field changes (title/body/owner). Compare each requested value
	// against the loaded row first and skip the UPDATE + issue.updated
	// event entirely when every requested field already matches reality.
	// Without this no-op detection, a request like
	// `kata edit 1 --title "$(current title)" --remove-blocks 2` would
	// fire issue.updated and increment hook/digest activity even when
	// no field actually changed.
	sets, args, payload, fieldsChanged, err := issueFieldUpdatePlan(issue, p.Title, p.Body, p.Owner, ts)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	if fieldsChanged {
		sets = append([]string{`updated_at = ?`}, sets...)
		args = append([]any{ts}, args...)
		args = append(args, p.IssueID)
		// `sets` only contains fixed string literals; user values are bound
		// via `args`. Concatenation is safe.
		q := `UPDATE issues SET ` + joinComma(sets) + ` WHERE id = ?` // #nosec G202
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return db.EditIssueAtomicResult{}, fmt.Errorf("update issue fields: %w", err)
		}
		evt, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:   issue.ProjectID,
			ProjectName: projectName,
			IssueID:     &issue.ID,
			Type:        "issue.updated",
			Actor:       p.Actor,
			Payload:     payload,
		})
		if err != nil {
			return db.EditIssueAtomicResult{}, err
		}
		events = append(events, evt)
		anyChange = true
	}

	// 2. Priority. Same shape as the standalone UpdatePriority but inline so
	// we share the surrounding TX. Idempotent no-op when value is unchanged.
	if p.SetPriority != nil || p.ClearPriority {
		var newPrio *int64
		if !p.ClearPriority {
			newPrio = p.SetPriority
		}
		if !priorityEqual(issue.Priority, newPrio) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE issues SET priority = ?, updated_at = ? WHERE id = ?`,
				newPrio, ts, p.IssueID); err != nil {
				return db.EditIssueAtomicResult{}, fmt.Errorf("update priority: %w", err)
			}
			eventType, payload, err := priorityEventPayload(issue.Priority, newPrio, ts)
			if err != nil {
				return db.EditIssueAtomicResult{}, err
			}
			evt, err := d.insertEventTx(ctx, tx, eventInsert{
				ProjectID:   issue.ProjectID,
				ProjectName: projectName,
				IssueID:     &issue.ID,
				Type:        eventType,
				Actor:       p.Actor,
				Payload:     payload,
			})
			if err != nil {
				return db.EditIssueAtomicResult{}, err
			}
			events = append(events, evt)
			anyChange = true
		}
	}

	// 3. Link delta. Any error here rolls back the entire TX, including
	// the field/priority changes above.
	linkChanged, err := d.applyLinksDeltaTx(ctx, tx, issue, p, &changes, ts)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	if linkChanged {
		bs, err := json.Marshal(struct {
			db.AtomicEditChanges
			UpdatedAt string `json:"updated_at"`
		}{changes, ts})
		if err != nil {
			return db.EditIssueAtomicResult{}, fmt.Errorf("marshal links_changed payload: %w", err)
		}
		// When exactly one distinct peer is referenced across the entire
		// aggregated change, preserve envelope-level peer metadata so
		// consumers that route on related_issue_id / related_issue_uid
		// (the per-link issue.linked / issue.unlinked envelope shape)
		// retain peer identity. Multi-peer edits leave them NULL — the
		// payload's *_uids slices are authoritative.
		peerID, peerUID, err := singlePeerForLinksChangedTx(ctx, tx, changes)
		if err != nil {
			return db.EditIssueAtomicResult{}, err
		}
		evt, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:       issue.ProjectID,
			ProjectName:     projectName,
			IssueID:         &issue.ID,
			RelatedIssueID:  peerID,
			RelatedIssueUID: peerUID,
			Type:            "issue.links_changed",
			Actor:           p.Actor,
			Payload:         string(bs),
		})
		if err != nil {
			return db.EditIssueAtomicResult{}, err
		}
		events = append(events, evt)
		anyChange = true
	}

	if err := tx.Commit(); err != nil {
		return db.EditIssueAtomicResult{}, fmt.Errorf("commit: %w", err)
	}

	updated, err := d.IssueByID(ctx, p.IssueID)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	return db.EditIssueAtomicResult{
		Issue:     updated,
		Events:    events,
		Changes:   changes,
		AnyChange: anyChange,
	}, nil
}

// applyLinksDeltaTx is the per-TX worker that performs every link mutation.
// Returns true when at least one row in `links` was inserted or deleted.
// Touches the issue's updated_at exactly once at the end if changed link changed.
func (d *Store) applyLinksDeltaTx(ctx context.Context, tx *sql.Tx, issue db.Issue, p db.EditIssueAtomicParams, changes *db.AtomicEditChanges, ts string) (bool, error) {
	changed := false

	// set_parent: replaces an existing parent if present. No-op when the
	// existing parent already points at the requested target. Cycle check
	// rejects an edit that would create a parent loop (#1 → #2 → #1).
	if p.SetParent != nil {
		target, err := lookupIssueByNumberTx(ctx, tx, issue.ProjectID, *p.SetParent)
		if errors.Is(err, db.ErrNotFound) {
			return changed, &db.LinkTargetNotFoundError{Number: *p.SetParent}
		}
		if err != nil {
			return changed, err
		}
		if target.ID == issue.ID {
			return changed, db.ErrSelfLink
		}
		if err := assertNoParentCycleTx(ctx, tx, issue.ID, target.ID); err != nil {
			return changed, err
		}
		existing, perr := lookupParentOfTx(ctx, tx, issue.ID)
		if perr != nil && !errors.Is(perr, db.ErrNotFound) {
			return changed, perr
		}
		hasExisting := !errors.Is(perr, db.ErrNotFound)
		if !hasExisting || existing.ToIssueID != target.ID {
			recordedRemoval := false
			if hasExisting {
				// Capture the OLD parent's short_id AND uid so the change
				// payload surfaces a parent_removed entry with both forms.
				// Use the soft-delete-tolerant lookup: the peer of an
				// existing link may have been soft-deleted, but we still
				// own the link row and need its endpoint identity to
				// describe the removal.
				oldParent, lerr := lookupIssueByIDTxIncludingDeleted(ctx, tx, existing.ToIssueID)
				if lerr != nil {
					return changed, lerr
				}
				res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, existing.ID)
				if err != nil {
					return changed, fmt.Errorf("delete existing parent: %w", err)
				}
				rows, err := res.RowsAffected()
				if err != nil {
					return changed, fmt.Errorf("delete existing parent rows affected: %w", err)
				}
				// rows == 0 means a concurrent transaction already
				// removed the link we expected to delete. Don't claim
				// credit for a removal we didn't perform; just continue
				// to the insert (the end-state user wanted is still
				// reachable).
				if rows > 0 {
					oldShort := oldParent.ShortID
					oldUID := oldParent.UID
					changes.ParentRemoved = &oldShort
					changes.ParentRemovedUID = &oldUID
					recordedRemoval = true
				}
			}
			err := insertLinkRowTx(ctx, tx, issue.ProjectID, issue.ID, target.ID, "parent", p.Actor)
			switch {
			case errors.Is(err, db.ErrLinkExists):
				// A concurrent edit set the same parent we wanted —
				// idempotent no-op. If we already recorded a removal
				// above, the net change is "removed old, no new added,"
				// which is a real mutation; keep ParentRemoved. If we
				// didn't record a removal, the call is a pure no-op.
				if recordedRemoval {
					changed = true
				}
			case err != nil:
				return changed, err
			default:
				short := target.ShortID
				uid := target.UID
				changes.ParentSet = &short
				changes.ParentSetUID = &uid
				changed = true
			}
		}
	}

	// remove_parent: strict — assert must match current parent's number.
	if p.RemoveParent != nil {
		existing, perr := lookupParentOfTx(ctx, tx, issue.ID)
		if errors.Is(perr, db.ErrNotFound) {
			return changed, db.ErrParentMismatch
		}
		if perr != nil {
			return changed, perr
		}
		// Soft-delete-tolerant: the parent peer may have been soft-deleted
		// since this issue was last edited; the link row still exists and
		// the user can still ask to clean it up.
		parentIssue, err := lookupIssueByIDTxIncludingDeleted(ctx, tx, existing.ToIssueID)
		if err != nil {
			return changed, err
		}
		// RemoveParent's int64 ref is interpreted as the parent's row id
		// for now (Task 10 migrates the public param to short_id).
		if parentIssue.ID != *p.RemoveParent {
			return changed, db.ErrParentMismatch
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, existing.ID)
		if err != nil {
			return changed, fmt.Errorf("delete parent: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return changed, fmt.Errorf("delete parent rows affected: %w", err)
		}
		// rows == 0 means a concurrent edit removed the parent link we
		// thought we'd just verified. The strict assertion ("the parent
		// IS #N right now") is no longer satisfied — surface the same
		// 409 the no-parent case produces, so the user knows their view
		// of the world was stale.
		if rows == 0 {
			return changed, db.ErrParentMismatch
		}
		short := parentIssue.ShortID
		uid := parentIssue.UID
		changes.ParentRemoved = &short
		changes.ParentRemovedUID = &uid
		changed = true
	}

	// add_blocks: URL issue → N (type=blocks).
	for _, n := range p.AddBlocks {
		added, peer, err := addEdgeTx(ctx, tx, issue, p.ProjectIDFor(issue), n, "blocks", p.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlocksAdded = append(changes.BlocksAdded, peer.ShortID)
			changes.BlocksAddedUIDs = append(changes.BlocksAddedUIDs, peer.UID)
			changed = true
		}
	}
	// add_blocked_by: N → URL issue (type=blocks, reversed).
	for _, n := range p.AddBlockedBy {
		added, peer, err := addEdgeTx(ctx, tx, issue, p.ProjectIDFor(issue), n, "blocks", p.Actor, true)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlockedByAdded = append(changes.BlockedByAdded, peer.ShortID)
			changes.BlockedByAddedUIDs = append(changes.BlockedByAddedUIDs, peer.UID)
			changed = true
		}
	}
	// add_related: URL issue ↔ N (type=related, canonicalized).
	for _, n := range p.AddRelated {
		added, peer, err := addEdgeTx(ctx, tx, issue, p.ProjectIDFor(issue), n, "related", p.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.RelatedAdded = append(changes.RelatedAdded, peer.ShortID)
			changes.RelatedAddedUIDs = append(changes.RelatedAddedUIDs, peer.UID)
			changed = true
		}
	}

	// remove_*: idempotent.
	for _, n := range p.RemoveBlocks {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "blocks", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlocksRemoved = append(changes.BlocksRemoved, peer.ShortID)
			changes.BlocksRemovedUIDs = append(changes.BlocksRemovedUIDs, peer.UID)
			changed = true
		}
	}
	for _, n := range p.RemoveBlockedBy {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "blocks", true)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlockedByRemoved = append(changes.BlockedByRemoved, peer.ShortID)
			changes.BlockedByRemovedUIDs = append(changes.BlockedByRemovedUIDs, peer.UID)
			changed = true
		}
	}
	for _, n := range p.RemoveRelated {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "related", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.RelatedRemoved = append(changes.RelatedRemoved, peer.ShortID)
			changes.RelatedRemovedUIDs = append(changes.RelatedRemovedUIDs, peer.UID)
			changed = true
		}
	}

	if changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = ? WHERE id = ?`,
			ts, issue.ID); err != nil {
			return changed, fmt.Errorf("touch issue: %w", err)
		}
	}
	return changed, nil
}

// linkPeerRef captures the identity of a link's peer (UID + short_id) for
// payload emission. UIDs are canonical; short_ids are display snapshots.
type linkPeerRef struct {
	UID     string
	ShortID string
}

// addEdgeTx inserts a link of the given type within the existing TX. When
// reverseDirection is true, the URL issue becomes the link's target and the
// numbered issue becomes the source (used for blocked_by). Idempotent on
// duplicate. Self-link returns ErrSelfLink.
func addEdgeTx(ctx context.Context, tx *sql.Tx, urlIssue db.Issue, projectID, targetNum int64, linkType, actor string, reverseDirection bool) (bool, linkPeerRef, error) {
	target, err := lookupIssueByNumberTx(ctx, tx, projectID, targetNum)
	if errors.Is(err, db.ErrNotFound) {
		return false, linkPeerRef{}, &db.LinkTargetNotFoundError{Number: targetNum}
	}
	if err != nil {
		return false, linkPeerRef{}, err
	}
	if target.ID == urlIssue.ID {
		return false, linkPeerRef{}, db.ErrSelfLink
	}
	from, to := urlIssue.ID, target.ID
	if reverseDirection {
		from, to = to, from
	}
	if linkType == "related" && from > to {
		from, to = to, from
	}
	// Detect duplicate before INSERT to make the no-op path cheap and to
	// avoid relying on a UNIQUE-violation error path.
	if _, err := lookupLinkByEndpointsTx(ctx, tx, from, to, linkType); err == nil {
		return false, linkPeerRef{}, nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return false, linkPeerRef{}, err
	}
	if err := insertLinkRowTx(ctx, tx, projectID, from, to, linkType, actor); err != nil {
		// A concurrent edit may have inserted the same link between the
		// pre-insert lookup above and our INSERT. Treat that race as the
		// same idempotent no-op the lookup would have produced — the
		// resulting graph state is exactly what the caller asked for, just
		// committed by someone else first. The dedicated link endpoint
		// (used by the TUI) has the same behavior; mapping ErrLinkExists
		// to a 500 here would be a regression.
		if errors.Is(err, db.ErrLinkExists) {
			return false, linkPeerRef{}, nil
		}
		return false, linkPeerRef{}, err
	}
	return true, linkPeerRef{UID: target.UID, ShortID: target.ShortID}, nil
}

// removeEdgeTx deletes a link of the given type within the existing TX.
//
// Behavior matrix:
//   - target exists, link exists → delete the link, return (true, peer, nil)
//   - target exists, link absent → idempotent no-op, return (false, {}, nil)
//   - target does not exist (typo, never created, or hard-purged) →
//     idempotent no-op, return (false, {}, nil). The contract is "no
//     link to N"; if there's no N at all, the desired end state already
//     holds, so the request succeeds. CLI-side resolution already
//     short-circuits this for UID/prefix refs (which never reach the
//     daemon when they don't resolve); this branch covers numeric refs
//     that bypass CLI resolution.
//
// Soft-delete-tolerant: a soft-deleted target's row still exists, so its
// number resolves and the link can be removed. The lookup uses the
// includes-deleted variant so a hidden peer doesn't mask the link row.
func removeEdgeTx(ctx context.Context, tx *sql.Tx, urlIssue db.Issue, targetNum int64, linkType string, reverseDirection bool) (bool, linkPeerRef, error) {
	target, err := lookupIssueByNumberTxIncludingDeleted(ctx, tx, urlIssue.ProjectID, targetNum)
	if errors.Is(err, db.ErrNotFound) {
		return false, linkPeerRef{}, nil
	}
	if err != nil {
		return false, linkPeerRef{}, err
	}
	from, to := urlIssue.ID, target.ID
	if reverseDirection {
		from, to = to, from
	}
	if linkType == "related" && from > to {
		from, to = to, from
	}
	link, err := lookupLinkByEndpointsTx(ctx, tx, from, to, linkType)
	if errors.Is(err, db.ErrNotFound) {
		return false, linkPeerRef{}, nil
	}
	if err != nil {
		return false, linkPeerRef{}, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID)
	if err != nil {
		return false, linkPeerRef{}, fmt.Errorf("delete link: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, linkPeerRef{}, fmt.Errorf("delete link rows affected: %w", err)
	}
	// rows == 0 means a concurrent edit deleted the link between our
	// lookup and our DELETE — treat as the same idempotent no-op the
	// missing-link branch above handles. Returning true here would let
	// the caller append a phantom entry to the change payload for a
	// removal that didn't actually happen this transaction.
	if rows == 0 {
		return false, linkPeerRef{}, nil
	}
	return true, linkPeerRef{UID: target.UID, ShortID: target.ShortID}, nil
}

// insertLinkRowTx inserts one row into the `links` table within an existing
// TX. Maps the standard schema errors (duplicate, parent-already-set,
// self-link, cross-project) onto the typed sentinels.
//
// Race-window disambiguation for parent: the partial-parent UNIQUE produces
// the same error text whether the conflicting row points at the same
// target (concurrent identical insert → idempotent no-op) or at a different
// parent (real "parent already set" rejection). This mirrors the existing
// CreateLinkAndEvent path: re-query under the same TX to tell them apart
// and surface ErrLinkExists for the same-target case so callers can
// short-circuit to a no-op rather than 409 the user.
func insertLinkRowTx(ctx context.Context, tx *sql.Tx, projectID, fromID, toID int64, linkType, author string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		projectID, fromID, toID, fromID, toID, linkType, author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		if errors.Is(classified, db.ErrParentAlreadySet) && linkType == "parent" {
			var n int
			qErr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM links WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
				fromID, toID, linkType).Scan(&n)
			if qErr == nil {
				return db.ErrLinkExists
			}
		}
		return classified
	}
	return nil
}

// lookupIssueByNumberTx fetches one issue by (project_id, number) within a
// TX. Soft-deleted rows are excluded — mutations that add link rows must
// not target hidden issues. For paths that need to identify the peer of an
// existing link (remove/replace), use lookupIssueByNumberTxIncludingDeleted
// so a soft-deleted other-endpoint doesn't make link cleanup impossible.
func lookupIssueByNumberTx(ctx context.Context, tx *sql.Tx, projectID, number int64) (db.Issue, error) {
	return lookupIssueByNumberTxOpts(ctx, tx, projectID, number, false)
}

// lookupIssueByNumberTxIncludingDeleted is the soft-delete-tolerant variant
// used by remove/replace link paths.
func lookupIssueByNumberTxIncludingDeleted(ctx context.Context, tx *sql.Tx, projectID, number int64) (db.Issue, error) {
	return lookupIssueByNumberTxOpts(ctx, tx, projectID, number, true)
}

func lookupIssueByNumberTxOpts(ctx context.Context, tx *sql.Tx, projectID, number int64, includeDeleted bool) (db.Issue, error) {
	// EditIssueAtomic still takes int64 link refs (api.LinkChanges remains
	// int64 until Task 10). Until the daemon migrates to short_id refs we
	// resolve the int64 ref against the issue's row id.
	const base = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.project_id = ? AND i.id = ?`
	q := base + ` AND i.deleted_at IS NULL`
	if includeDeleted {
		q = base
	}
	row := tx.QueryRowContext(ctx, q, projectID, number)
	return scanIssue(row)
}

// lookupIssueByIDTxIncludingDeleted fetches one issue by id within a TX,
// including soft-deleted rows. Used when reading the peer of an existing
// link, where the link row is still valid even if the peer issue has
// been soft-deleted.
func lookupIssueByIDTxIncludingDeleted(ctx context.Context, tx *sql.Tx, id int64) (db.Issue, error) {
	const q = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.id = ?`
	row := tx.QueryRowContext(ctx, q, id)
	return scanIssue(row)
}

// lookupParentOfTx returns the parent link for child (or ErrNotFound) within
// a TX. Mirrors DB.ParentOf's query but uses tx.
func lookupParentOfTx(ctx context.Context, tx *sql.Tx, childIssueID int64) (db.Link, error) {
	row := tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND type = 'parent'`,
		childIssueID)
	return scanLink(row)
}

// lookupLinkByEndpointsTx finds a link row matching (from, to, type) within
// a TX. Mirrors DB.LinkByEndpoints but uses tx.
func lookupLinkByEndpointsTx(ctx context.Context, tx *sql.Tx, fromID, toID int64, linkType string) (db.Link, error) {
	row := tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
		fromID, toID, linkType)
	return scanLink(row)
}

// assertNoParentCycleTx walks up newParentID's parent chain looking for
// editingID. If found, the requested set_parent edit would create a loop;
// returns ErrParentCycle. The walk is bounded by maxDepth so a corrupted
// graph (which the schema's UNIQUE-on-from + same-project triggers should
// already prevent) cannot wedge the transaction.
//
// Runs inside the same TX as the rest of the link delta so the check sees
// changed prior mutations the same edit has staged (e.g. a remove_parent on
// the new parent, which would already be visible after that branch ran).
func assertNoParentCycleTx(ctx context.Context, tx *sql.Tx, editingID, newParentID int64) error {
	const maxDepth = 1024
	current := newParentID
	for i := 0; i < maxDepth; i++ {
		if current == editingID {
			return db.ErrParentCycle
		}
		var parent int64
		err := tx.QueryRowContext(ctx,
			`SELECT to_issue_id FROM links WHERE from_issue_id = ? AND type = 'parent'`,
			current).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // reached the root without finding editingID
		}
		if err != nil {
			return fmt.Errorf("walk parent chain: %w", err)
		}
		current = parent
	}
	return fmt.Errorf("parent chain exceeds depth limit %d (corrupted graph?)", maxDepth)
}

// singlePeerForLinksChangedTx returns the lone peer's (id, uid) when the
// aggregated changes reference exactly one distinct peer UID. Returns
// nil/nil when zero or multiple peers are involved. The lookup ignores
// soft-delete state: an aggregated event can reference a peer that was
// soft-deleted (e.g. an idempotent --remove-blocks against a now-hidden
// peer), and the envelope should still point to it.
func singlePeerForLinksChangedTx(ctx context.Context, tx *sql.Tx, c db.AtomicEditChanges) (*int64, *string, error) {
	seen := map[string]struct{}{}
	add := func(uid string) {
		if uid != "" {
			seen[uid] = struct{}{}
		}
	}
	if c.ParentSetUID != nil {
		add(*c.ParentSetUID)
	}
	if c.ParentRemovedUID != nil {
		add(*c.ParentRemovedUID)
	}
	for _, lists := range [][]string{
		c.BlocksAddedUIDs, c.BlocksRemovedUIDs,
		c.BlockedByAddedUIDs, c.BlockedByRemovedUIDs,
		c.RelatedAddedUIDs, c.RelatedRemovedUIDs,
	} {
		for _, u := range lists {
			add(u)
		}
	}
	if len(seen) != 1 {
		return nil, nil, nil
	}
	var only string
	for u := range seen {
		only = u
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid = ?`, only).Scan(&id); err != nil {
		return nil, nil, fmt.Errorf("resolve single peer uid %s: %w", only, err)
	}
	return &id, &only, nil
}

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

// CommentBodyByID returns the body of the comment with the given id.
// Used by the hooks dispatcher to resolve comment_body for
// issue.commented events at fire time.
func (d *Store) CommentBodyByID(ctx context.Context, id int64) (string, error) {
	var body string
	err := d.QueryRowContext(ctx, `SELECT body FROM comments WHERE id = ?`, id).Scan(&body)
	return body, err
}

// LatestAliasForProject returns the most-recently-seen alias for the
// project, if any. ok=false means the project has no aliases (the hook
// payload omits the alias block in that case).
func (d *Store) LatestAliasForProject(ctx context.Context, projectID int64) (db.AliasRow, bool, error) {
	var a db.AliasRow
	err := d.QueryRowContext(ctx,
		`SELECT alias_identity, alias_kind, root_path
		 FROM project_aliases WHERE project_id = ?
		 ORDER BY last_seen_at DESC LIMIT 1`, projectID).
		Scan(&a.Identity, &a.Kind, &a.RootPath)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AliasRow{}, false, nil
	}
	if err != nil {
		return db.AliasRow{}, false, err
	}
	return a, true, nil
}

// LabelsForIssue returns sorted label values for the issue (alphabetical).
// Sorting is done in SQL so the result matches what the issue.created
// payload normalizes at insert time.
func (d *Store) LabelsForIssue(ctx context.Context, issueID int64) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT label FROM issue_labels WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// sqliteTimeFormat matches the schema's strftime('%Y-%m-%dT%H:%M:%fZ', ...)
// (3 fractional-second digits, UTC). Both sides must use the same width for
// SQLite's lexicographic string comparison on created_at to be correct.
const sqliteTimeFormat = "2006-01-02T15:04:05.000Z"

// Fingerprint returns the lowercase hex SHA-256 of the canonical concatenation
// of (title, body, owner, sorted labels, sorted links, priority) per spec §3.6.
// The fingerprint is order-independent for labels and links: both are sorted
// before hashing. Owner is canonicalized as "" when nil or empty. Labels are
// alphabetized. Links are sorted by (type, to_number).
//
// Canonical byte layout (the input to SHA-256):
//
//	title=<canonical-title>\nbody=<canonical-body>\nowner=<canonical-owner>\nlabels=<csv-of-sorted-labels>\nlinks=<canonical-json>
//
// When priority is non-nil, an extra "\npriority=<N>" line is appended after
// the links line. Nil priority emits no priority line so the canonical layout
// matches pre-priority fingerprints byte-for-byte; existing idempotency events
// stored against the five-line layout continue to match.
//
// where canonical-* applies similarity.Canonical (NFC + trim + collapse internal
// whitespace, case preserved). Cross-language clients reproducing this must use
// the same line layout, sort labels alphabetically, sort links by
// (type, to_number), and emit links as the JSON shape
// `[{"type":"…","other_number":N},…]`.
//
// Label-charset assumption: labels are constrained at the API layer to
// `[a-z0-9._:-]` (see the labels CHECK constraint in schema.sql), so the `,`
// separator can never collide with a label byte. Bypassing API validation
// before calling Fingerprint may break this contract.
func Fingerprint(title, body string, owner *string, labels []string, links []db.InitialLink, priority *int64) string {
	return fingerprintCore(title, body, owner, labels, dedupeLinks(links), priority)
}

// FingerprintLegacy reproduces the pre-kata#1 hashing layout that did NOT
// dedupe links before sort + serialize. Lookup paths compute both forms so
// idempotency events written before the dedupe-in-Fingerprint change still
// match a retry under the new code. New writes always use Fingerprint
// (deduped); FingerprintLegacy is read-only at the lookup boundary.
func FingerprintLegacy(title, body string, owner *string, labels []string, links []db.InitialLink, priority *int64) string {
	// Pass links through unchanged so the canonical form preserves any
	// duplicate / Incoming=true entries the caller emitted at create time.
	return fingerprintCore(title, body, owner, labels, append([]db.InitialLink(nil), links...), priority)
}

func fingerprintCore(title, body string, owner *string, labels []string, sortedLinks []db.InitialLink, priority *int64) string {
	ownerStr := ""
	if owner != nil {
		ownerStr = *owner
	}
	sortedLabels := append([]string(nil), labels...)
	sort.Strings(sortedLabels)
	sort.Slice(sortedLinks, func(i, j int) bool {
		if sortedLinks[i].Type != sortedLinks[j].Type {
			return sortedLinks[i].Type < sortedLinks[j].Type
		}
		if sortedLinks[i].ToNumber != sortedLinks[j].ToNumber {
			return sortedLinks[i].ToNumber < sortedLinks[j].ToNumber
		}
		// Incoming is part of the sort key because (blocks, N, false) and
		// (blocks, N, true) describe distinct requests (--blocks vs
		// --blocked-by). Without this discriminator, retried creates with
		// the same idempotency key but flipped direction would silently
		// reuse the wrong issue.
		return !sortedLinks[i].Incoming && sortedLinks[j].Incoming
	})
	// Use a fixed JSON form for the links portion so cross-language clients
	// can reproduce the same bytes. Each entry is {"type":"…","other_number":N}
	// per spec §3.6, plus an optional "incoming":true tail when the link is
	// inverse-direction (blocked_by). incoming=false uses omitempty so
	// pre-Incoming fingerprints continue to match byte-for-byte for the
	// common outgoing case.
	type linkRec struct {
		Type        string `json:"type"`
		OtherNumber int64  `json:"other_number"`
		Incoming    bool   `json:"incoming,omitempty"`
	}
	linkRecs := make([]linkRec, 0, len(sortedLinks))
	for _, l := range sortedLinks {
		linkRecs = append(linkRecs, linkRec{Type: l.Type, OtherNumber: l.ToNumber, Incoming: l.Incoming})
	}
	linksJSON, _ := json.Marshal(linkRecs) // never errors on this shape

	var b strings.Builder
	b.WriteString("title=")
	b.WriteString(similarity.Canonical(title))
	b.WriteString("\nbody=")
	b.WriteString(similarity.Canonical(body))
	b.WriteString("\nowner=")
	b.WriteString(similarity.Canonical(ownerStr))
	b.WriteString("\nlabels=")
	b.WriteString(strings.Join(sortedLabels, ","))
	b.WriteString("\nlinks=")
	b.WriteString(similarity.Canonical(string(linksJSON)))
	if priority != nil {
		fmt.Fprintf(&b, "\npriority=%d", *priority)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// LookupIdempotency searches `events` for an `issue.created` row in the given
// project whose payload's `idempotency_key` equals key and whose created_at is
// at-or-after `since`. Returns nil when no match. Uses the partial index
// idx_events_idempotency declared in 0001_init.sql.
func (d *Store) LookupIdempotency(ctx context.Context, projectID int64, key string, since time.Time) (*db.IdempotencyMatch, error) {
	const q = `
		SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
		       e.issue_id, e.issue_uid,
		       e.related_issue_id, e.related_issue_uid, e.type, e.actor, e.payload,
		       e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at,
		       json_extract(e.payload, '$.idempotency_fingerprint')
		FROM events e
		JOIN projects p ON p.id = e.project_id
		WHERE e.type = 'issue.created'
		  AND e.project_id = ?
		  AND json_extract(e.payload, '$.idempotency_key') = ?
		  AND e.created_at >= ?
		ORDER BY e.id DESC
		LIMIT 1`
	row := d.QueryRowContext(ctx, q, projectID, key, since.UTC().Format(sqliteTimeFormat))

	var (
		evt db.Event
		fp  sql.NullString
	)
	err := row.Scan(&evt.ID, &evt.UID, &evt.OriginInstanceUID, &evt.ProjectID, &evt.ProjectUID, &evt.ProjectName,
		&evt.IssueID, &evt.IssueUID, &evt.RelatedIssueID, &evt.RelatedIssueUID, &evt.Type, &evt.Actor,
		&evt.Payload, &evt.HLCPhysicalMS, &evt.HLCCounter, &evt.ContentHash, &evt.CreatedAt, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup idempotency: %w", err)
	}
	if evt.IssueID == nil {
		// Defensive: an issue.created event without an issue_id is malformed.
		return nil, fmt.Errorf("idempotency match has no issue_id")
	}
	// Carveout (spec §6): idempotency-key collision detection sees the issue
	// even if it has been soft-deleted, so it can report the right mismatch.
	// IssueByID returns rows regardless of deleted_at, matching that intent.
	issue, err := d.IssueByID(ctx, *evt.IssueID)
	if err != nil {
		return nil, fmt.Errorf("idempotency match issue: %w", err)
	}
	return &db.IdempotencyMatch{
		IssueID:      issue.ID,
		IssueShortID: issue.ShortID,
		Fingerprint:  fp.String,
		Event:        evt,
	}, nil
}

// AddLabel attaches a label to an issue.
func (d *Store) AddLabel(ctx context.Context, issueID int64, label, author string) (db.IssueLabel, error) {
	if _, err := d.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
		issueID, label, author); err != nil {
		return db.IssueLabel{}, classifyLabelInsertError(err)
	}
	row := d.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`, issueID, label)
	out, err := scanLabel(row)
	if err != nil {
		return db.IssueLabel{}, fmt.Errorf("re-fetch label: %w", err)
	}
	return out, nil
}

func classifyLabelInsertError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: issue_labels.issue_id, issue_labels.label"):
		return db.ErrLabelExists
	case strings.Contains(msg, "CHECK constraint failed") &&
		(strings.Contains(msg, "length(label)") || strings.Contains(msg, "label NOT GLOB")):
		// Scoped to the two label-related CHECKs (length BETWEEN 1 AND 64
		// and the charset GLOB). Other CHECKs on the table (e.g. blank
		// author) fall through to the wrapped generic error rather than
		// being misreported as invalid labels.
		return db.ErrLabelInvalid
	}
	return fmt.Errorf("insert label: %w", err)
}

// RemoveLabel detaches a label from an issue. Returns ErrNotFound when the row
// doesn't exist (idempotent unlink semantics live in the handler).
func (d *Store) RemoveLabel(ctx context.Context, issueID int64, label string) error {
	res, err := d.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, label)
	if err != nil {
		return fmt.Errorf("delete label: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete label rows affected: %w", err)
	}
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// HasLabel reports whether (issueID, label) exists.
func (d *Store) HasLabel(ctx context.Context, issueID int64, label string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT 1 FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, label).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("has label: %w", err)
	}
	return n == 1, nil
}

// LabelByEndpoints fetches the label row for (issueID, label). Returns
// ErrNotFound when the label is not attached to the issue.
func (d *Store) LabelByEndpoints(ctx context.Context, issueID int64, label string) (db.IssueLabel, error) {
	row := d.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`,
		issueID, label)
	return scanLabel(row)
}

// LabelsByIssue returns every label attached to issueID, ordered alphabetically.
func (d *Store) LabelsByIssue(ctx context.Context, issueID int64) ([]db.IssueLabel, error) {
	rows, err := d.QueryContext(ctx,
		labelSelect+` WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.IssueLabel
	for rows.Next() {
		l, err := scanLabel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LabelCounts returns the per-label aggregate for projectID, excluding
// soft-deleted issues.
func (d *Store) LabelCounts(ctx context.Context, projectID int64) ([]db.LabelCount, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT il.label, COUNT(*) AS n
		 FROM issue_labels il
		 JOIN issues i ON i.id = il.issue_id
		 WHERE i.project_id = ? AND i.deleted_at IS NULL
		 GROUP BY il.label
		 ORDER BY n DESC, il.label ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("label counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.LabelCount
	for rows.Next() {
		var c db.LabelCount
		if err := rows.Scan(&c.Label, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddLabelAndEvent attaches a label to an issue, emits the matching
// issue.labeled event, and bumps the issue's updated_at — all in one TX.
// Returns the new label row and the event row. Typed errors (ErrLabelExists,
// ErrLabelInvalid) flow up unchanged from the underlying INSERT classification.
//
// Used by the daemon's POST /labels handler so the label insert and its event
// are atomic — there's no window where the row exists without an event.
func (d *Store) AddLabelAndEvent(ctx context.Context, issueID int64, ev db.LabelEventParams) (db.IssueLabel, db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.IssueLabel{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
		issueID, ev.Label, ev.Actor); err != nil {
		return db.IssueLabel{}, db.Event{}, classifyLabelInsertError(err)
	}

	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      ev.Label,
		"updated_at": ts,
	})
	if err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        ev.EventType,
		Actor:       ev.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.IssueLabel{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, issueID); err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("touch issue: %w", err)
	}

	// Re-fetch the inserted row INSIDE the TX so a post-commit failure
	// (context cancellation, concurrent removal) can't leave the caller with
	// a 500 after the mutation has already committed.
	out, err := scanLabel(tx.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`, issueID, ev.Label))
	if err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("re-fetch label inside tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return out, evt, nil
}

// RemoveLabelAndEvent detaches a label and emits the matching issue.unlabeled
// event in one TX. Returns ErrNotFound when the label was never attached —
// caller maps to 200 no-op envelope per spec §4.5.
func (d *Store) RemoveLabelAndEvent(ctx context.Context, issueID int64, ev db.LabelEventParams) (db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Event{}, err
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, ev.Label)
	if err != nil {
		return db.Event{}, fmt.Errorf("delete label: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Event{}, fmt.Errorf("delete label rows affected: %w", err)
	}
	if n == 0 {
		return db.Event{}, db.ErrNotFound
	}

	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      ev.Label,
		"updated_at": ts,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        ev.EventType,
		Actor:       ev.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, issueID); err != nil {
		return db.Event{}, fmt.Errorf("touch issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return evt, nil
}

const labelSelect = `SELECT issue_id, label, author, created_at FROM issue_labels`

func scanLabel(r rowScanner) (db.IssueLabel, error) {
	var l db.IssueLabel
	err := r.Scan(&l.IssueID, &l.Label, &l.Author, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueLabel{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueLabel{}, fmt.Errorf("scan label: %w", err)
	}
	return l, nil
}

// labelsByIssuesChunkSize bounds the IN-clause width per query. SQLite's
// SQLITE_LIMIT_VARIABLE_NUMBER defaults to 32766 in modern builds and as
// low as 999 in older ones; 500 stays comfortably under both with one
// extra parameter slot for project_id. Chunking trades one query for
// ceil(N/500) queries on large projects in exchange for safety against
// 500-class errors on list pages with limit=0 / unbounded results.
const labelsByIssuesChunkSize = 500

// LabelsByIssues returns a map of issueID → []label for every issue in
// issueIDs that belongs to projectID. Labels per issue are sorted
// alphabetically; issues with no labels are absent from the map (callers
// should treat a missing key as "no labels"). Empty input short-circuits
// without a SQL roundtrip.
//
// Constrained by both project_id (via JOIN through issues) and id IN (...)
// for cross-project safety: a caller passing an issueID that belongs to a
// different project gets no rows for that ID rather than leaking labels
// across projects. The issue_labels table itself has no project_id
// column (see schema.sql) — projection has to go through
// issues.project_id.
//
// Chunked into groups of labelsByIssuesChunkSize to stay under SQLite's
// bound-parameter limit (roborev job 246). The list endpoint allows
// limit=0 / unbounded results, so callers can pass arbitrarily many IDs;
// the previous single-shot IN clause turned a >999-issue list page into
// a 500 on builds with the older SQLITE_LIMIT_VARIABLE_NUMBER default.
// Per-issue ordering is preserved by sorting on (issue_id, label) within
// each chunk and by appending into the same map across chunks.
func (d *Store) LabelsByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += labelsByIssuesChunkSize {
		end := i + labelsByIssuesChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendLabelsForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// appendLabelsForChunk runs the LabelsByIssues query for one chunk of
// issue IDs and merges results into out. Extracted to keep the chunking
// loop readable and to bound function complexity per the project's
// 8-cyclomatic / 100-line limits.
func (d *Store) appendLabelsForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]string,
) error {
	placeholders := make([]string, len(chunk))
	args := make([]interface{}, 0, len(chunk)+1)
	args = append(args, projectID)
	for i, id := range chunk {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `SELECT il.issue_id, il.label
	          FROM issue_labels il
	          JOIN issues i ON i.id = il.issue_id
	          WHERE i.project_id = ?
	            AND il.issue_id IN (` + strings.Join(placeholders, ",") + `)
	          ORDER BY il.issue_id ASC, il.label ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("labels by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			issueID int64
			label   string
		)
		if err := rows.Scan(&issueID, &label); err != nil {
			return fmt.Errorf("scan labels by issues: %w", err)
		}
		out[issueID] = append(out[issueID], label)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate labels by issues: %w", err)
	}
	return nil
}

// CreateLink inserts a links row. Distinct error types let the caller emit
// the right wire status without parsing SQLite messages.
func (d *Store) CreateLink(ctx context.Context, p db.CreateLinkParams) (db.Link, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		p.ProjectID, p.FromIssueID, p.ToIssueID, p.FromIssueID, p.ToIssueID, p.Type, p.Author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		// SQLite may report the partial-parent index violation as a bare
		// `links.from_issue_id` UNIQUE failure, which classifies to
		// ErrParentAlreadySet. For an exact-duplicate parent link the
		// caller-facing semantic is "already linked" (200 no-op), not
		// "different parent set" (409 conflict). Disambiguate by re-querying.
		if errors.Is(classified, db.ErrParentAlreadySet) && p.Type == "parent" {
			if _, lookupErr := d.LinkByEndpoints(ctx, p.FromIssueID, p.ToIssueID, "parent"); lookupErr == nil {
				return db.Link{}, db.ErrLinkExists
			}
		}
		return db.Link{}, classified
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.Link{}, fmt.Errorf("last insert id: %w", err)
	}
	return d.LinkByID(ctx, id)
}

// classifyLinkInsertError maps SQLite constraint failures to typed errors so
// the handler can choose the right HTTP status without string-matching.
//
// Order matters: the triple-UNIQUE check must run before the partial-parent
// check because both messages start with "links.from_issue_id". The triple is
// distinguishable by the trailing column list; once that case is rejected,
// any remaining "links.from_issue_id" UNIQUE error must be the partial index
// on (from_issue_id) WHERE type='parent'. modernc.org/sqlite's error text for
// partial-index violations names only the indexed column, not the WHERE
// clause — see TestCreateLink_SecondParentIsErrParentAlreadySet.
func classifyLinkInsertError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: links.from_issue_id, links.to_issue_id, links.type"):
		return db.ErrLinkExists
	case strings.Contains(msg, "UNIQUE constraint failed: links.from_issue_id"):
		return db.ErrParentAlreadySet
	case strings.Contains(msg, "CHECK constraint failed") &&
		strings.Contains(msg, "from_issue_id <> to_issue_id"):
		return db.ErrSelfLink
	case strings.Contains(msg, "cross-project links are not allowed"):
		return db.ErrCrossProjectLink
	}
	return fmt.Errorf("insert link: %w", err)
}

// LinkByID fetches a link by rowid.
func (d *Store) LinkByID(ctx context.Context, id int64) (db.Link, error) {
	row := d.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, id)
	return scanLink(row)
}

// LinkByEndpoints fetches the link for a (from, to, type) triple.
func (d *Store) LinkByEndpoints(ctx context.Context, fromIssueID, toIssueID int64, linkType string) (db.Link, error) {
	row := d.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
		fromIssueID, toIssueID, linkType)
	return scanLink(row)
}

// ParentOf returns the parent link for childIssueID (one-parent invariant).
// Returns ErrNotFound when no parent is set.
func (d *Store) ParentOf(ctx context.Context, childIssueID int64) (db.Link, error) {
	row := d.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND type = 'parent'`,
		childIssueID)
	return scanLink(row)
}

const relationshipChunkSize = labelsByIssuesChunkSize

// ParentShortIDsByIssues returns child issue ID -> parent short_id for
// parent links inside projectID. Used by the audit handler to render and
// filter close rows by parent ref.
func (d *Store) ParentShortIDsByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64]string, error) {
	out := map[int64]string{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		placeholders, args := relationshipChunkPlaceholders(projectID, issueIDs[i:end])
		query := `SELECT l.from_issue_id, parent.short_id
		          FROM links l
		          JOIN issues child  ON child.id  = l.from_issue_id
		          JOIN issues parent ON parent.id = l.to_issue_id
		          WHERE l.project_id = ?
		            AND child.project_id = ?
		            AND parent.project_id = ?
		            AND l.type = 'parent'
		            AND l.from_issue_id IN (` + placeholders + `)`
		rows, err := d.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("parent short ids by issues: %w", err)
		}
		if err := scanParentShortIDs(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanParentShortIDs(rows *sql.Rows, out map[int64]string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var childID int64
		var parentShortID string
		if err := rows.Scan(&childID, &parentShortID); err != nil {
			return fmt.Errorf("scan parent short id: %w", err)
		}
		out[childID] = parentShortID
	}
	return rows.Err()
}

// ParentNumbersByIssues returns child issue ID -> parent issue id for
// parent links inside projectID. Despite the name (transitional), the map
// value is the parent's rowid, not a user-facing number; downstream code
// resolves it to a LinkPeer.
func (d *Store) ParentNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendParentNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendParentNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	// Result references the parent issue's id while callers transition off
	// of `number` (Tasks 11/13/14). The map shape is preserved so downstream
	// list/show code keeps compiling against int64 keys until it migrates.
	query := `SELECT l.from_issue_id, parent.id
	          FROM links l
	          JOIN issues child ON child.id = l.from_issue_id
	          JOIN issues parent ON parent.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND child.project_id = ?
	            AND parent.project_id = ?
	            AND l.type = 'parent'
	            AND l.from_issue_id IN (` + placeholders + `)
	          ORDER BY l.from_issue_id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("parent numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var childID, parentNumber int64
		if err := rows.Scan(&childID, &parentNumber); err != nil {
			return fmt.Errorf("scan parent numbers by issues: %w", err)
		}
		out[childID] = parentNumber
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate parent numbers by issues: %w", err)
	}
	return nil
}

// BlockNumbersByIssues returns issue ID -> issue numbers directly blocked by
// that issue for outgoing "blocks" links inside projectID.
func (d *Store) BlockNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendBlockNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendBlockNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	query := `SELECT l.from_issue_id, blocked.id
	          FROM links l
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND blocker.project_id = ?
	            AND blocked.project_id = ?
	            AND l.type = 'blocks'
	            AND blocked.deleted_at IS NULL
	            AND l.from_issue_id IN (` + placeholders + `)
	          ORDER BY l.from_issue_id ASC, blocked.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("block numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blockerID, blockedNumber int64
		if err := rows.Scan(&blockerID, &blockedNumber); err != nil {
			return fmt.Errorf("scan block numbers by issues: %w", err)
		}
		out[blockerID] = append(out[blockerID], blockedNumber)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate block numbers by issues: %w", err)
	}
	return nil
}

// BlockedByNumbersByIssues returns issue ID -> issue numbers that block
// that issue. Inverse of BlockNumbersByIssues: for each issue X, the
// returned numbers are the issues whose outgoing `blocks` link points
// at X. Used by `kata list --json` to surface every relationship type
// per row, not just outgoing blocks.
func (d *Store) BlockedByNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendBlockedByNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendBlockedByNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	query := `SELECT l.to_issue_id, blocker.id
	          FROM links l
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND blocker.project_id = ?
	            AND blocked.project_id = ?
	            AND l.type = 'blocks'
	            AND blocker.deleted_at IS NULL
	            AND l.to_issue_id IN (` + placeholders + `)
	          ORDER BY l.to_issue_id ASC, blocker.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("blocked-by numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blockedID, blockerNumber int64
		if err := rows.Scan(&blockedID, &blockerNumber); err != nil {
			return fmt.Errorf("scan blocked-by numbers by issues: %w", err)
		}
		out[blockedID] = append(out[blockedID], blockerNumber)
	}
	return rows.Err()
}

// RelatedNumbersByIssues returns issue ID -> issue numbers symmetrically
// related to that issue. Related links are stored canonically as (from <
// to), so for any viewer X the peers may sit on either side; the query
// projects both directions.
func (d *Store) RelatedNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendRelatedNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendRelatedNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	// Project both directions so a viewer on either canonical end sees
	// the other endpoint. Live-only join on the peer side mirrors what
	// the blocks queries do for soft-delete tolerance.
	query := `SELECT viewer_id, peer_number FROM (
	            SELECT l.from_issue_id AS viewer_id, peer.id AS peer_number
	              FROM links l
	              JOIN issues self ON self.id = l.from_issue_id
	              JOIN issues peer ON peer.id = l.to_issue_id
	             WHERE l.project_id = ?
	               AND self.project_id = ?
	               AND peer.project_id = ?
	               AND l.type = 'related'
	               AND peer.deleted_at IS NULL
	               AND l.from_issue_id IN (` + placeholders + `)
	            UNION ALL
	            SELECT l.to_issue_id AS viewer_id, peer.id AS peer_number
	              FROM links l
	              JOIN issues self ON self.id = l.to_issue_id
	              JOIN issues peer ON peer.id = l.from_issue_id
	             WHERE l.project_id = ?
	               AND self.project_id = ?
	               AND peer.project_id = ?
	               AND l.type = 'related'
	               AND peer.deleted_at IS NULL
	               AND l.to_issue_id IN (` + placeholders + `)
	          ) ORDER BY viewer_id ASC, peer_number ASC`
	// Each chunk's args are reused for both halves of the UNION.
	combined := make([]any, 0, len(args)*2)
	combined = append(combined, args...)
	combined = append(combined, args...)
	rows, err := d.QueryContext(ctx, query, combined...)
	if err != nil {
		return fmt.Errorf("related numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var viewerID, peerNumber int64
		if err := rows.Scan(&viewerID, &peerNumber); err != nil {
			return fmt.Errorf("scan related numbers by issues: %w", err)
		}
		out[viewerID] = append(out[viewerID], peerNumber)
	}
	return rows.Err()
}

// ChildCountsByParents returns direct-child open/total counts keyed by parent
// issue ID inside projectID.
func (d *Store) ChildCountsByParents(
	ctx context.Context, projectID int64, parentIssueIDs []int64,
) (map[int64]db.ChildCounts, error) {
	out := map[int64]db.ChildCounts{}
	if len(parentIssueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(parentIssueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(parentIssueIDs) {
			end = len(parentIssueIDs)
		}
		if err := d.appendChildCountsForChunk(ctx, projectID, parentIssueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// txHasOpenChildren reports whether parentIssueID has any non-deleted,
// non-closed children when run inside the close transaction. The daemon
// handler runs the user-friendly OpenChildrenOf-backed check first; this
// closes the race between that read and the close write by re-checking
// inside the same write transaction.
func txHasOpenChildren(ctx context.Context, tx *sql.Tx, projectID, parentIssueID int64) (bool, error) {
	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM links l
		 JOIN issues child ON child.id = l.from_issue_id
		 WHERE l.project_id = ?
		   AND child.project_id = ?
		   AND l.type = 'parent'
		   AND l.to_issue_id = ?
		   AND child.status = 'open'
		   AND child.deleted_at IS NULL`,
		projectID, projectID, parentIssueID).Scan(&total); err != nil {
		return false, fmt.Errorf("open children check: %w", err)
	}
	return total > 0, nil
}

// txParentIdentity returns the parent UID and short_id for childIssueID
// at the moment of the close transaction. UID is the stable identity
// (immutable across project merges and federation reshuffles); short_id
// is the close-time display value. ok=false signals "no parent set" so
// CloseIssue can write empty markers into the payload rather than
// dropping the fields entirely (the audit projection uses field
// presence to distinguish "no parent at close" from "legacy event that
// predates this freezing").
func txParentIdentity(ctx context.Context, tx *sql.Tx, childIssueID int64) (uid, shortID string, ok bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT parent.uid, parent.short_id
		 FROM links l
		 JOIN issues parent ON parent.id = l.to_issue_id
		 WHERE l.from_issue_id = ? AND l.type = 'parent'`,
		childIssueID).Scan(&uid, &shortID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("close-time parent lookup: %w", err)
	}
	return uid, shortID, true, nil
}

// OpenChildrenOf returns up to limit non-deleted, non-closed children of
// parentIssueID, plus the total open-children count. Used by the parent-
// close completeness check: the truncated slice feeds the error listing,
// and the full count drives the "(N more)" suffix.
func (d *Store) OpenChildrenOf(
	ctx context.Context, projectID, parentIssueID int64, limit int,
) ([]db.Issue, int, error) {
	var total int
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM links l
		 JOIN issues child ON child.id = l.from_issue_id
		 WHERE l.project_id = ?
		   AND child.project_id = ?
		   AND l.type = 'parent'
		   AND l.to_issue_id = ?
		   AND child.status = 'open'
		   AND child.deleted_at IS NULL`,
		projectID, projectID, parentIssueID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("open children count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := d.QueryContext(ctx, issueSelect+`
		JOIN links l ON l.from_issue_id = i.id
		WHERE l.project_id = ?
		  AND i.project_id = ?
		  AND l.type = 'parent'
		  AND l.to_issue_id = ?
		  AND i.status = 'open'
		  AND i.deleted_at IS NULL
		ORDER BY i.created_at ASC
		LIMIT ?`,
		projectID, projectID, parentIssueID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("open children: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate open children: %w", err)
	}
	return out, total, nil
}

// ChildrenOfIssue returns direct, non-deleted children for parentIssueID in
// the same order as ListIssues.
func (d *Store) ChildrenOfIssue(ctx context.Context, projectID, parentIssueID int64) ([]db.Issue, error) {
	query := issueSelect + `
		JOIN links l ON l.from_issue_id = i.id
		JOIN issues parent ON parent.id = l.to_issue_id
		WHERE l.project_id = ?
		  AND i.project_id = ?
		  AND parent.project_id = ?
		  AND l.type = 'parent'
		  AND l.to_issue_id = ?
		  AND i.deleted_at IS NULL
		ORDER BY i.updated_at DESC, i.id DESC`
	rows, err := d.QueryContext(ctx, query, projectID, projectID, projectID, parentIssueID)
	if err != nil {
		return nil, fmt.Errorf("children of issue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate children of issue: %w", err)
	}
	return out, nil
}

func (d *Store) appendChildCountsForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64]db.ChildCounts,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	query := `SELECT l.to_issue_id,
	                 SUM(CASE WHEN child.status = 'open' THEN 1 ELSE 0 END) AS open_count,
	                 COUNT(*) AS total_count
	          FROM links l
	          JOIN issues child ON child.id = l.from_issue_id
	          JOIN issues parent ON parent.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND child.project_id = ?
	            AND parent.project_id = ?
	            AND l.type = 'parent'
	            AND child.deleted_at IS NULL
	            AND l.to_issue_id IN (` + placeholders + `)
	          GROUP BY l.to_issue_id
	          ORDER BY l.to_issue_id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("child counts by parents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var parentID int64
		var counts db.ChildCounts
		if err := rows.Scan(&parentID, &counts.Open, &counts.Total); err != nil {
			return fmt.Errorf("scan child counts by parents: %w", err)
		}
		out[parentID] = counts
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate child counts by parents: %w", err)
	}
	return nil
}

func relationshipChunkPlaceholders(projectID int64, chunk []int64) (string, []any) {
	placeholders := make([]string, len(chunk))
	args := make([]any, 0, len(chunk)+3)
	args = append(args, projectID, projectID, projectID)
	for i, id := range chunk {
		placeholders[i] = "?"
		args = append(args, id)
	}
	return strings.Join(placeholders, ","), args
}

// LinksByIssue returns every link involving issueID (either endpoint), ordered
// by id ASC. Used to build the show-issue response and to back the
// list-then-delete flow used by `kata edit --remove-*`.
func (d *Store) LinksByIssue(ctx context.Context, issueID int64) ([]db.Link, error) {
	rows, err := d.QueryContext(ctx,
		linkSelect+` WHERE from_issue_id = ? OR to_issue_id = ? ORDER BY id ASC`,
		issueID, issueID)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLinkByID removes a links row. Returns ErrNotFound when no row exists.
func (d *Store) DeleteLinkByID(ctx context.Context, linkID int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, linkID)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete link rows affected: %w", err)
	}
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

const linkSelect = `SELECT id, project_id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at FROM links`

func scanLink(r rowScanner) (db.Link, error) {
	var l db.Link
	err := r.Scan(&l.ID, &l.ProjectID, &l.FromIssueID, &l.FromIssueUID, &l.ToIssueID, &l.ToIssueUID, &l.Type, &l.Author, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Link{}, db.ErrNotFound
	}
	if err != nil {
		return db.Link{}, fmt.Errorf("scan link: %w", err)
	}
	return l, nil
}

// CreateLinkAndEvent inserts a link, emits the matching issue.linked event,
// and bumps the URL issue's updated_at — all in one TX. Returns the new link
// and the event row. Typed errors (ErrLinkExists, ErrParentAlreadySet,
// ErrSelfLink, ErrCrossProjectLink) flow up unchanged from the underlying
// INSERT classification.
//
// The DB-layer methods CreateLinkAndEvent and DeleteLinkAndEvent split "the
// link's storage endpoints" (from_issue_id/to_issue_id, possibly canonicalized
// for related) from "the issue the user acted on" (the URL ref, which
// determines events.issue_id and the updated_at bump).
//
// Used by the daemon's POST /links handler so the link insert and its event
// are atomic — there's no window where the row exists without an event.
//
// Storage endpoints come from p (canonicalized for related when fromID > toID
// at the call site); event attribution comes from ev. For parent/blocks the
// two coincide; for related they may differ when canonicalization swapped.
func (d *Store) CreateLinkAndEvent(ctx context.Context, p db.CreateLinkParams, ev db.LinkEventParams) (db.Link, db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, projectName, err := lookupIssueForEvent(ctx, tx, ev.EventIssueID)
	if err != nil {
		return db.Link{}, db.Event{}, err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		p.ProjectID, p.FromIssueID, p.ToIssueID, p.FromIssueID, p.ToIssueID, p.Type, p.Author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		// Same exact-duplicate-parent disambiguation as the non-TX CreateLink:
		// the partial-parent UNIQUE index produces the same error text whether
		// it's a different parent (409) or the exact same parent (200 no-op).
		// Re-query to tell them apart inside the same TX.
		if errors.Is(classified, db.ErrParentAlreadySet) && p.Type == "parent" {
			var n int
			qErr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM links WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
				p.FromIssueID, p.ToIssueID, p.Type).Scan(&n)
			if qErr == nil {
				return db.Link{}, db.Event{}, db.ErrLinkExists
			}
		}
		return db.Link{}, db.Event{}, classified
	}
	linkID, err := res.LastInsertId()
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("last insert id: %w", err)
	}

	// related_issue_id is the OTHER endpoint of the link (not the URL issue).
	// When the URL issue is one of the link's endpoints, pick the opposite;
	// otherwise default to the link's to_issue_id.
	relatedID := p.ToIssueID
	if relatedID == ev.EventIssueID {
		relatedID = p.FromIssueID
	}
	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]any{
		"link_id":       linkID,
		"type":          p.Type,
		"from_short_id": ev.FromShortID,
		"from_uid":      ev.FromUID,
		"to_short_id":   ev.ToShortID,
		"to_uid":        ev.ToUID,
		"updated_at":    ts,
	})
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("marshal link payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:      p.ProjectID,
		ProjectName:    projectName,
		IssueID:        &ev.EventIssueID,
		RelatedIssueID: &relatedID,
		Type:           ev.EventType,
		Actor:          ev.Actor,
		Payload:        string(payload),
	})
	if err != nil {
		return db.Link{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, ev.EventIssueID); err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("touch issue: %w", err)
	}

	// Re-fetch the inserted row INSIDE the TX so a post-commit failure
	// (context cancellation, concurrent deletion) can't leave the caller with
	// a 500 after the mutation has already committed.
	link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, linkID))
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("re-fetch link inside tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return link, evt, nil
}

// DeleteLinkAndEvent deletes a link and emits the matching issue.unlinked
// event in one TX. The link to delete comes from the link argument; event
// attribution (events.issue_id, updated_at bump, payload
// from_short_id/to_short_id/uid) comes from ev. Returns ErrNotFound if the
// link is already gone — caller maps to 200 no-op envelope per spec §4.5.
func (d *Store) DeleteLinkAndEvent(ctx context.Context, link db.Link, ev db.LinkEventParams) (db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, projectName, err := lookupIssueForEvent(ctx, tx, ev.EventIssueID)
	if err != nil {
		return db.Event{}, err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID)
	if err != nil {
		return db.Event{}, fmt.Errorf("delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Event{}, fmt.Errorf("delete link rows affected: %w", err)
	}
	if n == 0 {
		return db.Event{}, db.ErrNotFound
	}
	relatedID := link.ToIssueID
	if relatedID == ev.EventIssueID {
		relatedID = link.FromIssueID
	}
	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]any{
		"link_id":       link.ID,
		"type":          link.Type,
		"from_short_id": ev.FromShortID,
		"from_uid":      ev.FromUID,
		"to_short_id":   ev.ToShortID,
		"to_uid":        ev.ToUID,
		"updated_at":    ts,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal unlink payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:      link.ProjectID,
		ProjectName:    projectName,
		IssueID:        &ev.EventIssueID,
		RelatedIssueID: &relatedID,
		Type:           ev.EventType,
		Actor:          ev.Actor,
		Payload:        string(payload),
	})
	if err != nil {
		return db.Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, ev.EventIssueID); err != nil {
		return db.Event{}, fmt.Errorf("touch issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return evt, nil
}

// MoveIssueProject moves an issue from one project to another within the same
// database, allocating a fresh short_id in the target project and emitting an
// issue.moved event. It refuses if:
//   - source and target projects are the same
//   - IfMatchRev does not match the current revision (RevisionConflictError)
//   - the issue belongs to a recurrence series (RecurrencePinnedError)
//   - any link is anchored on the issue (CrossProjectLinksError)
func (d *Store) MoveIssueProject(ctx context.Context, in db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	var out db.MoveIssueProjectOut
	if in.FromProjectID == in.ToProjectID {
		return out, fmt.Errorf("source and target projects are the same")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curRev         int64
		curShortID     string
		recurrenceID   *int64
		issueUID       string
		fromProjectUID string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT i.revision, i.short_id, i.recurrence_id, i.uid, p.uid
		  FROM issues i JOIN projects p ON p.id = i.project_id
		 WHERE i.id = ? AND i.project_id = ? AND i.deleted_at IS NULL`,
		in.IssueID, in.FromProjectID,
	).Scan(&curRev, &curShortID, &recurrenceID, &issueUID, &fromProjectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("issue %d not in project %d", in.IssueID, in.FromProjectID)
		}
		return out, err
	}
	if err := ensureFederatedMoveAllowedTx(ctx, tx, in.FromProjectID, in.ToProjectID); err != nil {
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.FromProjectID); err != nil {
		return out, err
	}
	if in.IfMatchRev != curRev {
		return out, &db.RevisionConflictError{CurrentRevision: curRev}
	}
	if recurrenceID != nil {
		return out, &db.RecurrencePinnedError{}
	}

	blockers, err := d.findLinksTx(ctx, tx, in.IssueID)
	if err != nil {
		return out, err
	}
	if len(blockers) > 0 {
		return out, &db.CrossProjectLinksError{Blockers: blockers}
	}

	var (
		toProjectUID  string
		toProjectName string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT uid, name FROM projects WHERE id = ? AND deleted_at IS NULL`,
		in.ToProjectID,
	).Scan(&toProjectUID, &toProjectName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("target project %d not found", in.ToProjectID)
		}
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.ToProjectID); err != nil {
		return out, err
	}

	newShortID, err := assignShortIDIn(ctx, tx,
		[]int64{in.ToProjectID}, issueUID, shortid.MinLength,
	)
	if err != nil {
		return out, fmt.Errorf("allocate short_id in target: %w", err)
	}

	newRev := curRev + 1
	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx, `
		UPDATE issues
		   SET project_id = ?,
		       short_id   = ?,
		       revision   = ?,
		       updated_at = ?
		 WHERE id = ?`,
		in.ToProjectID, newShortID, newRev, ts, in.IssueID,
	); err != nil {
		return out, err
	}

	// Rehome import_mappings rows for the moved issue. The UNIQUE constraint on
	// import_mappings is (source, external_id, object_type, project_id). If the
	// target project already has a row for the same (source, external_id,
	// object_type), the UPDATE would violate UNIQUE — collect the colliding IDs
	// and delete them first (the target mapping is already authoritative).
	type collisionKey struct {
		source, externalID, objectType string
	}
	collisionRows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.source, m.external_id, m.object_type
		  FROM import_mappings m
		 WHERE m.issue_id = ? AND m.project_id = ?
		   AND EXISTS (
		       SELECT 1 FROM import_mappings t
		        WHERE t.project_id  = ?
		          AND t.source      = m.source
		          AND t.external_id = m.external_id
		          AND t.object_type = m.object_type
		   )`,
		in.IssueID, in.FromProjectID, in.ToProjectID,
	)
	if err != nil {
		return out, fmt.Errorf("find colliding import_mappings: %w", err)
	}
	var collidingIDs []int64
	for collisionRows.Next() {
		var id int64
		var k collisionKey
		if err := collisionRows.Scan(&id, &k.source, &k.externalID, &k.objectType); err != nil {
			_ = collisionRows.Close()
			return out, fmt.Errorf("scan colliding import_mappings: %w", err)
		}
		collidingIDs = append(collidingIDs, id)
	}
	if err := collisionRows.Close(); err != nil {
		return out, fmt.Errorf("close collision rows: %w", err)
	}
	if err := collisionRows.Err(); err != nil {
		return out, fmt.Errorf("iterate collision rows: %w", err)
	}
	for _, id := range collidingIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id = ?`, id); err != nil {
			return out, fmt.Errorf("drop colliding import_mapping %d: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE import_mappings
		   SET project_id = ?
		 WHERE issue_id = ? AND project_id = ?`,
		in.ToProjectID, in.IssueID, in.FromProjectID,
	); err != nil {
		return out, fmt.Errorf("rehome import_mappings: %w", err)
	}

	payload, _ := json.Marshal(map[string]string{
		"issue_uid":        issueUID,
		"from_project_uid": fromProjectUID,
		"from_short_id":    curShortID,
		"to_project_uid":   toProjectUID,
		"to_short_id":      newShortID,
		"updated_at":       ts,
	})
	ev, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   in.ToProjectID,
		ProjectName: toProjectName,
		IssueID:     &in.IssueID,
		IssueUID:    &issueUID,
		Type:        "issue.moved",
		Actor:       in.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return out, err
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}

	issue, err := d.IssueByID(ctx, in.IssueID)
	if err != nil {
		return out, err
	}
	out.Issue = issue
	out.EventID = ev.ID
	out.NewShortID = newShortID
	out.NewRevision = newRev
	return out, nil
}

// findLinksTx returns all links involving issueID (as either endpoint),
// used to detect anchored links that would become cross-project after a move.
func (d *Store) findLinksTx(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.LinkBlocker, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT l.id, l.type,
		       CASE WHEN l.from_issue_id = ? THEN l.to_issue_uid ELSE l.from_issue_uid END AS peer_uid
		  FROM links l
		 WHERE l.from_issue_id = ? OR l.to_issue_id = ?`,
		issueID, issueID, issueID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []db.LinkBlocker
	for rows.Next() {
		var b db.LinkBlocker
		if err := rows.Scan(&b.LinkID, &b.Type, &b.PeerUID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpdatePriority sets issues.priority to the new value and emits the matching
// priority_set / priority_cleared event. newPriority == nil means clear. No-op
// when the new value matches the current value (returns nil event,
// changed=false).
//
// Event payloads:
//   - issue.priority_set:     {"priority": <new>, "old_priority": <old>}
//     where old_priority is omitted when the prior value was nil.
//   - issue.priority_cleared: {"old_priority": <old>}
//     emitted only when there was a prior value to clear; clearing an
//     already-null priority is a no-op (changed=false, no event).
func (d *Store) UpdatePriority(ctx context.Context, issueID int64, newPriority *int64, actor string) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if priorityEqual(issue.Priority, newPriority) {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}

	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET priority   = ?,
		     updated_at = ?
		 WHERE id = ?`, newPriority, ts, issueID); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("update priority: %w", err)
	}

	eventType, payload, err := priorityEventPayload(issue.Priority, newPriority, ts)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        eventType,
		Actor:       actor,
		Payload:     payload,
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

func priorityEqual(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// priorityEventPayload returns the event type and JSON payload for a
// priority transition from old to new. old==new is rejected as a programming
// error because UpdatePriority short-circuits no-ops before reaching here.
func priorityEventPayload(old, newPrio *int64, ts string) (string, string, error) {
	type setPayload struct {
		Priority    int64  `json:"priority"`
		OldPriority *int64 `json:"old_priority,omitempty"`
		UpdatedAt   string `json:"updated_at"`
	}
	type clearedPayload struct {
		OldPriority int64  `json:"old_priority"`
		UpdatedAt   string `json:"updated_at"`
	}
	if newPrio != nil {
		bs, err := json.Marshal(setPayload{Priority: *newPrio, OldPriority: old, UpdatedAt: ts})
		if err != nil {
			return "", "", fmt.Errorf("marshal priority_set payload: %w", err)
		}
		return "issue.priority_set", string(bs), nil
	}
	// Clearing: old must be non-nil (priorityEqual short-circuits two nils).
	if old == nil {
		return "", "", fmt.Errorf("priorityEventPayload: cannot clear a nil priority")
	}
	bs, err := json.Marshal(clearedPayload{OldPriority: *old, UpdatedAt: ts})
	if err != nil {
		return "", "", fmt.Errorf("marshal priority_cleared payload: %w", err)
	}
	return "issue.priority_cleared", string(bs), nil
}

// MergeProjects moves every project-scoped row from SourceProjectID into
// TargetProjectID, then deletes the source project. Source-side issues whose
// short_ids collide with target-side short_ids are auto-extended in
// ULID-ascending order (spec §5.2); existing target short_ids stay put. The
// returned ShortIDExtensions list reports each shifted issue's pre/post values.
func (d *Store) MergeProjects(ctx context.Context, p db.MergeProjectsParams) (db.ProjectMergeResult, error) {
	if p.SourceProjectID == p.TargetProjectID {
		return db.ProjectMergeResult{}, db.ErrProjectMergeSameProject
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("begin merge projects: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	source, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.SourceProjectID))
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("load source project: %w", err)
	}
	target, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.TargetProjectID))
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("load target project: %w", err)
	}
	if isSystemProject(source) || isSystemProject(target) {
		return db.ProjectMergeResult{}, db.ErrNotFound
	}
	if source.DeletedAt != nil {
		return db.ProjectMergeResult{}, db.ErrProjectMergeArchivedSource
	}
	if target.DeletedAt != nil {
		return db.ProjectMergeResult{}, db.ErrProjectMergeArchivedTarget
	}
	if err := rejectFederatedProjectMerge(ctx, tx, source.ID, target.ID); err != nil {
		return db.ProjectMergeResult{}, err
	}

	mappingCollisions, err := projectMergeImportMappingCollisions(ctx, tx, p.SourceProjectID, p.TargetProjectID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	if len(mappingCollisions) > 0 {
		return db.ProjectMergeResult{}, &db.ProjectMergeImportMappingCollisionError{Mappings: mappingCollisions}
	}

	// Reconcile short_id collisions BEFORE the bulk UPDATE moves issues onto
	// the target. The UNIQUE(project_id, short_id) index would otherwise reject
	// the move at the database layer. Each source-side issue is rewritten to
	// its smallest non-colliding length across both projects in ULID-ascending
	// order so the result is deterministic (spec §5.2).
	extensions, err := extendCollidingSourceShortIDs(ctx, tx, source.ID, target.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}

	issuesMoved, err := countProjectRows(ctx, tx, "issues", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	aliasesMoved, err := countProjectRows(ctx, tx, "project_aliases", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	eventsMoved, err := countProjectRows(ctx, tx, "events", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}
	purgeLogsMoved, err := countProjectRows(ctx, tx, "purge_log", source.ID)
	if err != nil {
		return db.ProjectMergeResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE issues SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move issues: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE links SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move links: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET project_id = ?, project_name = ? WHERE project_id = ?`,
		target.ID, target.Name, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move events: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE purge_log SET project_id = ?, project_name = ? WHERE project_id = ?`,
		target.ID, target.Name, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move purge log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move import mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_aliases SET project_id = ? WHERE project_id = ?`, target.ID, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("move aliases: %w", err)
	}

	if p.TargetName != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET name = ? WHERE id = ?`,
			*p.TargetName, target.ID); err != nil {
			return db.ProjectMergeResult{}, fmt.Errorf("update target project: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, source.ID); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("delete source project: %w", err)
	}

	mergedTarget, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, target.ID))
	if err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("reload target project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.ProjectMergeResult{}, fmt.Errorf("commit merge projects: %w", err)
	}

	return db.ProjectMergeResult{
		Source:            source,
		Target:            mergedTarget,
		IssuesMoved:       issuesMoved,
		AliasesMoved:      aliasesMoved,
		EventsMoved:       eventsMoved,
		PurgeLogsMoved:    purgeLogsMoved,
		ShortIDExtensions: extensions,
	}, nil
}

func rejectFederatedProjectMerge(ctx context.Context, tx *sql.Tx, sourceID, targetID int64) error {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM federation_bindings WHERE project_id IN (?, ?)`,
		sourceID, targetID).Scan(&n); err != nil {
		return fmt.Errorf("check federation merge binding: %w", err)
	}
	if n > 0 {
		return db.ErrProjectMergeFederationBinding
	}
	return nil
}

// extendCollidingSourceShortIDs rewrites the short_id of every source-side
// issue whose value would collide with an existing target-side short_id on
// move. Iteration is ULID-ascending so replays produce the same result
// (spec §5.2). Each replacement is the shortest length L >= shortid.MinLength
// at which the candidate is free in BOTH source and target — checking both
// projects together avoids transient duplicates on the source side before
// the bulk UPDATE runs.
//
// Target-side purge_log tombstones are honored as collisions too: a source
// issue moving into a target whose purge_log already claims that short_id
// would otherwise silently take a slot a previously-purged issue owned.
func extendCollidingSourceShortIDs(
	ctx context.Context,
	tx *sql.Tx,
	sourceID, targetID int64,
) ([]db.ShortIDExtension, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.uid, s.short_id
		FROM issues s
		WHERE s.project_id = ?
		  AND (
		    EXISTS (SELECT 1 FROM issues t
		             WHERE t.project_id = ? AND t.short_id = s.short_id)
		    OR
		    EXISTS (SELECT 1 FROM purge_log p
		             WHERE p.project_id = ? AND p.short_id = s.short_id)
		  )
		ORDER BY s.uid ASC`, sourceID, targetID, targetID)
	if err != nil {
		return nil, fmt.Errorf("scan source/target short_id collisions: %w", err)
	}
	type collider struct {
		id       int64
		uid      string
		oldShort string
	}
	var colliders []collider
	for rows.Next() {
		var c collider
		if err := rows.Scan(&c.id, &c.uid, &c.oldShort); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan collider row: %w", err)
		}
		colliders = append(colliders, c)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close collider rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collider rows: %w", err)
	}

	var extensions []db.ShortIDExtension
	for _, c := range colliders {
		// Search strictly longer than the colliding length: spec §5.2 forbids
		// shortening on merge, and the current length is known to collide
		// against a target row (otherwise this issue wouldn't be a collider).
		// Without the +1 floor, a source issue that was extended past
		// shortid.MinLength to dodge source-side neighbors which were later
		// purged would be re-keyed down to MinLength here, violating the rule.
		newShortID, err := assignShortIDIn(ctx, tx, []int64{sourceID, targetID}, c.uid, len(c.oldShort)+1)
		if err != nil {
			return nil, fmt.Errorf("auto-extend short_id for %s: %w", c.uid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET short_id = ? WHERE id = ?`,
			newShortID, c.id); err != nil {
			return nil, fmt.Errorf("update extended short_id for %s: %w", c.uid, err)
		}
		extensions = append(extensions, db.ShortIDExtension{
			UID:              c.uid,
			PreMergeShortID:  c.oldShort,
			PostMergeShortID: newShortID,
		})
	}
	return extensions, nil
}

func projectMergeImportMappingCollisions(
	ctx context.Context,
	tx *sql.Tx,
	sourceID, targetID int64,
) ([]db.ProjectMergeImportMappingCollision, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.source, s.external_id, s.object_type
		FROM import_mappings s
		INNER JOIN import_mappings t
		  ON t.project_id = ?
		 AND t.source = s.source
		 AND t.external_id = s.external_id
		 AND t.object_type = s.object_type
		WHERE s.project_id = ?
		ORDER BY s.source, s.object_type, s.external_id
		LIMIT 20`, targetID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("check project merge import mapping collisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.ProjectMergeImportMappingCollision
	for rows.Next() {
		var c db.ProjectMergeImportMappingCollision
		if err := rows.Scan(&c.Source, &c.ExternalID, &c.ObjectType); err != nil {
			return nil, fmt.Errorf("scan project merge import mapping collision: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func countProjectRows(ctx context.Context, tx *sql.Tx, table string, projectID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s rows: %w", table, err)
	}
	return n, nil
}

// RemoveProject archives a project: sets projects.deleted_at, hard-deletes
// every project_aliases row, and emits one project.removed event. Refuses
// with ErrProjectHasOpenIssues when the project still has open, non-deleted
// issues unless Force=true. The project row stays so events/issues keep a
// valid FK target; subsequent ListProjects / ProjectByName calls exclude
// it from the active surface.
func (d *Store) RemoveProject(ctx context.Context, p db.RemoveProjectParams) (db.Project, *db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Project{}, nil, fmt.Errorf("begin remove project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.ProjectID))
	if err != nil {
		return db.Project{}, nil, err
	}
	if isSystemProject(project) {
		return db.Project{}, nil, db.ErrNotFound
	}
	if project.DeletedAt != nil {
		return db.Project{}, nil, db.ErrProjectAlreadyArchived
	}

	openIssues, err := countOpenIssues(ctx, tx, project.ID)
	if err != nil {
		return db.Project{}, nil, err
	}
	if openIssues > 0 && !p.Force {
		return db.Project{}, nil, &db.ProjectHasOpenIssuesError{OpenIssues: openIssues}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		project.ID); err != nil {
		return db.Project{}, nil, fmt.Errorf("archive project: %w", err)
	}

	aliasCount, err := deleteAllAliasesForProject(ctx, tx, project.ID)
	if err != nil {
		return db.Project{}, nil, err
	}

	payload, err := json.Marshal(struct {
		AliasCount int64 `json:"alias_count"`
		OpenIssues int64 `json:"open_issues"`
		Force      bool  `json:"force,omitempty"`
	}{AliasCount: aliasCount, OpenIssues: openIssues, Force: p.Force})
	if err != nil {
		return db.Project{}, nil, fmt.Errorf("marshal project.removed payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		Type:        "project.removed",
		Actor:       p.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.Project{}, nil, err
	}

	updated, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, project.ID))
	if err != nil {
		return db.Project{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return db.Project{}, nil, fmt.Errorf("commit remove project: %w", err)
	}
	return updated, &evt, nil
}

// RestoreProject clears projects.deleted_at and emits one project.restored
// event. Active projects return a retry-safe no-op envelope: the project row,
// nil event, and changed=false. Unknown projects return ErrNotFound.
func (d *Store) RestoreProject(ctx context.Context, projectID int64, actor string) (db.Project, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Project{}, nil, false, fmt.Errorf("begin restore project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, projectID))
	if err != nil {
		return db.Project{}, nil, false, err
	}
	if isSystemProject(project) {
		return db.Project{}, nil, false, db.ErrNotFound
	}
	if project.DeletedAt == nil {
		if err := tx.Commit(); err != nil {
			return db.Project{}, nil, false, fmt.Errorf("commit restore project noop: %w", err)
		}
		return project, nil, false, nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE projects SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`,
		project.ID)
	if err != nil {
		return db.Project{}, nil, false, fmt.Errorf("restore project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Project{}, nil, false, fmt.Errorf("restore project rows affected: %w", err)
	}
	if n == 0 {
		if err := tx.Commit(); err != nil {
			return db.Project{}, nil, false, fmt.Errorf("commit restore project race noop: %w", err)
		}
		updated, err := d.ProjectByID(ctx, project.ID)
		if err != nil {
			return db.Project{}, nil, false, err
		}
		return updated, nil, false, nil
	}

	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		Type:        "project.restored",
		Actor:       actor,
		Payload:     "{}",
	})
	if err != nil {
		return db.Project{}, nil, false, err
	}
	updated, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, project.ID))
	if err != nil {
		return db.Project{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Project{}, nil, false, fmt.Errorf("commit restore project: %w", err)
	}
	return updated, &evt, true, nil
}

// DetachProjectAlias deletes one project_aliases row and emits a
// project.alias_removed event. Refuses with ErrAliasIsLast when this is the
// only alias for its project unless Force=true — the last alias is what
// connects a workspace path to a project, so dropping it without intent
// orphans the project from the filesystem.
//
// Lookup is keyed on (project_id, alias_id) inside the transaction so a
// reassignment between handler preflight and this call cannot drop an
// alias from a different project than the request named.
func (d *Store) DetachProjectAlias(ctx context.Context, p db.DetachAliasParams) (db.ProjectAlias, *db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("begin detach alias: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	alias, err := scanAlias(tx.QueryRowContext(ctx,
		aliasSelect+` WHERE id = ? AND project_id = ?`, p.AliasID, p.ProjectID))
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, alias.ProjectID))
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}

	var siblingCount int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_aliases WHERE project_id = ?`, alias.ProjectID).Scan(&siblingCount); err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("count sibling aliases: %w", err)
	}
	if siblingCount <= 1 && !p.Force {
		return db.ProjectAlias{}, nil, db.ErrAliasIsLast
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_aliases WHERE id = ? AND project_id = ?`,
		alias.ID, alias.ProjectID); err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("delete alias: %w", err)
	}

	payload, err := json.Marshal(struct {
		AliasIdentity string `json:"alias_identity"`
		AliasKind     string `json:"alias_kind"`
		WasLast       bool   `json:"was_last,omitempty"`
		Force         bool   `json:"force,omitempty"`
	}{
		AliasIdentity: alias.AliasIdentity,
		AliasKind:     alias.AliasKind,
		WasLast:       siblingCount <= 1,
		Force:         p.Force,
	})
	if err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("marshal project.alias_removed payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		Type:        "project.alias_removed",
		Actor:       p.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.ProjectAlias{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return db.ProjectAlias{}, nil, fmt.Errorf("commit detach alias: %w", err)
	}
	return alias, &evt, nil
}

// countOpenIssues returns the number of open, non-deleted issues belonging
// to projectID. Used by RemoveProject's refusal check.
func countOpenIssues(ctx context.Context, tx *sql.Tx, projectID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues
		 WHERE project_id = ? AND status = 'open' AND deleted_at IS NULL`,
		projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open issues: %w", err)
	}
	return n, nil
}

// deleteAllAliasesForProject hard-deletes every project_aliases row for the
// project and returns the count for the audit event payload.
func deleteAllAliasesForProject(ctx context.Context, tx *sql.Tx, projectID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_aliases WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count aliases for archive: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_aliases WHERE project_id = ?`, projectID); err != nil {
		return 0, fmt.Errorf("delete aliases for archive: %w", err)
	}
	return n, nil
}

// SearchFTS runs an FTS5 BM25-ranked query against issues_fts, joins back to
// issues, and returns the top `limit` rows scoped to the given project. When
// includeDeleted is false, soft-deleted issues are filtered. The returned
// Score is the negated raw BM25 (so higher = better match); MatchedIn is
// derived from per-column MATCH subqueries since FTS5 highlight() returns
// NULL on contentless tables.
func (d *Store) SearchFTS(ctx context.Context, projectID int64, q string, limit int, includeDeleted bool) ([]db.SearchCandidate, error) {
	return d.searchFTS(ctx, searchFTSReq{
		projectID: projectID, q: q, mode: searchAll,
		limit: limit, includeDeleted: includeDeleted,
	})
}

// SearchFTSAny is like SearchFTS but joins query tokens with FTS5 OR rather
// than implicit AND. The look-alike soft-block uses this so candidate
// retrieval has high recall — similarity.Score is the actual gate, and the
// AND form prematurely filters near-duplicates that differ by one token.
func (d *Store) SearchFTSAny(ctx context.Context, projectID int64, q string, limit int, includeDeleted bool) ([]db.SearchCandidate, error) {
	return d.searchFTS(ctx, searchFTSReq{
		projectID: projectID, q: q, mode: searchAny,
		limit: limit, includeDeleted: includeDeleted,
	})
}

type searchMode int

const (
	searchAll searchMode = iota // implicit AND across query tokens
	searchAny                   // explicit OR across query tokens
)

// searchFTSReq bundles the inputs to the shared searchFTS implementation so
// the helper stays under the 5-positional-param limit.
type searchFTSReq struct {
	projectID      int64
	q              string
	mode           searchMode
	limit          int
	includeDeleted bool
}

func (d *Store) searchFTS(ctx context.Context, r searchFTSReq) ([]db.SearchCandidate, error) {
	q := strings.TrimSpace(r.q)
	if q == "" {
		return nil, nil
	}
	limit := r.limit
	if limit <= 0 {
		limit = 20
	}
	// Cap unbounded callers — the per-column subqueries make a huge limit
	// expensive, and the HTTP layer is the natural enforcer but defending
	// here is cheap.
	if limit > 200 {
		limit = 200
	}

	// Split the user query on whitespace, quote each whitespace-delimited
	// segment as an FTS5 phrase. This keeps every segment opaque to FTS5's
	// special characters (`:`, `*`, parens, `OR`/`AND`/`NOT` as bare words);
	// embedded double quotes are doubled per FTS5 quoting rules. The top-level
	// phrase joins quoted tokens by mode (space → implicit AND, " OR " →
	// explicit OR).
	var quoted []string
	for _, w := range strings.Fields(q) {
		quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	var topPhrase string
	switch r.mode {
	case searchAny:
		topPhrase = strings.Join(quoted, " OR ")
	default:
		topPhrase = strings.Join(quoted, " ")
	}
	// Per-column MATCH always uses OR-of-tokens regardless of the top-level
	// mode: matched_in answers "which columns contributed at least one term?"
	// — under implicit-AND a cross-column match (e.g. title="login",
	// body="Safari" for "login Safari") is a valid hit but no single column
	// holds all the tokens, so an AND per-column subquery would mark every
	// column as not-matched and matched_in would be empty.
	colPhrase := topPhrase
	if r.mode == searchAll && len(quoted) > 1 {
		colPhrase = strings.Join(quoted, " OR ")
	}

	deletedFilter := "AND i.deleted_at IS NULL"
	if r.includeDeleted {
		deletedFilter = ""
	}
	// Per-column MATCH subqueries replace highlight() because issues_fts is
	// declared content='' (contentless), and highlight() returns NULL for every
	// column on contentless tables. Each subquery returns 1 if the row's
	// title/body/comments column matches the per-column phrase, 0 otherwise.
	query := fmt.Sprintf(`
		SELECT i.id, i.project_id, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at,
		       bm25(issues_fts),
		       (issues_fts.rowid IN (SELECT rowid FROM issues_fts WHERE title    MATCH ?)) AS in_title,
		       (issues_fts.rowid IN (SELECT rowid FROM issues_fts WHERE body     MATCH ?)) AS in_body,
		       (issues_fts.rowid IN (SELECT rowid FROM issues_fts WHERE comments MATCH ?)) AS in_comments
		FROM issues_fts
		JOIN issues i ON i.id = issues_fts.rowid
		WHERE issues_fts MATCH ?
		  AND i.project_id = ?
		  %s
		ORDER BY bm25(issues_fts) ASC
		LIMIT %d`, deletedFilter, limit)

	// Bind order: colPhrase (×3 — title MATCH, body MATCH, comments MATCH),
	// then topPhrase (top-level MATCH), then projectID. Reordering the
	// SELECT/WHERE clauses without updating the bind list will silently
	// transpose binds.
	rows, err := d.QueryContext(ctx, query, colPhrase, colPhrase, colPhrase, topPhrase, r.projectID)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []db.SearchCandidate
	for rows.Next() {
		var (
			i                           db.Issue
			rawScore                    float64
			inTitle, inBody, inComments bool
		)
		if err := rows.Scan(&i.ID, &i.ProjectID, &i.ShortID, &i.Title, &i.Body, &i.Status,
			&i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision,
			&i.RecurrenceID, &i.OccurrenceKey,
			&i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt,
			&rawScore, &inTitle, &inBody, &inComments); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		matched := make([]string, 0, 3)
		if inTitle {
			matched = append(matched, "title")
		}
		if inBody {
			matched = append(matched, "body")
		}
		if inComments {
			matched = append(matched, "comments")
		}
		// FTS5 BM25 returns negative numbers; invert so callers compare with
		// "higher = better" semantics.
		out = append(out, db.SearchCandidate{
			Issue:     i,
			Score:     -rawScore,
			MatchedIn: matched,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// assignShortID returns the smallest-length short_id (>= shortid.MinLength)
// derived from ulid that does not collide with any existing issue in the
// project, including soft-deleted rows or purge_log tombstones. Soft-deleted
// issues retain their short_ids so `kata restore` is stable; purged issues
// leave a purge_log tombstone so external refs (`kata#abc4` in a commit
// message) can't be silently re-targeted at a later-created issue whose ULID
// happens to suffix-match.
func assignShortID(ctx context.Context, tx *sql.Tx, projectID int64, ulid string) (string, error) {
	return assignShortIDIn(ctx, tx, []int64{projectID}, ulid, shortid.MinLength)
}

// resolveShortID picks the short_id for a new issue. When override is empty,
// it auto-extends via assignShortID. When override is non-empty (JSONL import
// path; spec §8.1), it validates that override is a syntactically valid
// short_id and equals the lowercased suffix of ulid at its own length — the
// same invariant the schema CHECK enforces. The override is used verbatim
// without a collision check: the UNIQUE(project_id, short_id) index will
// surface any duplicate at INSERT time.
func resolveShortID(ctx context.Context, tx *sql.Tx, projectID int64, ulid, override string) (string, error) {
	if override == "" {
		s, err := assignShortID(ctx, tx, projectID, ulid)
		if err != nil {
			return "", fmt.Errorf("assign short_id: %w", err)
		}
		return s, nil
	}
	if !shortid.Valid(override) {
		return "", fmt.Errorf("invalid short_id override %q", override)
	}
	derived, err := shortid.Derive(ulid, len(override))
	if err != nil {
		return "", fmt.Errorf("validate short_id override %q against uid %q: %w", override, ulid, err)
	}
	if override != derived {
		return "", fmt.Errorf("short_id override %q does not match uid %q suffix at length %d (expected %q)",
			override, ulid, len(override), derived)
	}
	return override, nil
}

// assignShortIDIn is the generalized form of assignShortID that returns the
// smallest-length short_id (>= minLength) derived from ulid that doesn't
// collide with any issue in the given project set. Rows whose uid matches
// ulid are excluded from the collision count, so re-keying an issue in place
// doesn't count its own row as a self-collision. minLength must be in
// [shortid.MinLength, shortid.MaxLength]; the merge rekey path passes
// len(currentShortID)+1 so a rekey can only extend, never shorten (spec
// §5.2). Single-project creates pass shortid.MinLength via assignShortID.
func assignShortIDIn(ctx context.Context, tx *sql.Tx, projectIDs []int64, ulid string, minLength int) (string, error) {
	if len(projectIDs) == 0 {
		return "", fmt.Errorf("assignShortIDIn: empty projectIDs")
	}
	if minLength < shortid.MinLength || minLength > shortid.MaxLength {
		return "", fmt.Errorf("assignShortIDIn: minLength %d out of range [%d, %d]",
			minLength, shortid.MinLength, shortid.MaxLength)
	}
	placeholders, args := projectIDPlaceholders(projectIDs)
	// The collision check runs across both live issues and purge_log
	// tombstones. v7→v8 cutover entries have short_id IS NULL and so cannot
	// match candidate (which is non-NULL); no special-case needed.
	query := `SELECT (
		(SELECT COUNT(*) FROM issues
		   WHERE project_id IN (` + placeholders + `)
		     AND short_id = ?
		     AND uid <> ?)
		+
		(SELECT COUNT(*) FROM purge_log
		   WHERE project_id IN (` + placeholders + `)
		     AND short_id = ?)
	)`
	for length := minLength; length <= shortid.MaxLength; length++ {
		candidate, err := shortid.Derive(ulid, length)
		if err != nil {
			return "", fmt.Errorf("derive short_id at length %d: %w", length, err)
		}
		queryArgs := make([]any, 0, 2*len(args)+3)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, candidate, ulid)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, candidate)
		var n int
		if err := tx.QueryRowContext(ctx, query, queryArgs...).Scan(&n); err != nil {
			return "", fmt.Errorf("collision check at length %d: %w", length, err)
		}
		if n == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("short_id auto-extend exhausted for ulid %s", ulid)
}

// projectIDPlaceholders returns a comma-separated "?"-list and the matching
// args slice for use in a SQL IN-clause.
func projectIDPlaceholders(ids []int64) (string, []any) {
	out := make([]byte, 0, 2*len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
		args = append(args, id)
	}
	return string(out), args
}
