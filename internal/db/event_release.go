package db

import (
	"context"
	"errors"
	"fmt"
)

// EventIssueLookup resolves the issues one event read names. It memoizes each
// lookup, including not-found results, so a page that names the same issue
// many times resolves it once. Both lookups must honor the caller's project
// boundary, so a hidden issue resolves as not found and is never recorded.
type EventIssueLookup struct {
	byUID     func(string) (Issue, error)
	byRef     func(string, string) (Issue, error)
	uidIssues map[string]Issue
	uidMissed map[string]struct{}
	refIssues map[string]Issue
	refMissed map[string]struct{}
}

// NewEventIssueLookup memoizes byUID and byRef for one event read.
func NewEventIssueLookup(
	byUID func(string) (Issue, error), byRef func(string, string) (Issue, error),
) *EventIssueLookup {
	return &EventIssueLookup{
		byUID: byUID, byRef: byRef,
		uidIssues: make(map[string]Issue), uidMissed: make(map[string]struct{}),
		refIssues: make(map[string]Issue), refMissed: make(map[string]struct{}),
	}
}

// IssueByUID resolves an issue UID, memoizing the result.
func (l *EventIssueLookup) IssueByUID(uid string) (Issue, error) {
	return memoizedIssue(l.uidIssues, l.uidMissed, uid, func() (Issue, error) { return l.byUID(uid) })
}

// IssueByRef resolves a reference written in projectUID, memoizing the result.
func (l *EventIssueLookup) IssueByRef(projectUID, ref string) (Issue, error) {
	return memoizedIssue(l.refIssues, l.refMissed, projectUID+"\x00"+ref,
		func() (Issue, error) { return l.byRef(projectUID, ref) })
}

func memoizedIssue(
	found map[string]Issue, missed map[string]struct{}, key string, lookup func() (Issue, error),
) (Issue, error) {
	if issue, ok := found[key]; ok {
		return issue, nil
	}
	if _, ok := missed[key]; ok {
		return Issue{}, ErrNotFound
	}
	issue, err := lookup()
	if errors.Is(err, ErrNotFound) {
		missed[key] = struct{}{}
	}
	if err == nil {
		found[key] = issue
	}
	return issue, err
}

// RequiresProjectScopeReset applies EventRequiresProjectScopeReset to an event
// about to be released. An event that stays releasable has its subject and
// related issue recorded as exposed, beside the referenced issues the boundary
// check already resolved, so the project access release withholds a response
// whose issue moved into a hidden project after it was read.
func (l *EventIssueLookup) RequiresProjectScopeReset(ctx context.Context, event Event) (bool, error) {
	reset, err := EventRequiresProjectScopeReset(ctx, event, l.IssueByUID, l.IssueByRef)
	if err != nil || reset {
		return reset, err
	}
	return l.recordEventEndpoints(ctx, event)
}

// recordEventEndpoints registers the event's subject and related issue. An
// endpoint named by UID alone, as relayed events are, must resolve inside the
// caller's boundary: the event query admitted it, so a miss means the issue
// left the caller's projects since, and the event fails closed into a reset.
// Restricted reads resolve these endpoints even without a registry, so an
// event stream fails closed the same way.
func (l *EventIssueLookup) recordEventEndpoints(ctx context.Context, event Event) (bool, error) {
	_, restricted := AuthorizedProjects(ctx)
	if !restricted && issueIDSetFrom(ctx, exposedIssuesKey{}) == nil {
		return false, nil
	}
	endpoints := []struct {
		id  *int64
		uid *string
	}{{event.IssueID, event.IssueUID}, {event.RelatedIssueID, event.RelatedIssueUID}}
	for _, endpoint := range endpoints {
		if endpoint.id != nil {
			RecordExposedIssue(ctx, *endpoint.id)
			continue
		}
		if endpoint.uid == nil || *endpoint.uid == "" {
			continue
		}
		issue, err := l.IssueByUID(*endpoint.uid)
		if errors.Is(err, ErrNotFound) {
			if restricted {
				return true, nil
			}
			continue
		}
		if err != nil {
			return false, fmt.Errorf("resolving event %d issue %s: %w", event.ID, *endpoint.uid, err)
		}
		RecordExposedIssue(ctx, issue.ID)
	}
	return false, nil
}
