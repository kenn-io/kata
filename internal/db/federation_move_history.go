package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

type federationHistoryQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ReadFederationMoveHistory fills a page after excluding local move history
// covered by a retained federation baseline. Each standalone move waits for
// its first covering snapshot; refreshes without another standalone move cannot
// hide later deltas, and an uncovered trailing move remains visible. Cutoffs are
// computed once per issue in this read transaction, using bounded UID-indexed
// lookups rather than scanning the same subject history for every event.
func ReadFederationMoveHistory(ctx context.Context, tx *sql.Tx, afterID int64, limit int, snapshotExpression string, readPage func(int64, int) ([]Event, error)) ([]Event, error) {
	events := []Event{}
	cutoffs := map[string]int64{}
	for len(events) < limit {
		const candidateLimit = 1000
		page, err := readPage(afterID, candidateLimit)
		if err != nil {
			return nil, err
		}
		if err := loadFederationMoveHistoryCutoffs(ctx, tx, page, cutoffs, snapshotExpression); err != nil {
			return nil, err
		}
		for _, event := range page {
			afterID = event.ID
			if event.IssueUID != nil && event.ID < cutoffs[*event.IssueUID] {
				continue
			}
			events = append(events, event)
			if len(events) == limit {
				return events, nil
			}
		}
		if len(page) < candidateLimit {
			break
		}
	}
	return events, nil
}

func loadFederationMoveHistoryCutoffs(ctx context.Context, tx federationHistoryQuery, page []Event, cutoffs map[string]int64, snapshotExpression string) error {
	var pending []string
	for _, event := range page {
		if event.IssueUID == nil {
			continue
		}
		if _, known := cutoffs[*event.IssueUID]; !known {
			cutoffs[*event.IssueUID] = 0
			pending = append(pending, *event.IssueUID)
		}
	}
	for chunk := range slices.Chunk(pending, 500) {
		slots, args := make([]string, len(chunk)), make([]any, len(chunk))
		for i, uid := range chunk {
			slots[i], args[i] = fmt.Sprintf("$%d", i+1), uid
		}
		// Only generated placeholders and fixed backend JSON expressions enter the SQL; UIDs are bound parameters.
		//nolint:gosec // G202: the concatenated SQL fragments contain no user data.
		rows, err := tx.QueryContext(ctx, `SELECT m.issue_uid,m.id,m.type FROM events m
 WHERE m.issue_uid IN (`+strings.Join(slots, ",")+`)
 AND (m.type='issue.snapshot' OR (m.type='issue.moved' AND `+snapshotExpression+` IS NULL))
 ORDER BY m.id`, args...)
		if err != nil {
			return err
		}
		uncovered := map[string]bool{}
		for rows.Next() {
			var uid, eventType string
			var eventID int64
			if err := rows.Scan(&uid, &eventID, &eventType); err != nil {
				_ = rows.Close()
				return err
			}
			if eventType == "issue.moved" {
				uncovered[uid] = true
			} else if uncovered[uid] {
				cutoffs[uid] = eventID
				uncovered[uid] = false
			}
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
