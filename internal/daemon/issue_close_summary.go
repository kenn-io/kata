package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// currentIssueCloseSummary projects only explicit provenance for the current
// closure. Snapshot state alone cannot identify a closer. The ordinary fold
// supplies portable writer ordering, including delayed federation delivery.
func currentIssueCloseSummary(ctx context.Context, store db.Storage, issue db.Issue) (*api.IssueCloseSummary, error) {
	if issue.Status != "closed" {
		return nil, nil
	}
	through, err := store.MaxEventID(ctx)
	if err != nil {
		return nil, err
	}
	if through == 0 {
		return nil, nil
	}
	scope := issueScopeFromContext(ctx)
	var allowed map[int64]struct{}
	if scope != nil {
		if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
			return nil, err
		}
		allowed, _, err = issueScopedEventMembership(ctx, store, *scope)
		if err != nil {
			return nil, err
		}
	}
	var history []db.FoldEvent
	closes := map[string]*api.IssueCloseSummary{}
	var after int64
	for after < through {
		// Stable UID follows moves; EventsAfter retains the caller's original
		// project/host authorization on historical source projects. A move does
		// not grant access to source history solely through the destination.
		page, err := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: after, ThroughID: through, IssueUID: issue.UID,
			RequireIssueUpdatedStatus: true,
			Types:                     []string{"issue.created", "issue.snapshot", "issue.updated", "issue.closed", "issue.reopened", "issue.moved"},
			Limit:                     256,
		})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			after = event.ID
			// Move rows carry the storage layer's existing reset markers when
			// a lifecycle crossed an unreadable project. An older visible
			// close cannot prove the current cycle, even at the same instant.
			if event.Type == db.ProjectScopeResetEventType {
				return nil, nil
			}
			if event.Type == "issue.moved" {
				continue
			}
			visible, err := issueCloseSummaryEventVisible(ctx, store, event, scope, allowed)
			if err != nil {
				return nil, err
			}
			if !visible {
				return nil, nil
			}
			// Retain lifecycle state only. Snapshot comments, links and ordinary edit
			// bodies do not belong in this read projection or its memory footprint.
			var fields map[string]jsontext.Value
			if err := json.Unmarshal([]byte(event.Payload), &fields); err != nil {
				return nil, err
			}
			status := map[string]jsontext.Value{}
			for _, key := range []string{"uid", "issue_uid", "status", "closed_at", "closed_reason", "reason"} {
				if value, ok := fields[key]; ok {
					status[key] = value
				}
			}
			if event.Type == "issue.updated" {
				if _, ok := status["status"]; !ok {
					continue
				}
			}
			payload, err := json.Marshal(status)
			if err != nil {
				return nil, err
			}
			history = append(history, db.FoldEvent{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: issue.UID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), Payload: jsontext.Value(payload)})
			if event.Type == "issue.closed" {
				// Fold original status carriers internally, but expose provenance only
				// for a close the normal scoped event projection would reveal. A moved
				// issue's old-project close is outside a destination-subtree credential.
				if scope != nil {
					if _, visible := projectIssueScopedEvent(event, allowed, scope.ProjectUID); !visible {
						continue
					}
				}
				reason := closeReasonOf(event.Payload)
				if reason == "" {
					reason = "done"
				}
				closes[event.UID] = &api.IssueCloseSummary{EventID: event.ID, Actor: event.Actor, Teammate: event.Teammate, CreatedAt: event.CreatedAt, Reason: reason}
			}
		}
		if len(page) < 256 {
			break
		}
	}
	folded := db.FoldEvents(history).Issues[issue.UID]
	if folded.Status != "closed" || folded.StatusIntentUID == "" {
		return nil, nil
	}
	// Filtered history must also agree with the current materialized closure;
	// an older readable cycle cannot stand in for a different current closure.
	// Timestamp agreement is only a consistency check after the history guard.
	if issue.ClosedAt != nil {
		if folded.ClosedAt == nil {
			return nil, nil
		}
		closedAt, err := time.Parse(time.RFC3339Nano, *folded.ClosedAt)
		if err != nil || !closedAt.Equal(*issue.ClosedAt) {
			return nil, nil
		}
	}
	return closes[folded.StatusIntentUID], nil
}

// issueCloseSummaryEventVisible uses the scoped membership snapshot already
// loaded for this history scan. The outer response fence rechecks every issue
// target before the summary is released, so lifecycle pages do not need one
// IssueByID and IssueInScope round trip per event. Notification references
// still get their event-specific visibility check.
func issueCloseSummaryEventVisible(
	ctx context.Context,
	store db.Storage,
	event db.Event,
	scope *db.APITokenScope,
	allowed map[int64]struct{},
) (bool, error) {
	if scope != nil {
		if event.IssueID == nil {
			return false, nil
		}
		ids := []*int64{event.IssueID}
		if event.RelatedIssueID != nil {
			ids = append(ids, event.RelatedIssueID)
		}
		for _, issueID := range ids {
			if _, ok := allowed[*issueID]; !ok {
				return false, nil
			}
			db.RecordIssueScopeTarget(ctx, *issueID)
		}
	}
	return notificationEventStillVisible(ctx, store, event)
}
