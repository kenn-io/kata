package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	katauid "go.kenn.io/kata/internal/uid"
)

// ErrNotFound is returned when a single-row lookup matches zero rows.
var ErrNotFound = errors.New("not found")

// ErrOpenChildren is returned by CloseIssue when the issue still has open
// children at commit time. Callers map this to a 409 with the same
// parent_has_open_children code the pre-transaction guard uses; the
// in-transaction re-check closes the small race where a child link is
// inserted between the guard's read and the close write.
var ErrOpenChildren = errors.New("issue has open children")

// CreateProject inserts a new projects row.
func (d *DB) CreateProject(ctx context.Context, name string) (Project, error) {
	projectUID, err := katauid.New()
	if err != nil {
		return Project{}, fmt.Errorf("generate project uid: %w", err)
	}
	res, err := d.ExecContext(ctx,
		`INSERT INTO projects(uid, name) VALUES(?, ?)`, projectUID, name)
	if err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, fmt.Errorf("last id: %w", err)
	}
	return d.ProjectByID(ctx, id)
}

// ProjectByID fetches one project by its rowid. Archived (deleted_at != NULL)
// projects are returned as-is so callers like the merge / restore paths can
// see them; surface-level callers (HTTP handlers, CLI) inspect DeletedAt
// themselves.
func (d *DB) ProjectByID(ctx context.Context, id int64) (Project, error) {
	row := d.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, id)
	return scanProject(row)
}

// ProjectByName fetches one project by its UNIQUE name. Archived projects are
// excluded — resolve flow uses this and an archived project must look gone
// from the active surface. Callers needing the row even when archived can
// follow up with ProjectByNameIncludingArchived.
func (d *DB) ProjectByName(ctx context.Context, name string) (Project, error) {
	row := d.QueryRowContext(ctx,
		projectSelect+` WHERE name = ? AND deleted_at IS NULL`, name)
	return scanProject(row)
}

// ProjectByNameIncludingArchived returns the project even when archived.
// Used by error-message paths that want to distinguish "no project at all"
// from "project was archived".
func (d *DB) ProjectByNameIncludingArchived(ctx context.Context, name string) (Project, error) {
	row := d.QueryRowContext(ctx, projectSelect+` WHERE name = ?`, name)
	return scanProject(row)
}

// ProjectByUID fetches one project by its stable UID. Archived
// (deleted_at != NULL) projects are returned as-is so callers can decide
// how to surface the archived state; surface-level handlers should
// inspect DeletedAt themselves. Returns ErrNotFound when no row matches.
func (d *DB) ProjectByUID(ctx context.Context, uid string) (Project, error) {
	row := d.QueryRowContext(ctx, projectSelect+` WHERE uid = ?`, uid)
	return scanProject(row)
}

// RenameProject updates a project's canonical name without changing aliases or
// issue numbering.
func (d *DB) RenameProject(ctx context.Context, id int64, name string) (Project, error) {
	res, err := d.ExecContext(ctx, `UPDATE projects SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return Project{}, fmt.Errorf("rename project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Project{}, fmt.Errorf("rename project rows affected: %w", err)
	}
	if n == 0 {
		return Project{}, ErrNotFound
	}
	return d.ProjectByID(ctx, id)
}

// ListProjects returns every active project ordered by id ASC. Archived
// projects (deleted_at != NULL) are excluded; callers needing them too can
// use ListProjectsIncludingArchived.
func (d *DB) ListProjects(ctx context.Context) ([]Project, error) {
	return d.listProjects(ctx, false)
}

// ListProjectsIncludingArchived returns every project including archived
// rows. Used by surfaces that want to render archived state explicitly
// (e.g. operator inspection or restore tooling).
func (d *DB) ListProjectsIncludingArchived(ctx context.Context) ([]Project, error) {
	return d.listProjects(ctx, true)
}

func (d *DB) listProjects(ctx context.Context, includeArchived bool) ([]Project, error) {
	q := projectSelect
	if !includeArchived {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Project
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
func (d *DB) BatchProjectStats(ctx context.Context) (map[int64]ProjectStats, error) {
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
WHERE p.deleted_at IS NULL
ORDER BY p.id`
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("batch project stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]ProjectStats{}
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
		s := ProjectStats{Open: open, Closed: closed}
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
func (d *DB) AttachAlias(ctx context.Context, projectID int64, identity, kind, rootPath string) (ProjectAlias, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO project_aliases(project_id, alias_identity, alias_kind, root_path)
		 VALUES(?, ?, ?, ?)`, projectID, identity, kind, rootPath)
	if err != nil {
		return ProjectAlias{}, fmt.Errorf("insert alias: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ProjectAlias{}, err
	}
	return d.AliasByID(ctx, id)
}

// AliasByIdentity returns the alias for a given alias_identity.
func (d *DB) AliasByIdentity(ctx context.Context, identity string) (ProjectAlias, error) {
	row := d.QueryRowContext(ctx, aliasSelect+` WHERE alias_identity = ?`, identity)
	return scanAlias(row)
}

// AliasByID returns the project_aliases row with the given id.
func (d *DB) AliasByID(ctx context.Context, id int64) (ProjectAlias, error) {
	row := d.QueryRowContext(ctx, aliasSelect+` WHERE id = ?`, id)
	return scanAlias(row)
}

// TouchAlias updates last_seen_at to now and rewrites root_path. Returns
// ErrNotFound when no alias has the given id.
func (d *DB) TouchAlias(ctx context.Context, aliasID int64, rootPath string) error {
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
		return ErrNotFound
	}
	return nil
}

// ProjectAliases returns every alias attached to a project ordered by id ASC.
func (d *DB) ProjectAliases(ctx context.Context, projectID int64) ([]ProjectAlias, error) {
	rows, err := d.QueryContext(ctx, aliasSelect+` WHERE project_id = ? ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProjectAlias
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

func scanProject(r rowScanner) (Project, error) {
	var p Project
	err := r.Scan(&p.ID, &p.UID, &p.Name, &p.Metadata, &p.Revision, &p.CreatedAt, &p.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("scan project: %w", err)
	}
	return p, nil
}

// aliasSelect is the canonical SELECT list for project_aliases rows.
const aliasSelect = `SELECT id, project_id, alias_identity, alias_kind, root_path, created_at, last_seen_at FROM project_aliases`

func scanAlias(r rowScanner) (ProjectAlias, error) {
	var a ProjectAlias
	err := r.Scan(&a.ID, &a.ProjectID, &a.AliasIdentity, &a.AliasKind, &a.RootPath, &a.CreatedAt, &a.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectAlias{}, ErrNotFound
	}
	if err != nil {
		return ProjectAlias{}, fmt.Errorf("scan alias: %w", err)
	}
	return a, nil
}

// ErrNoFields is returned by EditIssue when no field is set.
var ErrNoFields = errors.New("no fields to update")

// InitialLink describes one of the optional links created in the same TX as
// the issue itself. The to_number is resolved within the same project.
//
// Default direction (Incoming=false): the new issue is the link's "from"
// side. Incoming=true reverses for type=blocks so the new issue is the
// "to" side (i.e. it is blocked by ToNumber). Rejected for type=parent
// (no inverse parent direction is exposed); meaningless for type=related
// which is symmetric.
type InitialLink struct {
	Type     string // "parent" | "blocks" | "related"
	ToNumber int64
	Incoming bool
}

// CreateIssueParams carries inputs for CreateIssue.
type CreateIssueParams struct {
	ProjectID int64
	Title     string
	Body      string
	Author    string

	// Optional. When non-empty, CreateIssue uses this ULID instead of
	// generating one. Required to be a valid 26-char ULID; the caller owns
	// uniqueness (the UNIQUE constraint on issues.uid will still surface
	// duplicates as a constraint error). This seam supports deterministic
	// tests and the JSONL replay path; live callers should leave it empty.
	UID string

	// Optional. When non-empty, CreateIssue bypasses assignShortID's
	// auto-extend loop and persists this value verbatim on the new row.
	// The value must satisfy shortid.Valid AND equal the lowercased suffix
	// of UID at its own length — the same invariant the schema CHECK
	// enforces. Used by JSONL import (spec §8.1) to preserve stored
	// short_ids across future cutovers; live callers leave it empty.
	ShortIDOverride string

	// Optional initial state. Plan 2 fields. CreateIssue inserts label/link
	// rows and applies the owner in the same TX, then folds them into the
	// issue.created event payload (no separate labeled/linked/assigned events).
	Labels   []string
	Links    []InitialLink
	Owner    *string
	Priority *int64

	// Optional. When non-empty, both fields are folded into the issue.created
	// event payload so future LookupIdempotency calls can find the row via
	// idx_events_idempotency.
	IdempotencyKey         string
	IdempotencyFingerprint string
}

// ErrInitialLinkTargetNotFound is returned when an InitialLink's to_number
// does not resolve to an existing, non-deleted issue in the same project.
var ErrInitialLinkTargetNotFound = errors.New("initial link target not found")

// ErrInitialLinkInvalidType is returned when an InitialLink's Type is not one
// of {parent, blocks, related}.
var ErrInitialLinkInvalidType = errors.New("invalid initial link type")

// CreateIssue inserts an issue, applies optional initial labels/links/owner,
// and appends a single issue.created event whose payload describes the initial
// state. All steps run in one TX.
func (d *DB) CreateIssue(ctx context.Context, p CreateIssueParams) (Issue, Event, error) {
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
				return Issue{}, Event{}, ErrInitialLinkInvalidType
			}
		case "blocks", "related":
		default:
			return Issue{}, Event{}, ErrInitialLinkInvalidType
		}
	}

	tx, err := d.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("begin: %w", err)
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
			return Issue{}, Event{}, ErrNotFound
		}
		return Issue{}, Event{}, fmt.Errorf("lookup project for create: %w", err)
	}

	issueUID := p.UID
	if issueUID == "" {
		issueUID, err = katauid.New()
		if err != nil {
			return Issue{}, Event{}, fmt.Errorf("generate issue uid: %w", err)
		}
	} else if !katauid.Valid(issueUID) {
		return Issue{}, Event{}, fmt.Errorf("invalid issue uid %q", issueUID)
	}

	shortID, err := resolveShortID(ctx, tx, p.ProjectID, issueUID, p.ShortIDOverride)
	if err != nil {
		return Issue{}, Event{}, err
	}

	// Insert issue + optional owner/priority columns in one statement.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO issues(uid, project_id, short_id, title, body, author, owner, priority)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		issueUID, p.ProjectID, shortID, p.Title, p.Body, p.Author, owner, p.Priority)
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("insert issue: %w", err)
	}
	issueID, err := res.LastInsertId()
	if err != nil {
		return Issue{}, Event{}, err
	}

	// Initial labels — dedupe (preserve first occurrence), then alphabetize
	// for stable payload + storage order.
	labels := dedupeStrings(p.Labels)
	sortStrings(labels)
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
			issueID, label, p.Author); err != nil {
			return Issue{}, Event{}, classifyLabelInsertError(err)
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
			return Issue{}, Event{}, ErrInitialLinkTargetNotFound
		}
		if err != nil {
			return Issue{}, Event{}, fmt.Errorf("resolve initial link target: %w", err)
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
			return Issue{}, Event{}, classifyLinkInsertError(err)
		}
	}

	payload := buildCreatedPayload(labels, links, resolvedTargets, owner, p.Priority, p.IdempotencyKey, p.IdempotencyFingerprint)

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
		return Issue{}, Event{}, err
	}

	if err := tx.Commit(); err != nil {
		return Issue{}, Event{}, fmt.Errorf("commit: %w", err)
	}

	issue, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return Issue{}, Event{}, err
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

// buildCreatedPayload returns the issue.created event payload as JSON. Empty
// initial state → "{}". Otherwise emits keys for whichever components are set,
// preserving determinism (sorted labels) so events are byte-stable.
//
// targets is parallel to links (same length and order). Each link's peer
// is captured at insertion time so the payload identifies the target
// stably (UID) and renderably (short_id). Pass nil/empty when no links
// are being recorded.
func buildCreatedPayload(labels []string, links []InitialLink, targets []createdLinkTarget, owner *string, priority *int64, idempotencyKey, idempotencyFingerprint string) string {
	type linkOut struct {
		Type       string `json:"type"`
		ToShortID  string `json:"to_short_id,omitempty"`
		ToIssueUID string `json:"to_issue_uid,omitempty"`
		Incoming   bool   `json:"incoming,omitempty"`
	}
	type out struct {
		Labels                 []string  `json:"labels,omitempty"`
		Links                  []linkOut `json:"links,omitempty"`
		Owner                  string    `json:"owner,omitempty"`
		Priority               *int64    `json:"priority,omitempty"`
		IdempotencyKey         string    `json:"idempotency_key,omitempty"`
		IdempotencyFingerprint string    `json:"idempotency_fingerprint,omitempty"`
	}
	var o out
	if len(labels) > 0 {
		o.Labels = labels
	}
	if len(links) > 0 {
		o.Links = make([]linkOut, 0, len(links))
		for i, l := range links {
			var t createdLinkTarget
			if i < len(targets) {
				t = targets[i]
			}
			o.Links = append(o.Links, linkOut{
				Type:       l.Type,
				ToShortID:  t.ShortID,
				ToIssueUID: t.UID,
				Incoming:   l.Incoming,
			})
		}
	}
	if owner != nil {
		o.Owner = *owner
	}
	o.Priority = priority
	o.IdempotencyKey = idempotencyKey
	o.IdempotencyFingerprint = idempotencyFingerprint
	bs, err := json.Marshal(o)
	if err != nil {
		return "{}"
	}
	return string(bs)
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
func dedupeLinks(in []InitialLink) []InitialLink {
	type key struct {
		Type     string
		ToNumber int64
		Incoming bool
	}
	seen := make(map[key]struct{}, len(in))
	out := make([]InitialLink, 0, len(in))
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
func (d *DB) IssueByID(ctx context.Context, id int64) (Issue, error) {
	row := d.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, id)
	return scanIssue(row)
}

// IncludeDeleted controls whether a lookup is allowed to return soft-deleted
// rows. Spec §6: normal read/mutate paths exclude them; the carveout paths
// (restore, idempotent re-delete, purge, idempotency-key collision detection)
// pass IncludeDeletedYes.
type IncludeDeleted int

const (
	// IncludeDeletedNo filters out rows with deleted_at IS NOT NULL.
	IncludeDeletedNo IncludeDeleted = 0
	// IncludeDeletedYes returns soft-deleted rows alongside live ones.
	IncludeDeletedYes IncludeDeleted = 1
)

// IssueByShortID resolves a project-scoped short_id. Soft-deleted issues are
// returned only when include == IncludeDeletedYes (spec §6: used by restore,
// idempotent re-delete, purge confirmation, and idempotency-key collision
// detection). Returns ErrNotFound when no row matches the filter.
func (d *DB) IssueByShortID(ctx context.Context, projectID int64, shortID string, include IncludeDeleted) (Issue, error) {
	q := issueSelect + ` WHERE i.project_id = ? AND i.short_id = ?`
	if include == IncludeDeletedNo {
		q += ` AND i.deleted_at IS NULL`
	}
	row := d.QueryRowContext(ctx, q, projectID, shortID)
	return scanIssue(row)
}

// IssueByUID fetches an issue by stable UID. Soft-deleted rows are returned
// only when include == IncludeDeletedYes (spec §6 carveout, matching
// IssueByShortID). Returns ErrNotFound when no row matches the filter.
func (d *DB) IssueByUID(ctx context.Context, issueUID string, include IncludeDeleted) (Issue, error) {
	q := issueSelect + ` WHERE i.uid = ?`
	if include == IncludeDeletedNo {
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
func (d *DB) ShortIDsByUIDs(
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
func (d *DB) IssueUIDPrefixMatch(ctx context.Context, prefix string, limit int, include IncludeDeleted) ([]Issue, error) {
	if limit <= 0 {
		limit = 20
	}
	q := issueSelect + ` WHERE i.uid LIKE ? || '%'`
	if include == IncludeDeletedNo {
		q += ` AND i.deleted_at IS NULL`
	}
	q += ` ORDER BY i.uid ASC LIMIT ?`
	rows, err := d.QueryContext(ctx, q, prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("issue uid prefix match: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	return out, rows.Err()
}

// ListIssuesParams filters list output. Status="" → all. Empty struct → all.
// Priority and MaxPriority are inclusive: Priority=*1 narrows to exactly P1;
// MaxPriority=*1 narrows to P0 and P1 (i.e. priority <= MaxPriority). Issues
// with NULL priority match neither filter — they only surface when both are
// nil.
type ListIssuesParams struct {
	ProjectID     int64
	Status        string   // "open" | "closed" | "" (any)
	Priority      *int64   // nil = no filter; non-nil = exactly this value
	MaxPriority   *int64   // nil = no filter; non-nil = priority <= MaxPriority
	Limit         int      // 0 = no limit
	Unowned       bool     // only issues where owner IS NULL
	Owner         string   // only issues where owner = this value (empty = no filter)
	Labels        []string // issues must have ALL these labels (AND logic)
	ExcludeLabels []string // issues must NOT have any of these labels
}

// ListIssues returns issues in the given project, excluding soft-deleted rows.
func (d *DB) ListIssues(ctx context.Context, p ListIssuesParams) ([]Issue, error) {
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
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ListAllIssuesParams filters cross-project list output. ProjectID==0 means
// "every project"; >0 narrows to a single project. Status="" → all statuses.
// Priority and MaxPriority work the same as ListIssuesParams.
type ListAllIssuesParams struct {
	ProjectID   int64
	Status      string
	Priority    *int64
	MaxPriority *int64
	Limit       int
}

// ListAllIssues returns issues across one or every project, excluding
// soft-deleted rows. Ordering is (created_at DESC, id DESC) per #22 — a
// stable "newest first" feed across projects, distinct from the per-project
// endpoint's updated_at-DESC ordering which leads with recent activity.
func (d *DB) ListAllIssues(ctx context.Context, p ListAllIssuesParams) ([]Issue, error) {
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
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// CreateCommentParams carries inputs for CreateComment.
type CreateCommentParams struct {
	IssueID int64
	Author  string
	Body    string
}

// CreateComment appends a comment + issue.commented event in one tx, bumping
// issues.updated_at.
func (d *DB) CreateComment(ctx context.Context, p CreateCommentParams) (Comment, Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Comment{}, Event{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return Comment{}, Event{}, err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO comments(issue_id, author, body) VALUES(?, ?, ?)`,
		p.IssueID, p.Author, p.Body)
	if err != nil {
		return Comment{}, Event{}, fmt.Errorf("insert comment: %w", err)
	}
	commentID, err := res.LastInsertId()
	if err != nil {
		return Comment{}, Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		p.IssueID); err != nil {
		return Comment{}, Event{}, fmt.Errorf("touch issue: %w", err)
	}

	payload := fmt.Sprintf(`{"comment_id":%d}`, commentID)
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.commented",
		Actor:       p.Author,
		Payload:     payload,
	})
	if err != nil {
		return Comment{}, Event{}, err
	}

	if err := tx.Commit(); err != nil {
		return Comment{}, Event{}, err
	}

	var c Comment
	if err := d.QueryRowContext(ctx,
		`SELECT id, issue_id, author, body, created_at FROM comments WHERE id = ?`,
		commentID).Scan(&c.ID, &c.IssueID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
		return Comment{}, Event{}, fmt.Errorf("read comment: %w", err)
	}
	return c, evt, nil
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
func (d *DB) CloseIssue(
	ctx context.Context,
	issueID int64,
	reason, actor, message string,
	evidence []Evidence,
) (Issue, *Event, bool, error) {
	if reason == "" {
		return Issue{}, nil, false, fmt.Errorf("close: reason is required")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return Issue{}, nil, false, err
	}
	if issue.Status == "closed" {
		return issue, nil, false, tx.Commit()
	}
	if hasOpen, err := txHasOpenChildren(ctx, tx, issue.ProjectID, issueID); err != nil {
		return Issue{}, nil, false, err
	} else if hasOpen {
		return Issue{}, nil, false, ErrOpenChildren
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET status        = 'closed',
		     closed_reason = ?,
		     closed_at     = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		     updated_at    = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`, reason, issueID); err != nil {
		return Issue{}, nil, false, fmt.Errorf("close: %w", err)
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
		return Issue{}, nil, false, err
	}
	parentUIDForPayload, parentSIDForPayload := new(string), new(string)
	if hasParent {
		*parentUIDForPayload = parentUID
		*parentSIDForPayload = parentSID
	}
	payloadBytes, err := json.Marshal(struct {
		Reason        string     `json:"reason"`
		Message       string     `json:"message,omitempty"`
		Evidence      []Evidence `json:"evidence,omitempty"`
		ParentUID     *string    `json:"parent_uid,omitempty"`
		ParentShortID *string    `json:"parent_short_id,omitempty"`
	}{
		Reason:        reason,
		Message:       message,
		Evidence:      evidence,
		ParentUID:     parentUIDForPayload,
		ParentShortID: parentSIDForPayload,
	})
	if err != nil {
		return Issue{}, nil, false, fmt.Errorf("close payload: %w", err)
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
		return Issue{}, nil, false, err
	}
	if reason == "done" && issue.RecurrenceID != nil && issue.OccurrenceKey != nil {
		if _, err := d.MaterializeNext(ctx, tx, *issue.RecurrenceID,
			*issue.OccurrenceKey, actor); err != nil {
			return Issue{}, nil, false, fmt.Errorf("materialize next recurrence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// CloseThrottleReason values for CloseThrottledPayload.Reason. Sibling-burst
// fires when an actor closes too many siblings under one parent in a short
// window; duplicate-message fires when an actor reuses identical close prose
// across sibling issues.
const (
	CloseThrottleReasonSiblingBurst     = "sibling-burst"
	CloseThrottleReasonDuplicateMessage = "duplicate-message"
)

// CloseThrottledPayload is the JSON wire shape persisted on close.throttled
// events. Parent is the user-facing issue number of the shared parent and is
// always populated when a throttle fires (the guards never refuse on an
// unparented issue). Cohort lists the recent sibling closes that triggered
// the burst guard; Prior is the prior matching close for the duplicate-
// message guard. Cohort and Prior are omitted when not relevant to the path
// that fired.
type CloseThrottledPayload struct {
	Reason string   `json:"reason"`
	Parent string   `json:"parent"`
	Cohort []string `json:"cohort,omitempty"`
	Prior  *string  `json:"prior,omitempty"`
}

// InsertCloseThrottledEvent records a close.throttled audit event for the
// refused close. The event is attached to the refused issue (issueID) so
// audit/replay tools can render it inline with that issue's other events.
// Returns the inserted event on success.
func (d *DB) InsertCloseThrottledEvent(
	ctx context.Context, issueID int64, actor string, payload CloseThrottledPayload,
) (Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return Event{}, err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal close.throttled payload: %w", err)
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
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	return evt, nil
}

// ReopenIssue clears status=closed unless already open. The
// issue.reopened event payload is always `{}`; bulk-reopen audit
// metadata was removed when bulk mode was dropped.
func (d *DB) ReopenIssue(
	ctx context.Context, issueID int64, actor string,
) (Issue, *Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return Issue{}, nil, false, err
	}
	if issue.Status == "open" {
		return issue, nil, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET status        = 'open',
		     closed_reason = NULL,
		     closed_at     = NULL,
		     updated_at    = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`, issueID); err != nil {
		return Issue{}, nil, false, fmt.Errorf("reopen: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.reopened",
		Actor:       actor,
		Payload:     "{}",
	})
	if err != nil {
		return Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

// EditIssueParams carries the optional fields for an edit. Nil = leave alone.
type EditIssueParams struct {
	IssueID int64
	Title   *string
	Body    *string
	Owner   *string
	Actor   string
}

// EditIssue mutates title/body/owner. ErrNoFields if none are set.
func (d *DB) EditIssue(ctx context.Context, p EditIssueParams) (Issue, *Event, bool, error) {
	if p.Title == nil && p.Body == nil && p.Owner == nil {
		return Issue{}, nil, false, ErrNoFields
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return Issue{}, nil, false, err
	}

	sets := []string{`updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`}
	args := []any{}
	if p.Title != nil {
		sets = append(sets, `title = ?`)
		args = append(args, *p.Title)
	}
	if p.Body != nil {
		sets = append(sets, `body = ?`)
		args = append(args, *p.Body)
	}
	if p.Owner != nil {
		sets = append(sets, `owner = ?`)
		args = append(args, *p.Owner)
	}
	args = append(args, p.IssueID)
	// `sets` only contains string literals chosen above; user-provided values
	// are parameterized via `args`. Safe to concatenate.
	q := `UPDATE issues SET ` + joinComma(sets) + ` WHERE id = ?` // #nosec G202
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return Issue{}, nil, false, fmt.Errorf("update issue: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        "issue.updated",
		Actor:       p.Actor,
		Payload:     "{}",
	})
	if err != nil {
		return Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, p.IssueID)
	if err != nil {
		return Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
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
func lookupIssueForEvent(ctx context.Context, tx *sql.Tx, issueID int64) (Issue, string, error) {
	const q = `
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at, p.name
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		WHERE i.id = ? AND i.deleted_at IS NULL`
	var i Issue
	var projectName string
	err := tx.QueryRowContext(ctx, q, issueID).
		Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status, &i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision, &i.RecurrenceID, &i.OccurrenceKey, &i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, "", ErrNotFound
	}
	if err != nil {
		return Issue{}, "", fmt.Errorf("lookup issue: %w", err)
	}
	return i, projectName, nil
}

const issueSelect = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status, i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision, i.recurrence_id, i.occurrence_key, i.created_at, i.updated_at, i.closed_at, i.deleted_at FROM issues i JOIN projects p ON p.id = i.project_id`

func scanIssue(r rowScanner) (Issue, error) {
	var i Issue
	err := r.Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status, &i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision, &i.RecurrenceID, &i.OccurrenceKey, &i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNotFound
	}
	if err != nil {
		return Issue{}, fmt.Errorf("scan issue: %w", err)
	}
	return i, nil
}

// eventInsert is the tx-internal payload used by insertEventTx.
type eventInsert struct {
	ProjectID       int64
	ProjectUID      string
	ProjectName     string
	IssueID         *int64
	IssueUID        *string
	RelatedIssueID  *int64
	RelatedIssueUID *string
	Type            string
	Actor           string
	Payload         string
}

// UpdateOwner sets issues.owner to the new value and emits the matching
// assigned/unassigned event. newOwner == nil means unassign. No-op when the
// new value matches the current value (returns nil event, changed=false).
func (d *DB) UpdateOwner(ctx context.Context, issueID int64, newOwner *string, actor string) (Issue, *Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return Issue{}, nil, false, err
	}
	// No-op: same owner.
	if ownerEqual(issue.Owner, newOwner) {
		return issue, nil, false, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET owner      = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`, newOwner, issueID); err != nil {
		return Issue{}, nil, false, fmt.Errorf("update owner: %w", err)
	}

	eventType := "issue.unassigned"
	payload := "{}"
	if newOwner != nil {
		eventType = "issue.assigned"
		bs, marshalErr := json.Marshal(struct {
			Owner string `json:"owner"`
		}{Owner: *newOwner})
		if marshalErr != nil {
			return Issue{}, nil, false, fmt.Errorf("marshal assigned payload: %w", marshalErr)
		}
		payload = string(bs)
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
		return Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return Issue{}, nil, false, err
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

// ErrAlreadyClaimed is returned by ClaimOwner when the issue is already owned
// by a different actor and force is false.
var ErrAlreadyClaimed = errors.New("already claimed")

// ClaimResult contains the result of a ClaimOwner operation.
type ClaimResult struct {
	Issue         Issue
	Event         *Event
	Changed       bool
	PreviousOwner *string
	CurrentOwner  *string // set when ErrAlreadyClaimed is returned
}

// ClaimOwner atomically claims an issue for the given actor. The conditional
// UPDATE ensures the claim only succeeds if the issue is unowned or owned by
// the same actor (or force is true). If a concurrent claim causes a SQLite
// busy/locked error during the UPDATE, we treat it as a conflict and return
// ErrAlreadyClaimed after fetching the current owner.
//
// Returns ErrAlreadyClaimed if the issue is already owned by a different actor
// and force is false. The ClaimResult.CurrentOwner field is set in this case.
func (d *DB) ClaimOwner(ctx context.Context, issueID int64, actor string, force bool) (ClaimResult, error) {
	actor = strings.TrimSpace(actor)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Read current state to get previous owner and check for no-op
	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return ClaimResult{}, err
	}

	// Already owned by same actor: no-op
	if issue.Owner != nil && *issue.Owner == actor {
		if err := tx.Commit(); err != nil {
			return ClaimResult{}, err
		}
		return ClaimResult{
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
	var res sql.Result
	if force {
		res, err = tx.ExecContext(ctx,
			`UPDATE issues
			 SET owner      = ?,
			     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ? AND deleted_at IS NULL`, actor, issueID)
	} else {
		res, err = tx.ExecContext(ctx,
			`UPDATE issues
			 SET owner      = ?,
			     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ? AND deleted_at IS NULL AND (owner IS NULL OR owner = ?)`, actor, issueID, actor)
	}
	if err != nil {
		return ClaimResult{}, fmt.Errorf("update owner: %w", err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return ClaimResult{}, fmt.Errorf("rows affected: %w", err)
	}

	// Zero rows affected means the conditional WHERE didn't match:
	// someone else claimed the issue between our read and write.
	if rowsAffected == 0 {
		return ClaimResult{CurrentOwner: issue.Owner}, ErrAlreadyClaimed
	}

	// Re-read the updated issue for response
	issue, _, err = lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return ClaimResult{}, err
	}

	// Emit assigned event
	bs, marshalErr := json.Marshal(struct {
		Owner string `json:"owner"`
	}{Owner: actor})
	if marshalErr != nil {
		return ClaimResult{}, fmt.Errorf("marshal assigned payload: %w", marshalErr)
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
		return ClaimResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return ClaimResult{}, err
	}

	return ClaimResult{
		Issue:         issue,
		Event:         &evt,
		Changed:       true,
		PreviousOwner: previousOwner,
	}, nil
}

// ReadyIssuesFilter holds optional filters for the ready query.
type ReadyIssuesFilter struct {
	Unowned       bool     // only issues where owner IS NULL
	Owner         string   // only issues where owner = this value (empty = no filter)
	Labels        []string // issues must have ALL these labels (AND logic)
	ExcludeLabels []string // issues must NOT have any of these labels
}

// ReadyIssues returns open, non-deleted issues with no open `blocks` predecessor,
// ordered by updated_at DESC. limit==0 means no limit.
func (d *DB) ReadyIssues(ctx context.Context, projectID int64, limit int, filter ReadyIssuesFilter) ([]Issue, error) {
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
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (d *DB) insertEventTx(ctx context.Context, tx *sql.Tx, in eventInsert) (Event, error) {
	eventUID, err := katauid.New()
	if err != nil {
		return Event{}, fmt.Errorf("generate event uid: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events(uid, origin_instance_uid, project_id, project_name, issue_id, issue_uid, related_issue_id, related_issue_uid, type, actor, payload)
		 VALUES(
		   ?, ?, ?, ?, ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   ?, ?, ?
		 )`,
		eventUID, d.instanceUID,
		in.ProjectID, in.ProjectName, in.IssueID,
		stringPtrValue(in.IssueUID), in.IssueID,
		in.RelatedIssueID,
		stringPtrValue(in.RelatedIssueUID), in.RelatedIssueID,
		in.Type, in.Actor, in.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	var e Event
	err = tx.QueryRowContext(ctx, eventSelectByID, id).
		Scan(&e.ID, &e.UID, &e.OriginInstanceUID, &e.ProjectID, &e.ProjectUID, &e.ProjectName, &e.IssueID,
			&e.IssueUID, &e.IssueShortID, &e.RelatedIssueID, &e.RelatedIssueUID, &e.RelatedIssueShortID,
			&e.Type, &e.Actor, &e.Payload, &e.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("read event: %w", err)
	}
	return e, nil
}

// eventSelectByID reads a single event by id with the same shape EventsAfter
// and EventsInWindow produce — the issue and related_issue short_ids are
// LEFT JOINed from the live `issues` table so mutation responses (which
// scan their inserted event through this query) carry the same wire shape
// as events streamed via poll/SSE.
const eventSelectByID = `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
       e.issue_id, e.issue_uid, i.short_id, e.related_issue_id, e.related_issue_uid, ri.short_id,
       e.type, e.actor, e.payload, e.created_at
  FROM events e
  JOIN projects p ON p.id = e.project_id
  LEFT JOIN issues i ON i.id = e.issue_id
  LEFT JOIN issues ri ON ri.id = e.related_issue_id
 WHERE e.id = ?`

func stringPtrValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
