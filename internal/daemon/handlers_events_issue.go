package daemon

import (
	"context"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// pollIssueHistory keeps history projection separate from stream invalidation:
// omitted scoped rows consume no visible budget and compound links are redacted
// as in UI history. Unrelated events never enter this scan.
//
// History is a read of one subject's rows, not a stream cursor, so it never
// returns the purge-reset envelope: a purge removes only rows naming the purged
// issue, and the next read omits them. Project-scope reset markers stand in for
// rows naming a private peer; history omits them rather than ending there.
func pollIssueHistory(ctx context.Context, store db.Storage, afterID int64, rawLimit api.OptionalInt, issue db.Issue) (*api.PollEventsResponse, error) {
	if afterID < 0 {
		return nil, api.NewError(400, "validation", "after_id must be a non-negative integer", "", nil)
	}
	limit, err := resolveLimit(rawLimit)
	if err != nil {
		return nil, err
	}
	scope := issueScopeFromContext(ctx)
	if scope != nil {
		if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
			return nil, err
		}
	}
	out := &api.PollEventsResponse{IssueUID: issue.UID}
	out.Body.Events = make([]api.EventEnvelope, 0, limit)
	out.Body.NextAfterID = afterID
	for len(out.Body.Events) < limit {
		// Keep batches large enough that omitted rows do not force one
		// membership read per event.
		batchLimit := min(pollLimitMax, max(limit-len(out.Body.Events), 100))
		rows, err := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: out.Body.NextAfterID, ProjectID: issue.ProjectID, IssueUID: issue.UID, Limit: batchLimit,
		})
		if err != nil {
			return nil, internalAPIError(err)
		}
		projected := rows
		visible := make([]bool, len(rows))
		if scope == nil {
			for index := range visible {
				visible[index] = true
			}
		} else {
			allowed, _, err := issueScopedEventMembership(ctx, store, *scope)
			if err != nil {
				return nil, err
			}
			var scopedVisible []bool
			projected, scopedVisible, err = projectIssueScopedEvents(ctx, store, rows, allowed, scope.ProjectUID)
			if err != nil {
				return nil, internalAPIError(err)
			}
			copy(visible, scopedVisible)
		}
		for index, event := range rows {
			out.Body.NextAfterID = event.ID
			if visible[index] && event.Type != db.ProjectScopeResetEventType {
				out.Body.Events = append(out.Body.Events, toEnvelopes(projected[index:index+1])...)
				if len(out.Body.Events) == limit {
					break
				}
			}
		}
		if len(rows) < batchLimit {
			break
		}
	}
	return out, nil
}
