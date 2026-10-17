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

// CheckFederationMoveSourceLifecycle keeps a native source joined while a
// destination still owes a locally authored move that needs its credential.
func CheckFederationMoveSourceLifecycle(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string) error {
	var sourceUID string
	err := tx.QueryRowContext(ctx, `SELECT p.uid FROM projects p
 JOIN federation_bindings b ON b.project_id=p.id
 WHERE p.id=$1 AND b.role='spoke' AND b.relay_config IS NULL`, projectID).Scan(&sourceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// A paused destination retains its owed move and may resume. Its durable
	// acknowledgment cursor, rather than its current push flag, releases source
	// authority for teardown.
	rows, err := tx.QueryContext(ctx, `SELECT e.payload FROM events e
 JOIN federation_bindings b ON b.project_id=e.project_id
 WHERE e.type='issue.moved' AND e.origin_instance_uid=$1
 AND b.role='spoke' AND b.relay_config IS NULL
 AND e.id>b.push_cursor_event_id`, originInstanceUID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		fromUID, _ := StringValue(PayloadMap([]byte(payload))["from_project_uid"])
		if fromUID == sourceUID {
			return fmt.Errorf("%w: sync pending issue moves in destination projects, then retry source leave or archive", ErrFederationResetBlockedByPendingPush)
		}
	}
	return rows.Err()
}

// CheckFederatedIssueMove allows local projects, or two active bindings in one
// hub group. Hub bindings belong to this daemon; spoke bindings name an origin.
func CheckFederatedIssueMove(ctx context.Context, q moveQuery, from, to int64) error {
	type binding struct {
		role, hub, actor string
		enabled, relay   bool
	}
	read := func(id int64) (binding, error) {
		var b binding
		var enabled int
		err := q.QueryRowContext(ctx, `SELECT b.role,b.hub_url,b.enabled,b.bound_actor,
 (b.relay_config IS NOT NULL OR EXISTS(SELECT 1 FROM federation_enrollments e
 WHERE e.project_id=b.project_id AND e.relay_protocol_version=1 AND e.revoked_at IS NULL))
 FROM federation_bindings b WHERE b.project_id=$1`, id).Scan(&b.role, &b.hub, &enabled, &b.actor, &b.relay)
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
	if ae == nil && be == nil && a.enabled && b.enabled && !a.relay && !b.relay && a.role == b.role {
		if a.role == string(FederationRoleHub) {
			return nil
		}
		if a.role == string(FederationRoleSpoke) && strings.TrimSpace(a.actor) == strings.TrimSpace(b.actor) {
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
	if !historical {
		if err := CheckFederatedIssueMove(ctx, tx, from, to); err != nil {
			return 0, 0, err
		}
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
	if e.Type == "issue.moved" || project != route {
		// Native pull trusts the joined hub with this stream's issue history.
		// A retained historical project identity needs no live credential or
		// binding after leave. Local authoring and hub push remain strict.
		var role string
		var enabled int
		var relay bool
		if err := tx.QueryRowContext(ctx, `SELECT role,enabled,relay_config IS NOT NULL
 FROM federation_bindings WHERE project_id=$1`, route).Scan(&role, &enabled, &relay); err != nil {
			return 0, err
		}
		if role != string(FederationRoleSpoke) || enabled != 1 || relay {
			return 0, errors.Join(ErrFederatedReadOnly, ErrFederatedMoveUnsupported)
		}
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
	}
	return project, nil
}

// RehomeFederatedMovedIssues preserves local row identities and connector/claim
// state before the existing per-project reconciliation writes the folded view.
func RehomeFederatedMovedIssues(ctx context.Context, tx *sql.Tx, events []FoldEvent, p FoldProjection, assign func(int64, FoldIssue) (string, error), validate func(int64, int64, int64) error, afterRehome func(string, string) error, acceptedEventUIDs []string, insertResetControl func(Project, *FederationMovedReference) (int64, error)) ([]int64, error) {
	moved := map[string]FoldEvent{}
	for _, e := range events {
		if e.Type == "issue.moved" {
			if previous, known := moved[e.IssueUID]; !known || compareClock(clockOf(e), clockOf(previous)) > 0 {
				moved[e.IssueUID] = e
			}
		}
	}
	projects := map[int64]bool{}
	for _, issue := range p.SortedIssues() {
		winner, known := moved[issue.UID]
		if !known {
			continue
		}
		var target int64
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT id,deleted_at IS NULL FROM projects WHERE uid=$1`, issue.ProjectUID).Scan(&target, &active); err != nil {
			return nil, err
		}
		// A detached destination retains standalone data. Its former streams
		// may keep syncing, but cannot reconcile that destination's projection.
		var joined bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)`, target).Scan(&joined); err != nil {
			return nil, err
		}
		if !joined {
			delete(p.Issues, issue.UID)
			continue
		}
		var id, source int64
		var sourceProject Project
		var recurrence *int64
		var sourceShort string
		err := tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,i.recurrence_id,p.uid,p.name,i.short_id FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, issue.UID).Scan(&id, &source, &recurrence, &sourceProject.UID, &sourceProject.Name, &sourceShort)
		if errors.Is(err, sql.ErrNoRows) {
			if active {
				projects[target] = true
				// A cold replica can expose an empty projection at the append
				// cursor before this fold creates the destination issue row.
				from, _ := StringValue(PayloadMap(winner.Payload)["from_project_uid"])
				if err := tx.QueryRowContext(ctx, `SELECT id,uid,name FROM projects WHERE uid=$1`, from).Scan(&sourceProject.ID, &sourceProject.UID, &sourceProject.Name); err != nil {
					return nil, err
				}
				if err := recordFederationMoveDisplacement(ctx, tx, sourceProject, issue.ProjectUID, winner, acceptedEventUIDs, nil, insertResetControl); err != nil {
					return nil, err
				}
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
		// Move ingress has already checked its authority and endpoints. Replaying
		// trusted history must also work after a former source has left.
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
		if err := afterRehome(issue.UID, issue.ProjectUID); err != nil {
			return nil, err
		}
		sourceProject.ID = source
		payload := PayloadMap(winner.Payload)
		fromProject, _ := StringValue(payload["from_project_uid"])
		fromShort, _ := StringValue(payload["from_short_id"])
		var reference *FederationMovedReference
		if sourceProject.UID != fromProject || sourceShort != fromShort {
			reference = &FederationMovedReference{IssueUID: issue.UID, FromProjectUID: sourceProject.UID, FromShortID: sourceShort, MoveEventUID: winner.UID}
		}
		if err := recordFederationMoveDisplacement(ctx, tx, sourceProject, issue.ProjectUID, winner, acceptedEventUIDs, reference, insertResetControl); err != nil {
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
func ClearFederatedMovedIssueHistory(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string) error {
	// Only quarantines covering subject events being removed block this reset;
	// unrelated quarantined work in a contributing stream remains independent.
	rows, err := tx.QueryContext(ctx, `SELECT q.event_uids FROM federation_quarantine q
 WHERE q.project_id<>$1 AND q.direction='push' AND q.skipped_at IS NULL
 AND EXISTS(SELECT 1 FROM events e WHERE e.project_id=q.project_id
 AND e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1))`, projectID)
	if err != nil {
		return err
	}
	quarantineUIDs := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		var uids []string
		if err := json.Unmarshal([]byte(raw), &uids); err != nil {
			_ = rows.Close()
			return err
		}
		for _, uid := range uids {
			quarantineUIDs[uid] = true
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(quarantineUIDs) > 0 {
		args := []any{projectID}
		placeholders := make([]string, 0, len(quarantineUIDs))
		for uid := range quarantineUIDs {
			args = append(args, uid)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		var quarantined bool
		//nolint:gosec // Only generated placeholders are joined; all UIDs are bound.
		query := `SELECT EXISTS(SELECT 1 FROM events WHERE project_id<>$1
  AND issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)
  AND uid IN (` + strings.Join(placeholders, ",") + `))`
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&quarantined); err != nil {
			return err
		}
		if quarantined {
			return ErrFederationResetBlockedByQuarantine
		}
	}
	// A destination reset must not discard offline work awaiting its former
	// source stream's acknowledgement, even when the destination is read-only.
	// Leaving retires a source binding and its cursor; its retained acknowledged
	// history must not become pending again against an invented zero cursor.
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e
 JOIN federation_bindings b ON b.project_id=e.project_id
 WHERE e.project_id<>$1 AND e.origin_instance_uid=$2
 AND e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)
 AND e.id>b.push_cursor_event_id
 AND e.type IN (`+FederationPushEventTypesSQL()+`))`, projectID, originInstanceUID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrFederationResetBlockedByPendingPush
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM events WHERE issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)`, projectID)
	return err
}

// ClearFederatedProjectEvents retains source envelopes still contributing to
// issues now owned elsewhere. A source reset cannot rewind those destinations.
func ClearFederatedProjectEvents(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1
 AND NOT EXISTS(SELECT 1 FROM issues i WHERE i.uid=events.issue_uid AND i.project_id<>$1)`, projectID); err != nil {
		return err
	}
	// Retained envelopes may still refer numerically to a peer being reset.
	// Portable UIDs and content hashes remain intact when local FKs detach.
	if _, err := tx.ExecContext(ctx, `UPDATE events SET issue_id=NULL
 WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, projectID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE events SET related_issue_id=NULL
 WHERE related_issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, projectID)
	return err
}
