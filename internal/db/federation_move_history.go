package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"
)

type federationHistoryQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ReadFederationMoveHistory fills a page after excluding move history covered
// by a retained federation baseline. A snapshot can retire a move only after
// its source hub enrollment ends; ordinary destination refreshes must preserve
// active move lineage. An uncovered retired move remains visible until a later
// snapshot covers it. Cutoffs are computed once per issue in this read
// transaction, using bounded UID-indexed lookups rather than scanning the same
// subject history for every event.
func ReadFederationMoveHistory(ctx context.Context, tx *sql.Tx, afterID int64, limit int, readPage func(int64, int) ([]Event, error)) ([]Event, error) {
	events := []Event{}
	cutoffs := map[string]int64{}
	for len(events) < limit {
		const candidateLimit = 1000
		page, err := readPage(afterID, candidateLimit)
		if err != nil {
			return nil, err
		}
		if err := loadFederationMoveHistoryCutoffs(ctx, tx, page, cutoffs); err != nil {
			return nil, err
		}
		for _, event := range page {
			afterID = event.ID
			issueUID := FederationEventIssueUID(event.IssueUID, event.Payload)
			if issueUID != "" && event.ID < cutoffs[issueUID] {
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

func loadFederationMoveHistoryCutoffs(ctx context.Context, tx federationHistoryQuery, page []Event, cutoffs map[string]int64) error {
	var pending []string
	for _, event := range page {
		issueUID := FederationEventIssueUID(event.IssueUID, event.Payload)
		if issueUID == "" {
			continue
		}
		if _, known := cutoffs[issueUID]; !known {
			cutoffs[issueUID] = 0
			pending = append(pending, issueUID)
		}
	}
	for chunk := range slices.Chunk(pending, 500) {
		slots, args := make([]string, len(chunk)), make([]any, len(chunk))
		for i, uid := range chunk {
			slots[i], args[i] = fmt.Sprintf("$%d", i+1), uid
		}
		// Only generated placeholders and fixed backend JSON expressions enter the SQL; UIDs are bound parameters.
		//nolint:gosec // G202: the concatenated SQL fragments contain no user data.
		rows, err := tx.QueryContext(ctx, `SELECT m.issue_uid,m.id,m.type,m.payload FROM events m
 WHERE m.issue_uid IN (`+strings.Join(slots, ",")+`)
 AND (m.type='issue.snapshot' OR m.type='issue.moved')
 ORDER BY m.id`, args...)
		if err != nil {
			return err
		}
		type historyEvent struct {
			issueUID         string
			eventID          int64
			eventType        string
			sourceProjectUID string
			snapshotBearing  bool
		}
		history := make([]historyEvent, 0)
		sourceProjectUIDs := make([]string, 0)
		sourceProjectUIDSet := make(map[string]struct{})
		for rows.Next() {
			var uid, eventType, payload string
			var eventID int64
			if err := rows.Scan(&uid, &eventID, &eventType, &payload); err != nil {
				_ = rows.Close()
				return err
			}
			entry := historyEvent{issueUID: uid, eventID: eventID, eventType: eventType}
			if eventType == "issue.moved" {
				movePayload := PayloadMap(jsontext.Value(payload))
				entry.sourceProjectUID, _ = StringValue(movePayload["from_project_uid"])
				snapshot, exists := movePayload["snapshot"]
				entry.snapshotBearing = exists && strings.TrimSpace(string(snapshot)) != "null"
				if entry.sourceProjectUID != "" {
					if _, exists := sourceProjectUIDSet[entry.sourceProjectUID]; !exists {
						sourceProjectUIDSet[entry.sourceProjectUID] = struct{}{}
						sourceProjectUIDs = append(sourceProjectUIDs, entry.sourceProjectUID)
					}
				}
			}
			history = append(history, entry)
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		activeSources, err := activeFederationHubProjectUIDs(ctx, tx, sourceProjectUIDs)
		if err != nil {
			return err
		}
		uncovered := map[string]bool{}
		for _, event := range history {
			if event.eventType == "issue.moved" {
				// Local moves have no federated snapshot, so the first later
				// federation baseline supersedes them even while their source hub
				// remains active. Snapshot-bearing moves stay in the feed while
				// that source hub is active; a later baseline retires them only
				// after the source enrollment ends.
				uncovered[event.issueUID] = !event.snapshotBearing || event.sourceProjectUID == "" || !activeSources[event.sourceProjectUID]
			} else if uncovered[event.issueUID] {
				cutoffs[event.issueUID] = event.eventID
				uncovered[event.issueUID] = false
			}
		}
	}
	return nil
}

func activeFederationHubProjectUIDs(ctx context.Context, tx federationHistoryQuery, projectUIDs []string) (map[string]bool, error) {
	active := make(map[string]bool, len(projectUIDs))
	for chunk := range slices.Chunk(projectUIDs, 500) {
		slots, args := make([]string, len(chunk)), make([]any, len(chunk))
		for i, uid := range chunk {
			slots[i], args[i] = fmt.Sprintf("$%d", i+1), uid
		}
		// Placeholders are generated locally; project UIDs stay bound as values.
		//nolint:gosec // G202: the concatenated SQL fragments contain no user data.
		rows, err := tx.QueryContext(ctx, `SELECT p.uid FROM projects p
 JOIN federation_bindings b ON b.project_id=p.id
 WHERE p.uid IN (`+strings.Join(slots, ",")+`)
   AND p.deleted_at IS NULL AND b.role='hub' AND b.enabled=1`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err != nil {
				_ = rows.Close()
				return nil, err
			}
			active[uid] = true
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return active, nil
}
