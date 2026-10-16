package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// ReadFederationMoveHistory fills a page after excluding local move history
// covered by the first retained federation baseline. Later snapshots cannot
// hide later deltas or invalid moves. Cutoffs are computed once per issue in
// this read transaction, using bounded UID-indexed lookups rather than a scan
// of the same subject history for every event.
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

func loadFederationMoveHistoryCutoffs(ctx context.Context, tx *sql.Tx, page []Event, cutoffs map[string]int64, snapshotExpression string) error {
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
		rows, err := tx.QueryContext(ctx, `SELECT history_baseline.issue_uid,history_baseline.baseline_id FROM (
 SELECT issue_uid,MIN(id) AS baseline_id FROM events
 WHERE issue_uid IN (`+strings.Join(slots, ",")+`) AND type='issue.snapshot'
 GROUP BY issue_uid) history_baseline
 WHERE EXISTS (
 SELECT 1 FROM events m WHERE m.issue_uid=history_baseline.issue_uid AND m.type='issue.moved'
 AND m.id<history_baseline.baseline_id AND `+snapshotExpression+` IS NULL)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var uid string
			var cutoff int64
			if err := rows.Scan(&uid, &cutoff); err != nil {
				_ = rows.Close()
				return err
			}
			cutoffs[uid] = cutoff
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
