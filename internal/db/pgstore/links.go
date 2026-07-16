package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

const linkSelect = `SELECT id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at FROM links`

var linkConstraintErrors = map[string]error{
	"links_unique_edge":                        db.ErrLinkExists,
	"links_from_issue_id_to_issue_id_type_key": db.ErrLinkExists,
	"links_not_self_check":                     db.ErrSelfLink,
	"links_check":                              db.ErrSelfLink,
	"uniq_one_parent_per_child":                db.ErrParentAlreadySet,
}

// CreateLink inserts one relationship edge without an event.
func (s *Store) CreateLink(ctx context.Context, params db.CreateLinkParams) (db.Link, error) {
	var link db.Link
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if err := prepareLinkInsertTx(ctx, tx, params, false); err != nil {
			return err
		}
		var err error
		link, err = insertLinkTx(ctx, tx, params)
		return err
	})
	return link, err
}

// CreateLinkAndEvent inserts a relationship, records its event, and touches
// the issue on whose surface the mutation was requested.
func (s *Store) CreateLinkAndEvent(
	ctx context.Context,
	params db.CreateLinkParams,
	eventParams db.LinkEventParams,
) (db.Link, db.Event, error) {
	var link db.Link
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		eventIssue, project, err := lockedIssueTx(ctx, tx, eventParams.EventIssueID, false)
		if err != nil {
			return err
		}
		actor := strings.TrimSpace(eventParams.Actor)
		if actor == "" {
			actor = params.Author
		}
		actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, actor)
		if err != nil {
			return err
		}
		params.Author = actor
		eventParams.Actor = actor
		if err := prepareLinkInsertTx(ctx, tx, params, true); err != nil {
			return err
		}
		link, err = insertLinkTx(ctx, tx, params)
		if err != nil {
			return err
		}
		relatedID, relatedUID := linkPeer(link, eventIssue.ID)
		updatedAt := nowStoredTimestamp()
		payload, err := json.Marshal(map[string]any{
			"link_id":       link.ID,
			"type":          link.Type,
			"from_short_id": eventParams.FromShortID,
			"from_uid":      eventParams.FromUID,
			"to_short_id":   eventParams.ToShortID,
			"to_uid":        eventParams.ToUID,
			"updated_at":    updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal link payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &eventIssue.ID, IssueUID: &eventIssue.UID,
			RelatedIssueID: &relatedID, RelatedIssueUID: &relatedUID,
			Type: eventParams.EventType, Actor: eventParams.Actor, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, updatedAt, eventIssue.ID)
		return mapSQLError(err, nil)
	})
	return link, event, err
}

// DeleteLinkByID removes one edge without emitting an event.
func (s *Store) DeleteLinkByID(ctx context.Context, linkID int64) error {
	return s.RetryTransient(ctx, func() error {
		result, err := s.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, linkID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return db.ErrNotFound
		}
		return nil
	})
}

// DeleteLinkAndEvent removes an edge and records the unlink atomically.
func (s *Store) DeleteLinkAndEvent(
	ctx context.Context,
	link db.Link,
	eventParams db.LinkEventParams,
) (db.Event, error) {
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		eventIssue, project, err := lockedIssueTx(ctx, tx, eventParams.EventIssueID, false)
		if err != nil {
			return err
		}
		eventParams.Actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, eventParams.Actor)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, link.ID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return db.ErrNotFound
		}
		relatedID, relatedUID := linkPeer(link, eventIssue.ID)
		updatedAt := nowStoredTimestamp()
		payload, err := json.Marshal(map[string]any{
			"link_id":       link.ID,
			"type":          link.Type,
			"from_short_id": eventParams.FromShortID,
			"from_uid":      eventParams.FromUID,
			"to_short_id":   eventParams.ToShortID,
			"to_uid":        eventParams.ToUID,
			"link_from_uid": link.FromIssueUID,
			"link_to_uid":   link.ToIssueUID,
			"updated_at":    updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal unlink payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &eventIssue.ID, IssueUID: &eventIssue.UID,
			RelatedIssueID: &relatedID, RelatedIssueUID: &relatedUID,
			Type: eventParams.EventType, Actor: eventParams.Actor, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, updatedAt, eventIssue.ID)
		return mapSQLError(err, nil)
	})
	return event, err
}

// LinkByID fetches one edge by row identity.
func (s *Store) LinkByID(ctx context.Context, id int64) (db.Link, error) {
	return scanLink(s.QueryRowContext(ctx, linkSelect+` WHERE id = $1`, id))
}

// LinkByEndpoints fetches one exact directed edge.
func (s *Store) LinkByEndpoints(
	ctx context.Context,
	fromIssueID int64,
	toIssueID int64,
	linkType string,
) (db.Link, error) {
	return scanLink(s.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = $1 AND to_issue_id = $2 AND type = $3`,
		fromIssueID, toIssueID, linkType))
}

// LinksByIssue returns every edge involving an issue in insertion order.
func (s *Store) LinksByIssue(ctx context.Context, issueID int64) ([]db.Link, error) {
	rows, err := s.QueryContext(ctx,
		linkSelect+` WHERE from_issue_id = $1 OR to_issue_id = $1 ORDER BY id ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var links []db.Link
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, mapSQLError(rows.Err(), nil)
}

// ParentOf returns the single parent edge for a child.
func (s *Store) ParentOf(ctx context.Context, childIssueID int64) (db.Link, error) {
	return scanLink(s.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = $1 AND type = 'parent'`, childIssueID))
}

func insertLinkTx(ctx context.Context, tx *sql.Tx, params db.CreateLinkParams) (db.Link, error) {
	link, err := scanLink(tx.QueryRowContext(ctx, `INSERT INTO links(
			from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author
		)
		SELECT $1, $2, from_issue.uid, to_issue.uid, $3, $4
		  FROM issues from_issue, issues to_issue
		 WHERE from_issue.id = $1 AND to_issue.id = $2
		RETURNING id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at`,
		params.FromIssueID, params.ToIssueID, params.Type, params.Author))
	return link, mapSQLError(err, linkConstraintErrors)
}

func prepareLinkInsertTx(
	ctx context.Context,
	tx *sql.Tx,
	params db.CreateLinkParams,
	checkParentCycle bool,
) error {
	if params.FromIssueID == params.ToIssueID {
		return db.ErrSelfLink
	}
	if params.Type != "parent" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashint8($1))`, params.FromIssueID); err != nil {
		return mapSQLError(err, nil)
	}
	var currentParentID int64
	err := tx.QueryRowContext(ctx,
		`SELECT to_issue_id FROM links WHERE from_issue_id = $1 AND type = 'parent'`,
		params.FromIssueID,
	).Scan(&currentParentID)
	if err == nil {
		if currentParentID == params.ToIssueID {
			return db.ErrLinkExists
		}
		return db.ErrParentAlreadySet
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return mapSQLError(err, nil)
	}
	if checkParentCycle {
		return assertNoParentCycleTx(ctx, tx, params.FromIssueID, params.ToIssueID)
	}
	return nil
}

func assertNoParentCycleTx(ctx context.Context, tx *sql.Tx, childID, parentID int64) error {
	current := parentID
	for range db.MaxParentDepth {
		if current == childID {
			return db.ErrParentCycle
		}
		var next int64
		err := tx.QueryRowContext(ctx,
			`SELECT to_issue_id FROM links WHERE from_issue_id = $1 AND type = 'parent'`, current).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		current = next
	}
	return fmt.Errorf("parent chain exceeds depth limit %d", db.MaxParentDepth)
}

func linkPeer(link db.Link, eventIssueID int64) (int64, string) {
	if link.ToIssueID == eventIssueID {
		return link.FromIssueID, link.FromIssueUID
	}
	return link.ToIssueID, link.ToIssueUID
}

func scanLink(row rowScanner) (db.Link, error) {
	var link db.Link
	var createdAt string
	err := row.Scan(
		&link.ID, &link.FromIssueID, &link.FromIssueUID, &link.ToIssueID, &link.ToIssueUID,
		&link.Type, &link.Author, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Link{}, db.ErrNotFound
	}
	if err != nil {
		return db.Link{}, mapSQLError(err, nil)
	}
	link.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.Link{}, fmt.Errorf("parse link created_at: %w", err)
	}
	return link, nil
}
