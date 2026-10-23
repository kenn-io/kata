package mcpevents

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"
)

// Project identifies one project visible to a principal.
type Project struct {
	UID  string
	ID   int64
	Name string
}

// Occurrence is one durable event row after authorization. Payload is the
// projection the principal may see: scoped credentials receive redacted
// payloads.
type Occurrence struct {
	EventID    int64 // global events.id
	EventUID   string
	Type       string
	ProjectUID string
	IssueUID   string
	Payload    []byte
	CreatedAt  time.Time
}

// Page is one authorized read. Occurrences are the visible rows in the
// requested projects; Next is the highest event id the read inspected,
// including rows that were hidden or outside the selection. Reset reports a
// gap: a purge or scope reset above the cursor. ResetHead is the head to
// resume from.
type Page struct {
	Occurrences []Occurrence
	Next        int64
	Reset       bool
	ResetHead   int64
}

// Authority reconstructs the full authorization context of a persisted
// principal. It is implemented by the daemon, which owns the decision
// functions, and injected. It never allows on a missing context.
type Authority interface {
	// Reacquire returns ErrPrincipalRevoked when the principal no longer holds
	// authority, and ErrAuthorityUnavailable when no decision can be made now.
	Reacquire(ctx context.Context, p Principal) (Authorized, error)
}

// Authorized is one principal's current read authority. Every archive read
// the Events service makes goes through it.
type Authorized interface {
	// VisibleProjects returns the visible projects, intersected with selection
	// when it is non-nil, sorted by UID.
	VisibleProjects(ctx context.Context, selection []string) ([]Project, error)
	// Head is the global head: the last event id or the highest reserved
	// purge-reset position, whichever is greater.
	Head(ctx context.Context, projects []Project) (int64, error)
	// Read returns visible rows with id > after in the projects, inspecting at
	// most limit rows.
	Read(ctx context.Context, projects []Project, after int64, limit int) (Page, error)
	// AuthorizeOccurrence rechecks that the occurrence's project and issue are
	// still visible, returning ErrOccurrenceHidden when they are not.
	AuthorizeOccurrence(ctx context.Context, o Occurrence) error
}

// Match is an occurrence translated to the subscription's recipient.
type Match struct {
	EventKey       string // the webhook eventId
	SourceEventID  int64  // global events.id; the cursor position
	SourceEventUID string
	ProjectUID     string
	IssueUID       string
	Reason         string
	Timestamp      time.Time
}

// ScanResult is one translated read. Rows that did not match settle without
// delivery: when Matches is empty the cursor may move to Next, otherwise to
// each match's SourceEventID in turn.
type ScanResult struct {
	Matches   []Match
	Next      int64
	Reset     bool
	ResetHead int64
	// Scanned counts the selected rows the read inspected, matching or not.
	Scanned int
}

// ResolveProjects returns the visible projects for a subscription. An
// explicit selection with none visible returns ErrScopeRemoved; a dynamic
// selection keeps an empty catalog, which is not a loss of authority.
func ResolveProjects(ctx context.Context, a Authorized, args Arguments) ([]Project, error) {
	projects, err := a.VisibleProjects(ctx, args.ProjectUIDs)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 && !args.Dynamic() {
		return nil, ErrScopeRemoved
	}
	return projects, nil
}

// Scan reads one authorized page after the cursor and translates it for the
// subscription's recipient. An empty project set reads nothing.
func Scan(ctx context.Context, a Authorized, daemonUID string, args Arguments, projects []Project, after int64, limit int) (ScanResult, error) {
	if len(projects) == 0 {
		return ScanResult{Next: after}, nil
	}
	page, err := a.Read(ctx, projects, after, limit)
	if err != nil {
		return ScanResult{}, err
	}
	if page.Reset {
		return ScanResult{Next: page.Next, Reset: true, ResetHead: page.ResetHead}, nil
	}
	if page.Next < after {
		return ScanResult{}, fmt.Errorf("event read moved backwards: %w", ErrUnavailable)
	}
	selected := make(map[string]bool, len(projects))
	for _, p := range projects {
		selected[p.UID] = true
	}
	result := ScanResult{Next: page.Next, Matches: []Match{}, Scanned: len(page.Occurrences)}
	for _, o := range page.Occurrences {
		if o.EventID <= after {
			return ScanResult{}, fmt.Errorf("event read repeated a row: %w", ErrUnavailable)
		}
		visibleProject, hideIssue := o.ProjectUID, false
		if !selected[o.ProjectUID] {
			// A move is recorded in the destination project. A subscriber of the
			// departure project learns of it without the destination or issue.
			var move struct {
				From string `json:"from_project_uid"`
				To   string `json:"to_project_uid"`
			}
			if o.Type != "issue.moved" || json.Unmarshal(o.Payload, &move) != nil || !selected[move.From] || move.To != o.ProjectUID {
				continue
			}
			visibleProject, hideIssue = move.From, true
		}
		reason := inboxEventReason(o, args)
		if reason == "" {
			continue
		}
		issueUID := o.IssueUID
		if hideIssue {
			issueUID = ""
		}
		result.Matches = append(result.Matches, Match{
			EventKey: inboxSourceEventID(daemonUID, o.EventUID, args.For), SourceEventID: o.EventID,
			SourceEventUID: o.EventUID, ProjectUID: visibleProject, IssueUID: issueUID, Reason: reason, Timestamp: o.CreatedAt,
		})
	}
	return result, nil
}

// sortedUnique reports whether uids is strictly ascending.
func sortedUnique(uids []string) bool {
	return slices.IsSorted(uids) && len(slices.Compact(slices.Clone(uids))) == len(uids)
}
