package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

const eventSelect = `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
       e.issue_id, e.issue_uid, i.short_id, e.related_issue_id, e.related_issue_uid, ri.short_id,
       e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at
  FROM events e
  JOIN projects p ON p.id = e.project_id
  LEFT JOIN issues i ON i.id = e.issue_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL AND i.uid = e.issue_uid)
  LEFT JOIN issues ri ON ri.id = e.related_issue_id OR (e.related_issue_id IS NULL AND e.related_issue_uid IS NOT NULL AND ri.uid = e.related_issue_uid)`

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

// MaxEventID returns the current event high-water mark.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var value sql.NullInt64
	if err := s.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&value); err != nil {
		return 0, mapSQLError(err, nil)
	}
	return value.Int64, nil
}

// EventsAfter returns an ascending, bounded event page.
func (s *Store) EventsAfter(ctx context.Context, params db.EventsAfterParams) ([]db.Event, error) {
	conditions := []string{"e.id > $1", "p.name <> $2"}
	args := []any{params.AfterID, db.SystemProjectName}
	if params.ProjectID != 0 {
		args = append(args, params.ProjectID)
		conditions = append(conditions, fmt.Sprintf("e.project_id = $%d", len(args)))
	}
	if params.ThroughID != 0 {
		args = append(args, params.ThroughID)
		conditions = append(conditions, fmt.Sprintf("e.id <= $%d", len(args)))
	}
	args = append(args, params.Limit)
	query := eventSelect + ` WHERE ` + strings.Join(conditions, " AND ") +
		fmt.Sprintf(` ORDER BY e.id ASC LIMIT $%d`, len(args))
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var events []db.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	return events, nil
}

func eventsAfterTx(ctx context.Context, tx *sql.Tx, afterID int64) ([]db.Event, error) {
	rows, err := tx.QueryContext(ctx, eventSelect+` WHERE e.id > $1 ORDER BY e.id ASC`, afterID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var events []db.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, mapSQLError(rows.Err(), nil)
}

// EventsByUIDs resolves the requested event identities in caller order.
func (s *Store) EventsByUIDs(ctx context.Context, projectID int64, uids []string) ([]db.Event, error) {
	events := make([]db.Event, 0, len(uids))
	for _, uid := range uids {
		event, err := scanEvent(s.QueryRowContext(ctx,
			eventSelect+` WHERE e.project_id = $1 AND e.uid = $2`, projectID, uid))
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// EventsInWindow returns events in the inclusive timestamp window.
func (s *Store) EventsInWindow(ctx context.Context, params db.EventsInWindowParams) ([]db.Event, error) {
	conditions := []string{"e.created_at >= $1", "e.created_at <= $2", "p.name <> $3"}
	args := []any{params.Since, params.Until, db.SystemProjectName}
	if params.ProjectID != 0 {
		args = append(args, params.ProjectID)
		conditions = append(conditions, fmt.Sprintf("e.project_id = $%d", len(args)))
	}
	if len(params.Actors) > 0 {
		positions := make([]string, 0, len(params.Actors))
		for _, actor := range params.Actors {
			args = append(args, actor)
			positions = append(positions, fmt.Sprintf("$%d", len(args)))
		}
		conditions = append(conditions, "e.actor IN ("+strings.Join(positions, ",")+")")
	}
	rows, err := s.QueryContext(ctx,
		eventSelect+` WHERE `+strings.Join(conditions, " AND ")+` ORDER BY e.id ASC`, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var events []db.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, mapSQLError(rows.Err(), nil)
}

// MaxLocalOriginEventID returns the newest locally-originated project event.
func (s *Store) MaxLocalOriginEventID(ctx context.Context, projectID int64) (int64, error) {
	var value sql.NullInt64
	if err := s.QueryRowContext(ctx, `SELECT MAX(id) FROM events
      WHERE project_id = $1 AND origin_instance_uid = $2`, projectID, s.instanceUID).Scan(&value); err != nil {
		return 0, mapSQLError(err, nil)
	}
	return value.Int64, nil
}

// MaxFederationBaselineEventID returns the newest snapshot event at or after a cursor.
func (s *Store) MaxFederationBaselineEventID(ctx context.Context, projectID, sinceEventID int64) (int64, error) {
	var value sql.NullInt64
	if err := s.QueryRowContext(ctx, `SELECT MAX(id) FROM events
      WHERE project_id = $1 AND type = 'issue.snapshot' AND id >= $2`, projectID, sinceEventID).Scan(&value); err != nil {
		return 0, mapSQLError(err, nil)
	}
	return value.Int64, nil
}

// InsertCloseThrottledEvent records an audit event for a refused close.
func (s *Store) InsertCloseThrottledEvent(
	ctx context.Context,
	issueID int64,
	actor string,
	payload db.CloseThrottledPayload,
) (db.Event, error) {
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal close.throttled payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "close.throttled", actor, string(body)))
		return err
	})
	return event, err
}

// RecentSiblingCloses returns recent closes by one actor on direct siblings
// under an active parent relationship.
func (s *Store) RecentSiblingCloses(
	ctx context.Context,
	parentIssueID int64,
	excludeIssueID int64,
	actor string,
	since time.Time,
) ([]db.Event, error) {
	rows, err := s.QueryContext(ctx, eventSelect+`
  JOIN links sibling_link ON sibling_link.from_issue_id = e.issue_id
  JOIN projects child_project ON child_project.id = i.project_id
 WHERE e.type = 'issue.closed'
   AND e.actor = $1
   AND e.created_at >= $2
   AND sibling_link.type = 'parent'
   AND sibling_link.to_issue_id = $3
   AND e.issue_id <> $4
   AND child_project.deleted_at IS NULL
 ORDER BY e.created_at DESC, e.id DESC`,
		actor, formatStoredTime(since), parentIssueID, excludeIssueID,
	)
	if err != nil {
		return nil, fmt.Errorf("recent sibling closes: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var events []db.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent sibling closes: %w", mapSQLError(err, nil))
	}
	return events, nil
}

// RecentSameMessageClose finds the newest qualifying sibling close with the
// same normalized prose.
func (s *Store) RecentSameMessageClose(
	ctx context.Context,
	parentIssueID int64,
	excludeIssueID int64,
	actor string,
	normalizedMessage string,
	since time.Time,
) (*db.Event, error) {
	events, err := s.RecentSiblingCloses(ctx, parentIssueID, excludeIssueID, actor, since)
	if err != nil {
		return nil, err
	}
	for index := range events {
		var payload struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(events[index].Payload), &payload); err != nil {
			continue
		}
		if payload.Reason != "done" && payload.Reason != "audit-no-change" {
			continue
		}
		if db.NormalizeCloseMessage(payload.Message) == normalizedMessage {
			return &events[index], nil
		}
	}
	return nil, nil
}

func (s *Store) insertEventTx(ctx context.Context, tx *sql.Tx, input eventInsert) (db.Event, error) {
	eventUID := input.UID
	var err error
	if eventUID == "" {
		eventUID, err = katauid.New()
		if err != nil {
			return db.Event{}, fmt.Errorf("generate event uid: %w", err)
		}
	}
	originInstanceUID := input.OriginInstanceUID
	if originInstanceUID == "" {
		originInstanceUID = s.instanceUID
	}
	createdAt := input.CreatedAt
	if createdAt == "" {
		createdAt = nowStoredTimestamp()
	}
	actor := input.Actor
	if input.ContentHash == "" && originInstanceUID == s.instanceUID {
		actor, err = effectiveLocalEventActorTx(ctx, tx, input.ProjectID, actor)
		if err != nil {
			return db.Event{}, err
		}
	}
	// Local HLC values must advance even when unrelated projects emit events
	// concurrently. Scope the advisory key to this schema so isolated stores
	// sharing one database do not serialize each other.
	if err := lockEventSequenceTx(ctx, tx); err != nil {
		return db.Event{}, err
	}
	var hlc db.EventHLCTimestamp
	if input.HLC != nil {
		hlc = *input.HLC
	} else {
		hlc, err = nextEventHLCTx(ctx, tx, time.Now().UTC())
		if err != nil {
			return db.Event{}, err
		}
	}
	hash := input.ContentHash
	if hash == "" {
		hash, err = db.EventContentHash(db.EventHashInput{
			UID: eventUID, OriginInstanceUID: originInstanceUID,
			ProjectUID: input.ProjectUID, ProjectName: input.ProjectName,
			IssueUID: input.IssueUID, RelatedIssueUID: input.RelatedIssueUID,
			Type: input.Type, Actor: actor, HLCPhysicalMS: hlc.PhysicalMS,
			HLCCounter: hlc.Counter, CreatedAt: createdAt,
			Payload: json.RawMessage(input.Payload),
		})
		if err != nil {
			return db.Event{}, fmt.Errorf("hash event content: %w", err)
		}
	}
	var eventID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO events(
		  uid, origin_instance_uid, project_id, project_name, issue_id, issue_uid,
		  related_issue_id, related_issue_uid, type, actor, payload,
		  hlc_physical_ms, hlc_counter, content_hash, created_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		eventUID, originInstanceUID, input.ProjectID, input.ProjectName, input.IssueID, input.IssueUID,
		input.RelatedIssueID, input.RelatedIssueUID, input.Type, actor, input.Payload,
		hlc.PhysicalMS, hlc.Counter, hash, createdAt,
	).Scan(&eventID)
	if err != nil {
		return db.Event{}, mapSQLError(err, nil)
	}
	return scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id = $1`, eventID))
}

func effectiveLocalEventActorTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	requestedActor string,
) (string, error) {
	return effectiveLocalMutationActorTx(ctx, tx, projectID, requestedActor)
}

func effectiveLocalMutationActorTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	requestedActor string,
) (string, error) {
	var actor string
	err := tx.QueryRowContext(ctx, `SELECT bound_actor FROM federation_bindings
WHERE project_id=$1 AND role=$2 AND enabled=1 AND push_enabled=1`,
		projectID, string(db.FederationRoleSpoke)).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return requestedActor, nil
	}
	if err != nil {
		return "", mapSQLError(err, nil)
	}
	if actor = strings.TrimSpace(actor); actor != "" {
		return actor, nil
	}
	return requestedActor, nil
}

func lockEventSequenceTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashtext('events_sequence'))`); err != nil {
		return mapSQLError(err, nil)
	}
	return nil
}

func nextEventHLCTx(ctx context.Context, tx *sql.Tx, now time.Time) (db.EventHLCTimestamp, error) {
	var last db.EventHLCTimestamp
	err := tx.QueryRowContext(ctx, `SELECT hlc_physical_ms, hlc_counter FROM events
      ORDER BY hlc_physical_ms DESC, hlc_counter DESC LIMIT 1`).Scan(&last.PhysicalMS, &last.Counter)
	if errors.Is(err, sql.ErrNoRows) {
		return db.NextEventHLCValue(db.EventHLCTimestamp{}, now), nil
	}
	if err != nil {
		return db.EventHLCTimestamp{}, mapSQLError(err, nil)
	}
	return db.NextEventHLCValue(last, now), nil
}

func scanEvent(row rowScanner) (db.Event, error) {
	var event db.Event
	var createdAt string
	err := row.Scan(
		&event.ID, &event.UID, &event.OriginInstanceUID, &event.ProjectID, &event.ProjectUID, &event.ProjectName,
		&event.IssueID, &event.IssueUID, &event.IssueShortID, &event.RelatedIssueID, &event.RelatedIssueUID,
		&event.RelatedIssueShortID, &event.Type, &event.Actor, &event.Payload, &event.HLCPhysicalMS,
		&event.HLCCounter, &event.ContentHash, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Event{}, db.ErrNotFound
	}
	if err != nil {
		return db.Event{}, mapSQLError(err, nil)
	}
	event.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.Event{}, fmt.Errorf("parse event created_at: %w", err)
	}
	return event, nil
}
