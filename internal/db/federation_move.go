package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

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
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	pending, err := federationMoveSourceHasPendingEvents(ctx, tx, projectID, originInstanceUID)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: sync pending events for moved issues before source leave or archive", ErrFederationResetBlockedByPendingPush)
	}
	return nil
}

// federationMoveSourceHasPendingEvents protects history that remains in a
// former project stream after the issue itself has moved. The destination's
// acknowledgement of issue.moved cannot acknowledge earlier source updates
// or comments in that separate stream.
func federationMoveSourceHasPendingEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	originInstanceUID string,
) (bool, error) {
	const movedIssueEvents = `SELECT EXISTS(SELECT 1 FROM events e
 JOIN federation_bindings b ON b.project_id=e.project_id
 JOIN issues i ON i.uid=e.issue_uid AND i.project_id<>e.project_id
 WHERE e.project_id=$1 AND e.origin_instance_uid=$2`
	var pending bool
	if err := tx.QueryRowContext(ctx, movedIssueEvents+` AND e.id>b.push_cursor_event_id
 AND e.type IN (`+FederationPushEventTypesSQL()+`))`,
		projectID, originInstanceUID).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return true, nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT event_uids FROM federation_quarantine
 WHERE project_id=$1 AND direction='push' AND skipped_at IS NULL`, projectID)
	if err != nil {
		return false, err
	}
	quarantined := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return false, err
		}
		var eventUIDs []string
		if err := json.Unmarshal([]byte(raw), &eventUIDs); err != nil {
			_ = rows.Close()
			return false, err
		}
		for _, eventUID := range eventUIDs {
			quarantined[eventUID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(quarantined) == 0 {
		return false, nil
	}

	eventUIDs := make([]string, 0, len(quarantined))
	for eventUID := range quarantined {
		eventUIDs = append(eventUIDs, eventUID)
	}
	for start := 0; start < len(eventUIDs); start += 500 {
		end := min(start+500, len(eventUIDs))
		args := []any{projectID, originInstanceUID}
		placeholders := make([]string, 0, end-start)
		for _, eventUID := range eventUIDs[start:end] {
			args = append(args, eventUID)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		query := movedIssueEvents + ` AND e.type IN (` + FederationPushEventTypesSQL() + `)
 AND e.uid IN (` + strings.Join(placeholders, ",") + `))`
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&pending); err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
	}
	return false, nil
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
		if historical && errors.Is(err, sql.ErrNoRows) {
			return retainFederationProject(ctx, tx, uid)
		}
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
	err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1`, e.ProjectUID).Scan(&project)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	missing := errors.Is(err, sql.ErrNoRows)
	var routeUID string
	if missing || e.Type == "issue.moved" || project != route {
		// Native pull trusts the joined hub with this stream's issue history.
		// A retained historical project identity needs no live credential or
		// binding after leave. Local authoring and hub push remain strict.
		var role string
		var enabled int
		var relay bool
		if err := tx.QueryRowContext(ctx, `SELECT b.role,b.enabled,b.relay_config IS NOT NULL,p.uid
 FROM federation_bindings b JOIN projects p ON p.id=b.project_id WHERE b.project_id=$1`, route).Scan(&role, &enabled, &relay, &routeUID); err != nil {
			if missing && errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("%w: event project is not joined", ErrFederationIngestValidation)
			}
			return 0, err
		}
		if role != string(FederationRoleSpoke) || enabled != 1 || relay {
			if missing {
				return 0, fmt.Errorf("%w: event project is not joined", ErrFederationIngestValidation)
			}
			return 0, errors.Join(ErrFederatedReadOnly, ErrFederatedMoveUnsupported)
		}
	}
	if e.Type == "issue.moved" {
		_, destination, err := federationMoveProjects(ctx, tx, route, e, true)
		if err != nil {
			return 0, err
		}
		project, missing = destination, false
	}
	if missing || project != route {
		if EventIssueUID(e.IssueUID) == "" {
			return 0, fmt.Errorf("%w: project event outside requested stream", ErrFederationIngestValidation)
		}
	}
	if (missing || project != route) && e.ProjectUID != routeUID {
		if err := recordFederationProjectIdentityDependency(ctx, tx, routeUID, e.ProjectUID); err != nil {
			return 0, err
		}
	}
	if missing {
		if !katauid.Valid(e.ProjectUID) {
			return 0, fmt.Errorf("%w: invalid project UID", ErrFederationIngestValidation)
		}
		return retainFederationProject(ctx, tx, e.ProjectUID)
	}
	return project, nil
}

// retainFederationProject keeps the envelope's UID without reserving its hub name.
func retainFederationProject(ctx context.Context, tx *sql.Tx, uid string) (int64, error) {
	for suffix := 0; ; suffix++ {
		name := "federation-" + strings.ToLower(uid)
		if suffix > 0 {
			name = fmt.Sprintf("%s-%d", name, suffix)
		}
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO projects(uid,name,deleted_at) VALUES($1,$2,$3)
 ON CONFLICT DO NOTHING RETURNING id`, uid, name, time.Now().UTC().Format(EventTimestampFormat)).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1`, uid).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
}

// FederatedMoveDecision records the state used to decide whether to rehome an
// issue. It can be compared with a fresh decision before applying a snapshot.
type FederatedMoveDecision struct {
	IssueUID, SourceUID, SourceName, SourceShortID string
	IssueID, SourceID, TargetID                    int64
	RecurrenceID                                   sql.NullInt64
	Active, Joined, Exists                         bool
}

// IndexFederatedMoveEvents selects the latest move for each issue.
func IndexFederatedMoveEvents(events []FoldEvent) map[string]FoldEvent {
	moved := map[string]FoldEvent{}
	for _, e := range events {
		if e.Type == "issue.moved" {
			if previous, known := moved[e.IssueUID]; !known || compareClock(clockOf(e), clockOf(previous)) > 0 {
				moved[e.IssueUID] = e
			}
		}
	}
	return moved
}

// PlanFederatedMovedIssues reads move decisions without changing any rows.
func PlanFederatedMovedIssues(ctx context.Context, tx *sql.Tx, moved map[string]FoldEvent, p FoldProjection) ([]FederatedMoveDecision, []int64, error) {
	var decisions []FederatedMoveDecision
	projects := map[int64]bool{}
	for _, issue := range p.SortedIssues() {
		if _, ok := moved[issue.UID]; !ok {
			continue
		}
		decision := FederatedMoveDecision{IssueUID: issue.UID}
		if err := tx.QueryRowContext(ctx, `SELECT id,deleted_at IS NULL FROM projects WHERE uid=$1`, issue.ProjectUID).Scan(&decision.TargetID, &decision.Active); err != nil {
			return nil, nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)`, decision.TargetID).Scan(&decision.Joined); err != nil {
			return nil, nil, err
		}
		err := tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,i.recurrence_id,p.uid,p.name,i.short_id FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, issue.UID).Scan(&decision.IssueID, &decision.SourceID, &decision.RecurrenceID, &decision.SourceUID, &decision.SourceName, &decision.SourceShortID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		decision.Exists = err == nil
		if decision.Joined && decision.Active {
			projects[decision.TargetID] = true
		}
		decisions = append(decisions, decision)
	}
	return decisions, slices.Sorted(maps.Keys(projects)), nil
}

// RehomeFederatedMovedIssues preserves local row identities and connector/claim
// state before the existing per-project reconciliation writes the folded view.
func RehomeFederatedMovedIssues(ctx context.Context, tx *sql.Tx, moved map[string]FoldEvent, p FoldProjection, decisions []FederatedMoveDecision, assign func(int64, FoldIssue) (string, error), validate func(int64, int64, int64) error, afterRehome func(string, string) error, applyUnjoined func(int64, FoldIssue, FoldProjection) error, acceptedEventUIDs []string, insertResetControl func(Project, *FederationMovedReference) (int64, error)) (resultErr error) {
	var applying *FoldEvent
	defer func() {
		if resultErr != nil && applying != nil {
			resultErr = fmt.Errorf("apply move project %s event %s: %w", applying.ProjectUID, applying.UID, resultErr)
		}
	}()
	for _, decision := range decisions {
		issue := p.Issues[decision.IssueUID]
		winner := moved[issue.UID]
		applying = &winner
		target, active := decision.TargetID, decision.Active
		if !decision.Joined {
			delete(p.Issues, issue.UID)
			if !decision.Exists {
				continue
			}
		}
		id, source := decision.IssueID, decision.SourceID
		sourceProject := Project{UID: decision.SourceUID, Name: decision.SourceName}
		sourceShort := decision.SourceShortID
		if !decision.Exists {
			if active {
				// A cold replica can expose an empty projection at the append
				// cursor before this fold creates the destination issue row.
				from, _ := StringValue(PayloadMap(winner.Payload)["from_project_uid"])
				if err := tx.QueryRowContext(ctx, `SELECT id,uid,name FROM projects WHERE uid=$1`, from).Scan(&sourceProject.ID, &sourceProject.UID, &sourceProject.Name); err != nil {
					return err
				}
				if err := recordFederationMoveDisplacement(ctx, tx, sourceProject, issue.ProjectUID, winner, acceptedEventUIDs, nil, insertResetControl); err != nil {
					return err
				}
			} else {
				delete(p.Issues, issue.UID)
			}
			continue
		}
		if source == target {
			if !decision.Joined {
				if err := applyUnjoined(target, issue, p); err != nil {
					return err
				}
				if err := reconcileParkedIssueLinks(ctx, tx, issue.UID, p); err != nil {
					return err
				}
			}
			continue
		}
		if decision.Joined && !active {
			return ErrNotFound
		}
		// Move ingress has already checked its authority and endpoints. Replaying
		// trusted history must also work after a former source has left.
		if decision.RecurrenceID.Valid {
			return fmt.Errorf("%w: %w", ErrFederationIngestValidation, &RecurrencePinnedError{})
		}
		if err := validate(id, source, target); err != nil {
			return fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
		}

		short, err := assign(target, issue)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET project_id=$1,short_id=$2,revision=revision+1 WHERE id=$3`, target, short, id); err != nil {
			return err
		}
		if err := afterRehome(issue.UID, issue.ProjectUID); err != nil {
			return err
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
			return err
		}
		for _, query := range []string{
			`UPDATE issue_claims SET project_id=$1 WHERE issue_id=$2`,
			`UPDATE pending_claim_requests SET project_id=$1 WHERE issue_id=$2`,
			`UPDATE import_mappings SET project_id=$1,observed_status=NULL,observed_status_at=NULL,pending_event_uid=NULL,remote_locator=NULL WHERE issue_id=$2`,
			`UPDATE external_root_bindings SET project_id=$1 WHERE issue_id=$2`,
		} {
			if _, err := tx.ExecContext(ctx, query, target, id); err != nil {
				return err
			}
		}
		if !decision.Joined {
			if err := applyUnjoined(target, issue, p); err != nil {
				return err
			}
			if err := reconcileParkedIssueLinks(ctx, tx, issue.UID, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func reconcileParkedIssueLinks(ctx context.Context, tx *sql.Tx, issueUID string, p FoldProjection) error {
	for key, state := range p.Links {
		if state.Present || key.FromUID != issueUID && key.ToUID != issueUID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE type=$1 AND (from_issue_uid=$2 AND to_issue_uid=$3 OR $1='related' AND from_issue_uid=$3 AND to_issue_uid=$2)`, key.Type, key.FromUID, key.ToUID); err != nil {
			return err
		}
	}
	for _, edge := range p.PresentLinks() {
		key, state := edge.Key, edge.State
		if key.FromUID != issueUID && key.ToUID != issueUID {
			continue
		}
		var fromID, toID int64
		err := tx.QueryRowContext(ctx, `SELECT a.id,b.id FROM issues a JOIN issues b ON b.uid=$2 WHERE a.uid=$1`, key.FromUID, key.ToUID).Scan(&fromID, &toID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if key.Type == "related" && fromID > toID {
			fromID, toID = toID, fromID
			key.FromUID, key.ToUID = key.ToUID, key.FromUID
		}
		author := state.Author
		if strings.TrimSpace(author) == "" {
			author = "federation"
		}
		created := state.CreatedAt
		if created == "" {
			created = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(from_issue_id,to_issue_id,type) DO UPDATE SET author=excluded.author,created_at=CASE WHEN $8='' THEN links.created_at ELSE excluded.created_at END`, fromID, toID, key.FromUID, key.ToUID, key.Type, author, created, state.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

type movedProjectPassKey struct{}

// PreserveMovedIssueLabels retains labels that have no folded add or remove.
func PreserveMovedIssueLabels(ctx context.Context, tx *sql.Tx, projectID int64, p FoldProjection, decisions []FederatedMoveDecision) error {
	for _, decision := range decisions {
		if !decision.Exists || !decision.Joined || decision.TargetID != projectID {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT label FROM issue_labels WHERE issue_id=$1`, decision.IssueID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var label string
			if err := rows.Scan(&label); err != nil {
				_ = rows.Close()
				return err
			}
			key := FoldLabelKey{IssueUID: decision.IssueUID, Label: label}
			if _, ok := p.Labels[key]; !ok {
				p.Labels[key] = FoldElementState{Present: true}
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// FoldMovedIssue limits reconciliation to the rehomed issue and its history.
func FoldMovedIssue(p FoldProjection, issue FoldIssue) FoldProjection {
	result := FoldProjection{
		Issues:        map[string]FoldIssue{issue.UID: issue},
		IssueMetadata: map[string]jsontext.Value{issue.UID: p.IssueMetadata[issue.UID]},
		Comments:      make(map[string]FoldComment),
		Labels:        make(map[FoldLabelKey]FoldElementState),
	}
	for uid, comment := range p.Comments {
		if comment.IssueUID == issue.UID {
			result.Comments[uid] = comment
		}
	}
	for key, state := range p.Labels {
		if key.IssueUID == issue.UID {
			result.Labels[key] = state
		}
	}
	return result
}

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
	RestrictFoldProjectWhere(p, projectUID, nil)
}

// RestrictFoldProjectWhere applies an additional issue filter to a project view.
func RestrictFoldProjectWhere(p *FoldProjection, projectUID string, keep func(string) bool) {
	for uid, issue := range p.Issues {
		if issue.ProjectUID != projectUID || (keep != nil && !keep(uid)) {
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
		query := `SELECT EXISTS(SELECT 1 FROM events e WHERE e.project_id<>$1
  AND e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)
  AND e.uid IN (` + strings.Join(placeholders, ",") + `))`
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
	if err := PreserveFederationProjectIdentityDependencies(ctx, tx, projectID); err != nil {
		return fmt.Errorf("preserve former project identity dependencies: %w", err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM events AS e WHERE e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)`, projectID)
	return err
}

// ClearFederatedProjectEvents retains source envelopes still contributing to
// issues now owned elsewhere. A source reset cannot rewind those destinations.
func ClearFederatedProjectEvents(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM events AS e WHERE e.project_id=$1
 AND NOT EXISTS(SELECT 1 FROM issues i WHERE i.uid=e.issue_uid AND i.project_id<>$1)`, projectID); err != nil {
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
