package federation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// Recover original source deltas before applying an inbound move. A project
// cursor can already be newer than those deltas even when creation is retained.
// Using the original clocks avoids treating the move snapshot as a content edit.
func backfillFederationMoveHistory(ctx context.Context, client *Client, projectID int64, page api.PollEventsBody) ([]api.EventEnvelope, error) {
	moves := map[string]api.EventEnvelope{}
	var subjects []string
	seen := map[string]bool{}
	for _, event := range page.Events {
		seen[event.EventUID] = true
		if event.Type != "issue.moved" || event.IssueUID == nil {
			continue
		}
		if _, known := moves[*event.IssueUID]; !known {
			subjects = append(subjects, *event.IssueUID)
			moves[*event.IssueUID] = event
		}
	}
	events := slices.Clone(page.Events)
	for _, uid := range subjects {
		// Arrival order does not determine ownership for concurrent moves.
		// The hub checks current ownership before returning this history.
		history, err := client.issueHistoryThrough(ctx, projectID, uid, page.NextAfterID)
		if err != nil {
			move := moves[uid]
			return nil, fmt.Errorf("backfill issue %s history for moved event project %s event %s: %w", uid, move.ProjectUID, move.EventUID, err)
		}
		for _, event := range history {
			if !seen[event.EventUID] {
				seen[event.EventUID] = true
				events = append(events, event)
			}
		}
	}
	slices.SortStableFunc(events, func(a, b api.EventEnvelope) int { return cmp.Compare(a.EventID, b.EventID) })
	return events, nil
}

func (c *Client) issueHistoryThrough(ctx context.Context, projectID int64, uid string, through int64) ([]api.EventEnvelope, error) {
	var events []api.EventEnvelope
	var cursor int64
	for {
		page, err := c.PollIssueEvents(ctx, projectID, uid, cursor, federationPollLimit)
		if err != nil {
			var status *HubStatusError
			if errors.As(err, &status) && status != nil && status.StatusCode == http.StatusNotFound {
				// This route may now be a historical transit project. Its normal
				// stream still delivers the move out; only the current owner can
				// authorize the independent history read.
				return events, nil
			}
			return nil, err
		}
		if page.ResetRequired {
			return nil, ErrFederationResetRequired
		}
		for _, event := range page.Events {
			if db.EventIssueUID(event.IssueUID) != uid {
				return nil, errors.New("hub did not honor issue history filter")
			}
			if event.EventID <= through {
				events = append(events, event)
			}
		}
		if len(page.Events) == 0 || page.NextAfterID >= through {
			return events, nil
		}
		if page.NextAfterID <= cursor {
			return nil, errors.New("hub issue history cursor did not advance")
		}
		cursor = page.NextAfterID
	}
}
