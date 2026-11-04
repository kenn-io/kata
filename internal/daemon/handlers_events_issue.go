package daemon

import (
	"context"
	"encoding/json/jsontext"
	"fmt"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// pollIssueHistory keeps history projection separate from stream invalidation:
// omitted scoped rows consume no visible budget and compound links are redacted
// as in UI history. Unrelated events never enter this scan. Historical rows
// remain in their original project after a move; EventsAfter enforces the
// caller's authorized projects while selecting this subject across projects.
//
// History is a read of one subject's rows, not a stream cursor, so it never
// returns the purge-reset envelope: a purge removes only rows naming the purged
// issue, and the next read omits them. Project-scope reset markers stand in for
// rows naming a private peer; history omits them rather than ending there.
func pollIssueHistory(ctx context.Context, store db.Storage, afterID int64, rawLimit api.OptionalInt, issue db.Issue, includeEffectiveCloseEventUID bool) (*api.PollEventsResponse, error) {
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
	if includeEffectiveCloseEventUID {
		uid, err := effectiveIssueCloseEventUID(ctx, store, issue)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out.Body.EffectiveCloseEventUID = uid
	}
	out.Body.Events = make([]api.EventEnvelope, 0, limit)
	out.Body.NextAfterID = afterID
	for len(out.Body.Events) < limit {
		// Keep batches large enough that omitted rows do not force one
		// membership read per event.
		batchLimit := min(pollLimitMax, max(limit-len(out.Body.Events), 100))
		rows, err := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: out.Body.NextAfterID, IssueUID: issue.UID, Limit: batchLimit,
		})
		if err != nil {
			return nil, internalAPIError(err)
		}
		rows = attachResolvedIssueID(issue, rows)
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
	if len(out.Body.Events) > 0 {
		names, err := store.ListFederationSpokeNames(ctx)
		if err != nil {
			return nil, internalAPIError(err)
		}
		for i := range out.Body.Events {
			out.Body.Events[i].OriginSpokeName = names[out.Body.Events[i].OriginInstanceUID]
		}
	}
	return out, nil
}

// effectiveIssueCloseEventUID uses the same HLC fold as federation to select
// native close intent. Scoped history deliberately redacts origin identity and
// status transitions, so clients cannot safely re-derive this choice.
func effectiveIssueCloseEventUID(ctx context.Context, store db.Storage, issue db.Issue) (string, error) {
	scope := issueScopeFromContext(ctx)
	projectUIDs, projectRestricted := db.AuthorizedProjects(ctx)
	visibleProjects := make(map[string]struct{}, len(projectUIDs))
	for _, projectUID := range projectUIDs {
		visibleProjects[projectUID] = struct{}{}
	}
	var allowed map[int64]struct{}
	if scope != nil {
		if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
			return "", err
		}
		var err error
		allowed, _, err = issueScopedEventMembership(ctx, store, *scope)
		if err != nil {
			return "", err
		}
	}

	const batchLimit = pollLimitMax
	afterID := int64(0)
	var foldEvents []db.FoldEvent
	visibleCloseUIDs := make(map[string]struct{})
	eventIssueLookup := newProjectScopeEventIssueLookup(ctx, store)
	// Status provenance must include transitions in projects the caller cannot
	// inspect. Only a selected close event whose project, issue scope, and
	// structured references are visible may be returned to the caller.
	// Hidden scan endpoints must not become response release dependencies.
	historyCtx := db.WithExposedIssues(db.WithAuthorizedProjects(ctx, nil))
	for {
		rows, err := store.EventsAfter(historyCtx, db.EventsAfterParams{
			AfterID: afterID, IssueUID: issue.UID, Limit: batchLimit,
		})
		if err != nil {
			return "", err
		}
		rows = attachResolvedIssueID(issue, rows)
		previousAfterID := afterID
		for _, event := range rows {
			visible := !projectRestricted
			if projectRestricted {
				_, visible = visibleProjects[event.ProjectUID]
			}
			if visible && scope != nil {
				_, visible = projectIssueScopedEvent(event, allowed, scope.ProjectUID)
			}
			if visible && event.Type == "issue.closed" {
				requiresReset, err := db.EventRequiresProjectScopeReset(
					ctx, event, eventIssueLookup.IssueByUID, eventIssueLookup.IssueByRef,
				)
				if err != nil {
					return "", err
				}
				visible = !requiresReset
			}
			foldEvents = append(foldEvents, db.FoldEvent{
				UID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
				ProjectUID: event.ProjectUID, IssueUID: issue.UID,
				Type: event.Type, Actor: event.Actor,
				HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter,
				CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat),
				Payload:   jsontext.Value(event.Payload),
			})
			if visible && event.Type == "issue.closed" {
				visibleCloseUIDs[event.UID] = struct{}{}
			}
			afterID = event.ID
		}
		if len(rows) < batchLimit {
			break
		}
		if afterID <= previousAfterID {
			return "", fmt.Errorf("issue history cursor did not advance")
		}
	}

	state := db.FoldEvents(foldEvents).Issues[issue.UID]
	if state.Status != "closed" || state.StatusIntentUID == "" {
		return "", nil
	}
	if _, ok := visibleCloseUIDs[state.StatusIntentUID]; !ok {
		return "", nil
	}
	return state.StatusIntentUID, nil
}

// attachResolvedIssueID lets the scoped projection authorize federated history
// rows whose stable issue UID is present but whose local issue_id is NULL. The
// history query already resolved and filtered this exact subject in the active
// project, so the local row ID is the principal's authorized issue identity.
func attachResolvedIssueID(issue db.Issue, events []db.Event) []db.Event {
	for index := range events {
		event := &events[index]
		if event.IssueID == nil && event.IssueUID != nil && *event.IssueUID == issue.UID {
			issueID := issue.ID
			event.IssueID = &issueID
		}
	}
	return events
}
