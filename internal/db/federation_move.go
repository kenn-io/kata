package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/httpurl"
	katauid "go.kenn.io/kata/internal/uid"
)

type moveQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// federationMoveSupersededSQL reports whether a newer move won the fold over
// the alias issue's local moves. The fold stores its winner as the issue's
// project, so a local move still in force names that project as its
// destination. A later local move that wins keeps every earlier local move in
// force, because its destination stream needs them as predecessors. The
// concatenation keeps SQLite on the issue's (issue_uid,id) index instead of
// walking every event from this origin.
func federationMoveSupersededSQL(alias string) string {
	return `EXISTS (SELECT 1 FROM issues superseded WHERE superseded.uid=` + alias + `.issue_uid
 AND superseded.project_id<>(SELECT latest.project_id FROM events latest
 WHERE latest.issue_uid=` + alias + `.issue_uid AND latest.type='issue.moved'
 AND latest.origin_instance_uid||''=` + alias + `.origin_instance_uid
 ORDER BY latest.id DESC LIMIT 1))`
}

// FederationResetOwedEventSQL excludes events retained for issues in another project.
func FederationResetOwedEventSQL(alias string) string {
	return alias + `.type IN (` + FederationPushEventTypesSQL() + `) AND NOT (` + FederationHeldEventSQL(alias) + `) AND NOT (` + FederationEventReferencesMovedIssueSQL(alias) + `)`
}

// FederationEventReferencesMovedIssueSQL matches either moved issue endpoint.
func FederationEventReferencesMovedIssueSQL(alias string) string {
	return `EXISTS (SELECT 1 FROM issues i WHERE i.uid IN (` + alias + `.issue_uid,` + alias + `.related_issue_uid) AND i.project_id<>` + alias + `.project_id)`
}

const FederationHeldCursorPrefix = "federation_held_cursor."

// RetainFederationHeldCursor records the cursor before a leave deletes its binding.
func RetainFederationHeldCursor(ctx context.Context, tx *sql.Tx, projectID int64, projectUID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) SELECT $1,CAST(push_cursor_event_id AS TEXT) FROM federation_bindings WHERE project_id=$2 ON CONFLICT(key) DO UPDATE SET value=CASE WHEN CAST(meta.value AS BIGINT)>CAST(excluded.value AS BIGINT) THEN meta.value ELSE excluded.value END`, FederationHeldCursorPrefix+projectUID, projectID)
	return err
}

// RestoreFederationPushCursor preserves offline edits when push resumes.
func RestoreFederationPushCursor(ctx context.Context, tx *sql.Tx, projectID, cursor, bindingCursor int64) (int64, error) {
	var held int64
	err := tx.QueryRowContext(ctx, `SELECT CAST(m.value AS BIGINT) FROM meta m JOIN projects p ON m.key=$1 || p.uid WHERE p.id=$2`, FederationHeldCursorPrefix, projectID).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return cursor, nil
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1 || (SELECT uid FROM projects WHERE id=$2)`, FederationHeldCursorPrefix, projectID)
	return max(held, bindingCursor), err
}

// CheckFederationMoveSourceLifecycle keeps a native source joined while a
// destination still owes a locally authored move that needs its credential.
func CheckFederationMoveSourceLifecycle(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string, leaving bool) error {
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
	if leaving {
		var fenced bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meta WHERE key=$1)`, FederationLeaveFencePrefix+sourceUID).Scan(&fenced); err != nil {
			return err
		}
		if fenced {
			return nil
		}
	}
	// A paused destination retains its owed move and may resume. Its durable
	// acknowledgment cursor, rather than its current push flag, releases source
	// authority for teardown. Leaving retains a held cursor for superseded moves.
	hasSupersededMove := false
	//nolint:gosec // The SQL fragment uses a fixed alias.
	rows, err := tx.QueryContext(ctx, `SELECT e.payload, `+federationMoveSupersededSQL("e")+` FROM events e
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
		var superseded bool
		if err := rows.Scan(&payload, &superseded); err != nil {
			return err
		}
		if fromUID, _ := StringValue(PayloadMap([]byte(payload))["from_project_uid"]); fromUID == sourceUID {
			if leaving && superseded {
				hasSupersededMove = true
				continue
			}
			return fmt.Errorf("%w: sync pending issue moves in destination projects, then retry source leave or archive", ErrFederationResetBlockedByPendingPush)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	pending, err := federationMoveSourceHasPendingEvents(ctx, tx, projectID, originInstanceUID, hasSupersededMove)
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
	leaving bool,
) (bool, error) {
	movedIssueEvents := `SELECT EXISTS(SELECT 1 FROM events e
 JOIN federation_bindings b ON b.project_id=e.project_id
 JOIN issues i ON i.uid=e.issue_uid AND i.project_id<>e.project_id
 WHERE e.project_id=$1 AND e.origin_instance_uid=$2`
	var pending bool
	if !leaving {
		if err := tx.QueryRowContext(ctx, movedIssueEvents+` AND e.id>b.push_cursor_event_id
 AND e.type IN (`+FederationPushEventTypesSQL()+`))`,
			projectID, originInstanceUID).Scan(&pending); err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
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
	if missing || e.Type == "issue.moved" || project != route {
		// Native pull trusts the joined hub with this stream's issue history.
		// A retained historical project identity needs no live credential or
		// binding after leave. Local authoring and hub push remain strict.
		var role string
		var enabled int
		var relay bool
		err := tx.QueryRowContext(ctx, `SELECT role,enabled,relay_config IS NOT NULL
 FROM federation_bindings WHERE project_id=$1`, route).Scan(&role, &enabled, &relay)
		if missing && (errors.Is(err, sql.ErrNoRows) || err == nil && (role != string(FederationRoleSpoke) || enabled != 1 || relay)) {
			return 0, fmt.Errorf("%w: event project is not joined", ErrFederationIngestValidation)
		}
		if err != nil {
			return 0, err
		}
		if role != string(FederationRoleSpoke) || enabled != 1 || relay {
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
			if _, err = tx.ExecContext(ctx, `INSERT INTO project_access_policies(project_uid,visibility) VALUES($1,'teams')`, uid); err != nil {
				return 0, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE meta SET value=CAST(CAST(value AS BIGINT)+1 AS TEXT) WHERE key='project_access_revision'`); err != nil {
				return 0, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,'1') ON CONFLICT(key) DO NOTHING`, FederationRetainedProjectPrefix+uid)
			return id, err
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
	// Snapshot reports that a destination snapshot, not a retained move,
	// relocates the issue.
	Snapshot bool
	// Park moves the issue into the retained placeholder for a destination
	// this spoke has not joined, so it leaves the joined source's lists.
	Park bool
}

// FederatedMoveIndex holds the events that can relocate each issue.
type FederatedMoveIndex struct {
	// Winners holds each issue's latest move or snapshot.
	Winners map[string]FoldEvent
	// Moves holds each issue's latest retained move.
	Moves map[string]FoldEvent
}

// IndexFederatedMoveEvents selects the latest move and the latest move or
// snapshot for each issue.
func IndexFederatedMoveEvents(events []FoldEvent) FederatedMoveIndex {
	index := FederatedMoveIndex{Winners: map[string]FoldEvent{}, Moves: map[string]FoldEvent{}}
	latest := func(into map[string]FoldEvent, e FoldEvent) {
		if previous, known := into[e.IssueUID]; !known || compareClock(clockOf(e), clockOf(previous)) > 0 {
			into[e.IssueUID] = e
		}
	}
	for _, e := range events {
		switch e.Type {
		case "issue.moved":
			latest(index.Moves, e)
			latest(index.Winners, e)
		case "issue.snapshot":
			latest(index.Winners, e)
		}
	}
	return index
}

// PlanFederatedMovedIssues reads move decisions without changing any rows.
// projectID names the project being materialized; its own pull also plans the
// snapshot relocations described at planFederatedSnapshotRelocations.
func PlanFederatedMovedIssues(ctx context.Context, tx *sql.Tx, moves FederatedMoveIndex, p FoldProjection, projectID int64) ([]FederatedMoveDecision, []int64, error) {
	relocations, err := planFederatedSnapshotRelocations(ctx, tx, moves, p, projectID)
	if err != nil {
		return nil, nil, err
	}
	var decisions []FederatedMoveDecision
	projects := map[int64]bool{}
	for _, issue := range p.SortedIssues() {
		if decision, ok := relocations[issue.UID]; ok {
			decisions = append(decisions, decision)
			continue
		}
		if _, ok := moves.Moves[issue.UID]; !ok {
			continue
		}
		decision := FederatedMoveDecision{IssueUID: issue.UID}
		var placeholder bool
		if err := tx.QueryRowContext(ctx, `SELECT id,deleted_at IS NULL,EXISTS(SELECT 1 FROM meta WHERE key=$2) FROM projects WHERE uid=$1`, issue.ProjectUID, FederationRetainedProjectPrefix+issue.ProjectUID).Scan(&decision.TargetID, &decision.Active, &placeholder); err != nil {
			return nil, nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)`, decision.TargetID).Scan(&decision.Joined); err != nil {
			return nil, nil, err
		}
		var retained sql.NullString
		var unbound, active bool
		err := tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,i.recurrence_id,p.uid,p.name,i.short_id,b.project_id IS NULL,p.deleted_at IS NULL,m.value FROM issues i JOIN projects p ON p.id=i.project_id LEFT JOIN federation_bindings b ON b.project_id=p.id LEFT JOIN meta m ON m.key='federation_retained_project.' || p.uid WHERE i.uid=$1`, issue.UID).Scan(&decision.IssueID, &decision.SourceID, &decision.RecurrenceID, &decision.SourceUID, &decision.SourceName, &decision.SourceShortID, &unbound, &active, &retained)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		decision.Exists = err == nil
		// An issue held in a standalone project keeps its local copy.
		standalone := unbound && (active || !retained.Valid)
		decision.Park = placeholder && !decision.Joined && decision.Exists && !standalone
		if decision.Joined && decision.Active {
			projects[decision.TargetID] = true
		}
		decisions = append(decisions, decision)
	}
	// When this fold places an issue in another project through that project's
	// own snapshot, which reaches this fold only through a recorded relocation,
	// refresh that project as an applied move would. History that arrives here
	// for the issue then reaches the project that holds it.
	relocatedTo := map[string]bool{}
	for uid, issue := range p.Issues {
		if snapshot, ok := moves.Winners[uid]; ok && snapshot.Type == "issue.snapshot" && snapshot.ProjectUID == issue.ProjectUID {
			relocatedTo[issue.ProjectUID] = true
		}
	}
	for _, uid := range slices.Sorted(maps.Keys(relocatedTo)) {
		var id int64
		var joined bool
		err := tx.QueryRowContext(ctx, `SELECT p.id,EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id=p.id)
 FROM projects p WHERE p.uid=$1 AND p.deleted_at IS NULL`, uid).Scan(&id, &joined)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if joined && id != projectID {
			projects[id] = true
		}
	}
	return decisions, slices.Sorted(maps.Keys(projects)), nil
}

// planFederatedSnapshotRelocations finds issues that this project's own
// snapshot places here while another local project still holds their row. A
// spoke that joins after the hub moved an issue receives the destination's
// snapshot but never the move, so the snapshot relocates the held row the way
// an applied move would. Only the destination's own pull decides this, so a
// refused relocation fails that pull and no other. A pass that follows another
// project's moves into this one leaves such issues out of its reconciliation.
// Other projects see the relocation only after it is recorded.
func planFederatedSnapshotRelocations(ctx context.Context, tx *sql.Tx, moves FederatedMoveIndex, p FoldProjection, projectID int64) (map[string]FederatedMoveDecision, error) {
	var projectUID string
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT uid,deleted_at IS NULL FROM projects WHERE id=$1`, projectID).Scan(&projectUID, &active); err != nil {
		return nil, err
	}
	var uids []string
	for uid, issue := range p.Issues {
		snapshot, ok := moves.Winners[uid]
		if !ok || snapshot.Type != "issue.snapshot" || snapshot.ProjectUID != projectUID || issue.ProjectUID != projectUID {
			continue
		}
		// An issue with a retained move is rehomed by its move decision, as it
		// is without snapshots.
		if _, ok := moves.Moves[uid]; ok {
			continue
		}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return nil, nil
	}
	relocations := map[string]FederatedMoveDecision{}
	for chunk := range slices.Chunk(uids, 500) {
		slots, args := make([]string, len(chunk)), []any{projectID}
		for i, uid := range chunk {
			slots[i] = fmt.Sprintf("$%d", i+2)
			args = append(args, uid)
		}
		//nolint:gosec // G202: only generated placeholders enter SQL; all UIDs are bound.
		rows, err := tx.QueryContext(ctx, `SELECT i.uid,i.id,i.project_id,i.recurrence_id,p.uid,p.name,i.short_id
 FROM issues i JOIN projects p ON p.id=i.project_id
 WHERE i.project_id<>$1 AND i.uid IN (`+strings.Join(slots, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			decision := FederatedMoveDecision{TargetID: projectID, Active: active, Joined: true, Exists: true, Snapshot: true}
			if err := rows.Scan(&decision.IssueUID, &decision.IssueID, &decision.SourceID, &decision.RecurrenceID, &decision.SourceUID, &decision.SourceName, &decision.SourceShortID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			relocations[decision.IssueUID] = decision
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if FollowingMovedProjects(ctx) {
		for uid := range relocations {
			delete(p.Issues, uid)
		}
		return nil, nil
	}
	origin, err := federationNativeHubOrigin(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	for _, decision := range relocations {
		// Only another replica of the same hub can hand its issue over.
		source, err := federationNativeHubOrigin(ctx, tx, decision.SourceID)
		if err != nil {
			return nil, err
		}
		if origin == "" || source != origin {
			return nil, fmt.Errorf("%w: issue %s is held in project %s, and the two projects are not both joined directly to the same hub", errors.Join(ErrFederatedReadOnly, ErrFederatedMoveUnsupported), decision.IssueUID, decision.SourceName)
		}
	}
	return relocations, nil
}

// federationNativeHubOrigin returns the hub origin of a project's direct spoke
// binding, or "" when the project has none.
func federationNativeHubOrigin(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	var hubURL string
	err := tx.QueryRowContext(ctx, `SELECT hub_url FROM federation_bindings
 WHERE project_id=$1 AND role='spoke' AND relay_config IS NULL`, projectID).Scan(&hubURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	origin, err := httpurl.CanonicalHTTPOrigin(hubURL)
	if err != nil {
		return "", nil
	}
	return origin, nil
}

// AppendFederatedRelocationHistory adds the holding project's events for each
// issue a snapshot relocates. Until the relocation is recorded, the
// destination's fold has only the hub snapshot, and reconciling from it alone
// would drop the issue's spoke-local discussion. It reports whether any history
// was added, in which case the caller folds and plans once more.
func AppendFederatedRelocationHistory(events []FoldEvent, decisions []FederatedMoveDecision, sourceEvents func(sourceID int64, issueUIDs []string) ([]FoldEvent, error)) ([]FoldEvent, bool, error) {
	relocated := map[int64]map[string]bool{}
	for _, decision := range decisions {
		if decision.Snapshot && decision.Exists && decision.SourceID != decision.TargetID {
			if relocated[decision.SourceID] == nil {
				relocated[decision.SourceID] = map[string]bool{}
			}
			relocated[decision.SourceID][decision.IssueUID] = true
		}
	}
	if len(relocated) == 0 {
		return events, false, nil
	}
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		seen[event.UID] = true
	}
	added := false
	for _, sourceID := range slices.Sorted(maps.Keys(relocated)) {
		history, err := sourceEvents(sourceID, slices.Sorted(maps.Keys(relocated[sourceID])))
		if err != nil {
			return nil, false, err
		}
		for _, event := range history {
			if relocated[sourceID][event.IssueUID] && !seen[event.UID] {
				seen[event.UID] = true
				events = append(events, event)
				added = true
			}
		}
	}
	return events, added, nil
}

// RehomeFederatedMovedIssues preserves local row identities and connector/claim
// state before the existing per-project reconciliation writes the folded view.
func RehomeFederatedMovedIssues(ctx context.Context, tx *sql.Tx, moves FederatedMoveIndex, p FoldProjection, decisions []FederatedMoveDecision, assign func(int64, FoldIssue) (string, error), validate func(int64, int64, int64) error, afterRehome func(string, string) error, applyUnjoined func(int64, FoldIssue, FoldProjection) error, acceptedEventUIDs []string, insertResetControl func(Project, *FederationMovedReference) (int64, error)) error {
	for _, decision := range decisions {
		issue := p.Issues[decision.IssueUID]
		winner := moves.Winners[issue.UID]
		if winner.Type == "issue.snapshot" && !decision.Snapshot {
			winner = moves.Moves[issue.UID]
		}
		target, active := decision.TargetID, decision.Active
		// A detached destination retains standalone data. Its former streams
		// may keep syncing, but cannot reconcile that destination's projection.
		if !decision.Joined {
			delete(p.Issues, issue.UID)
			if !decision.Park {
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
				if winner.Type != "issue.moved" {
					continue
				}
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
			if decision.Park && !active {
				if err := applyUnjoined(target, issue, p); err != nil {
					return err
				}
			}
			continue
		}
		if !decision.Park && !active {
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
		if winner.Type == "issue.snapshot" || sourceProject.UID != fromProject || sourceShort != fromShort {
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
		if decision.Park {
			// Like any issue outside the joined projects, a parked issue keeps
			// no links. Joining its project restores them from history.
			if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE from_issue_id=$1 OR to_issue_id=$1`, id); err != nil {
				return err
			}
			if err := applyUnjoined(target, issue, p); err != nil {
				return err
			}
		}
	}
	return nil
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
 AND NOT (`+FederationHeldEventSQL("e")+`)
 AND e.type IN (`+FederationPushEventTypesSQL()+`))`, projectID, originInstanceUID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrFederationResetBlockedByPendingPush
	}
	if err := PreserveFederationProjectIdentityDependencies(ctx, tx, projectID); err != nil {
		return fmt.Errorf("preserve former project identity dependencies: %w", err)
	}
	// Keep this spoke's edits that a left project never had acknowledged.
	//nolint:gosec // Only the fixed snapshot type list is joined; values are bound.
	_, err = tx.ExecContext(ctx, `DELETE FROM events AS e WHERE e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)
 AND NOT (e.origin_instance_uid=$2 AND e.type NOT IN ('issue.created','issue.moved',`+FederationSnapshotEventTypesSQL()+`) AND (`+FederationHeldEventSQL("e")+` OR (EXISTS (SELECT 1 FROM federation_bindings b WHERE b.project_id=e.project_id AND e.id>b.push_cursor_event_id) AND `+FederationEventReferencesMovedIssueSQL("e")+`)))`, projectID, originInstanceUID)
	return err
}

// FederationHeldEventSQL selects local edits beyond a departed source's cursor.
func FederationHeldEventSQL(alias string) string {
	return alias + `.type NOT IN ('issue.created','issue.moved',` + FederationSnapshotEventTypesSQL() + `)
 AND EXISTS (SELECT 1 FROM projects p JOIN meta h ON h.key='` + FederationHeldCursorPrefix + `' || p.uid
 WHERE p.id=` + alias + `.project_id AND CAST(h.value AS BIGINT)<` + alias + `.id)`
}

// ClearFederatedProjectEvents retains source envelopes still contributing to
// issues now owned elsewhere. A source reset cannot rewind those destinations.
func ClearFederatedProjectEvents(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM events AS e WHERE e.project_id=$1
	 AND NOT (`+FederationEventReferencesMovedIssueSQL("e")+`)
 AND NOT (e.origin_instance_uid=$2 AND `+FederationHeldEventSQL("e")+`)`, projectID, originInstanceUID); err != nil {
		return err
	}
	// Retained envelopes may still refer numerically to a peer being reset.
	// Portable UIDs and content hashes remain intact when local FKs detach.
	if _, err := tx.ExecContext(ctx, `UPDATE events SET issue_id=NULL
 WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET related_issue_id=NULL
 WHERE related_issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, projectID); err != nil {
		return err
	}
	return nil
}
