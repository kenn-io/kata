package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	katauid "go.kenn.io/kata/internal/uid"
)

type moveQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CheckFederatedIssueMove allows local projects, or two active bindings in one
// hub group. Hub bindings belong to this daemon; spoke bindings name an origin.
func CheckFederatedIssueMove(ctx context.Context, q moveQuery, from, to int64) error {
	type binding struct {
		role, hub string
		enabled   bool
	}
	read := func(id int64) (binding, error) {
		var b binding
		var enabled int
		err := q.QueryRowContext(ctx, `SELECT role,hub_url,enabled FROM federation_bindings WHERE project_id=$1`, id).Scan(&b.role, &b.hub, &enabled)
		b.enabled = enabled == 1
		return b, err
	}
	a, ae := read(from)
	b, be := read(to)
	if errors.Is(ae, sql.ErrNoRows) && errors.Is(be, sql.ErrNoRows) {
		return nil
	}
	for _, err := range []error{ae, be} {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if ae == nil && be == nil && a.enabled && b.enabled && a.role == b.role {
		if a.role == string(FederationRoleHub) {
			return nil
		}
		if a.role == string(FederationRoleSpoke) {
			origin := func(raw string) string {
				u, err := url.Parse(raw)
				if err != nil || u.Hostname() == "" {
					return ""
				}
				scheme := strings.ToLower(u.Scheme)
				port := u.Port()
				if port == "" {
					switch scheme {
					case "https":
						port = "443"
					case "http":
						port = "80"
					default:
						return ""
					}
				}
				return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
			}
			if x := origin(a.hub); x != "" && x == origin(b.hub) {
				return nil
			}
		}
	}
	return errors.Join(ErrFederatedReadOnly, ErrFederatedMoveUnsupported)
}

// FederationMoveProjects checks the normal move envelope and its existing
// snapshot format. Both local replicas must be joined before accepting a move.
func FederationMoveProjects(ctx context.Context, tx *sql.Tx, routeID int64, e RemoteEvent) (int64, int64, error) {
	return federationMoveProjects(ctx, tx, routeID, e, false)
}

func federationMoveProjects(ctx context.Context, tx *sql.Tx, routeID int64, e RemoteEvent, historical bool) (int64, int64, error) {
	var payload struct {
		IssueUID, FromProjectUID, ToProjectUID, ToShortID string
		Snapshot                                          struct {
			UID string `json:"uid"`
		}
	}
	// The field names on the wire use underscores.
	m := PayloadMap(e.Payload)
	payload.IssueUID, _ = StringValue(m["issue_uid"])
	payload.FromProjectUID, _ = StringValue(m["from_project_uid"])
	payload.ToProjectUID, _ = StringValue(m["to_project_uid"])
	payload.ToShortID, _ = StringValue(m["to_short_id"])
	if err := json.Unmarshal(m["snapshot"], &payload.Snapshot); err != nil {
		return 0, 0, fmt.Errorf("%w: move missing snapshot", ErrFederationIngestValidation)
	}
	if e.IssueUID == nil || payload.IssueUID != *e.IssueUID || payload.Snapshot.UID != payload.IssueUID || !katauid.Valid(payload.FromProjectUID) || !katauid.Valid(payload.ToProjectUID) || payload.FromProjectUID == payload.ToProjectUID || e.ProjectUID != payload.ToProjectUID || payload.ToShortID == "" {
		return 0, 0, fmt.Errorf("%w: invalid move payload", ErrFederationIngestValidation)
	}
	lookup := func(uid string) (int64, error) {
		var id int64
		query := `SELECT id FROM projects WHERE uid=$1`
		if !historical {
			query += ` AND deleted_at IS NULL`
		}
		err := tx.QueryRowContext(ctx, query, uid).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: move project %s is not joined; join it and retry sync", ErrFederationIngestValidation, uid)
		}
		return id, err
	}
	from, err := lookup(payload.FromProjectUID)
	if err != nil {
		return 0, 0, err
	}
	to, err := lookup(payload.ToProjectUID)
	if err != nil {
		return 0, 0, err
	}
	if !historical && routeID != from && routeID != to {
		return 0, 0, fmt.Errorf("%w: move outside requested project", ErrFederationIngestValidation)
	}
	if err := CheckFederatedIssueMove(ctx, tx, from, to); err != nil {
		return 0, 0, err
	}
	return from, to, nil
}

// RemoteIssueEventProject keeps portable hashes and receipts tied to their
// envelope project. A trusted hub may deliver issue history through any later
// destination, including a page that precedes the move itself.
func RemoteIssueEventProject(ctx context.Context, tx *sql.Tx, route int64, e RemoteEvent) (int64, error) {
	var project int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1`, e.ProjectUID).Scan(&project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: event project is not joined", ErrFederationIngestValidation)
		}
		return 0, err
	}
	if e.Type == "issue.moved" {
		if _, _, err := federationMoveProjects(ctx, tx, route, e, true); err != nil {
			return 0, err
		}
	}
	if project != route {
		if e.IssueUID == nil {
			return 0, fmt.Errorf("%w: project event outside requested stream", ErrFederationIngestValidation)
		}
		if err := CheckFederatedIssueMove(ctx, tx, project, route); err != nil {
			return 0, err
		}
	}
	return project, nil
}

// RehomeFederatedMovedIssues preserves local row identities and connector/claim
// state before the existing per-project reconciliation writes the folded view.
func RehomeFederatedMovedIssues(ctx context.Context, tx *sql.Tx, events []FoldEvent, p FoldProjection, assign func(int64, FoldIssue) (string, error), validate func(int64, int64, int64) error) ([]int64, error) {
	moved := map[string]bool{}
	for _, e := range events {
		if e.Type == "issue.moved" {
			moved[e.IssueUID] = true
		}
	}
	projects := map[int64]bool{}
	for _, issue := range p.SortedIssues() {
		if !moved[issue.UID] {
			continue
		}
		var target int64
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT id,deleted_at IS NULL FROM projects WHERE uid=$1`, issue.ProjectUID).Scan(&target, &active); err != nil {
			return nil, err
		}
		var id, source int64
		var recurrence *int64
		err := tx.QueryRowContext(ctx, `SELECT id,project_id,recurrence_id FROM issues WHERE uid=$1`, issue.UID).Scan(&id, &source, &recurrence)
		if errors.Is(err, sql.ErrNoRows) {
			if active {
				projects[target] = true
			} else {
				delete(p.Issues, issue.UID)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if source == target {
			if active {
				projects[target] = true
			}
			continue
		}
		if !active {
			return nil, ErrNotFound
		}
		projects[target] = true
		if err := CheckFederatedIssueMove(ctx, tx, source, target); err != nil {
			return nil, err
		}
		if recurrence != nil {
			return nil, fmt.Errorf("%w: %w", ErrFederationIngestValidation, &RecurrencePinnedError{})
		}
		if err := validate(id, source, target); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
		}

		short, err := assign(target, issue)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET project_id=$1,short_id=$2,revision=revision+1 WHERE id=$3`, target, short, id); err != nil {
			return nil, err
		}
		for _, query := range []string{
			`UPDATE issue_claims SET project_id=$1 WHERE issue_id=$2`,
			`UPDATE pending_claim_requests SET project_id=$1 WHERE issue_id=$2`,
			`UPDATE import_mappings SET project_id=$1,observed_status=NULL,observed_status_at=NULL,pending_event_uid=NULL,remote_locator=NULL WHERE issue_id=$2`,
			`UPDATE external_root_bindings SET project_id=$1 WHERE issue_id=$2`,
		} {
			if _, err := tx.ExecContext(ctx, query, target, id); err != nil {
				return nil, err
			}
		}
	}
	var out []int64
	for id := range projects {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

type movedProjectPassKey struct{}

// FollowingMovedProjects reports whether a destination reconciliation pass is running.
func FollowingMovedProjects(ctx context.Context) bool {
	v, _ := ctx.Value(movedProjectPassKey{}).(bool)
	return v
}

// WithMovedProjectPass prevents destination reconciliation from recursively following moves.
func WithMovedProjectPass(ctx context.Context) context.Context {
	return context.WithValue(ctx, movedProjectPassKey{}, true)
}

// RestrictFoldProject gives the existing reconcilers only their own issue view.
func RestrictFoldProject(p *FoldProjection, projectUID string) {
	for uid, issue := range p.Issues {
		if issue.ProjectUID != projectUID {
			delete(p.Issues, uid)
		}
	}
	for uid, comment := range p.Comments {
		if _, ok := p.Issues[comment.IssueUID]; !ok {
			delete(p.Comments, uid)
		}
	}
	for key := range p.Labels {
		if _, ok := p.Issues[key.IssueUID]; !ok {
			delete(p.Labels, key)
		}
	}
}

// ClearFederatedMovedIssueHistory removes every contributing stream for issues
// being reset. Keeping a former source baseline would recreate a purged issue.
func ClearFederatedMovedIssueHistory(ctx context.Context, tx *sql.Tx, projectID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)`, projectID)
	return err
}
