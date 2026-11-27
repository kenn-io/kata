package db

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"slices"
	"time"

	"go.kenn.io/kata/internal/metadata"
)

// UIStore is the narrow coherent-read contract used by the browser API.
type UIStore interface {
	UIEventCursor(context.Context) (int64, error)
	ReadUISnapshot(context.Context, UISnapshotQuery) (UISnapshotData, error)
	ReadUIReferenceHydration(context.Context, UIReferencesQuery) (UIReferenceHydration, error)
	ReadUIReferences(context.Context, UIReferencesQuery) (UIReferencesData, error)
}

// UISnapshotQuery is normalized browser route, filtering, and detail intent.
type UISnapshotQuery struct {
	View             string
	ProjectUID       string
	Status           string
	Statuses         []string
	Owner            string
	Owners           []string
	Label            string
	Labels           []string
	Relationships    []string
	Text             string
	SelectedIssueUID string
	IncludeGraph     bool
	IncludeHistory   bool
	LocalDate        string
	TimeZone         string
	// ReadyAt is the daemon-selected time used by the synthetic ready status.
	// It is part of the snapshot cache identity so timed readiness can change
	// without a database event.
	ReadyAt         string
	DefaultTimezone string
	Limit           int
	// ClosedLimit, when positive, returns every matching open issue but only
	// the ClosedLimit most recently closed matching issues, and reports the
	// number of matching closed issues in UISnapshotData.ClosedTotal. Views
	// that filter rows after the query (today, upcoming, deadlines, delegated)
	// do not support it.
	ClosedLimit int
	// AllowedIssueIDs is nil for unrestricted callers. A non-nil slice is
	// applied before ordering and limiting every issue collection.
	AllowedIssueIDs []int64
	IssueScope      *APITokenScope
	// ReuseAuthorityCursor asks the store to omit catalog and collection rows
	// only when its consistent read observes this exact durable cursor.
	ReuseAuthorityCursor *int64
}

// UIProject combines active project identity with its collection statistics.
type UIProject struct {
	Project Project      `json:"project"`
	Stats   ProjectStats `json:"stats"`
}

// UIIssue is an issue plus the display identity and labels needed by the SPA.
type UIIssue struct {
	Issue
	ProjectName     string   `json:"project_name"`
	QualifiedID     string   `json:"qualified_id"`
	Labels          []string `json:"labels"`
	ScheduledOnDate string   `json:"scheduled_on_date,omitempty"`
	DeadlineOnDate  string   `json:"deadline_on_date,omitempty"`
	// CommentCount is filled for snapshot collection issues only.
	CommentCount int64 `json:"comment_count,omitempty"`
}

// MatchUICalendarView evaluates date-sensitive browser collection predicates
// after planning dates have been projected into the browser timezone.
// scheduledDefaultTimezone can include the legacy recurrence fallback;
// deadlines always use the daemon default from query.
func MatchUICalendarView(
	raw string,
	query UISnapshotQuery,
	scheduledDefaultTimezone string,
) (string, string, bool, error) {
	dates := resolvePlanningCalendarDates(raw, &query.TimeZone, scheduledDefaultTimezone, query.DefaultTimezone)
	if err := dates.err(); err != nil {
		return "", "", false, err
	}
	scheduledDate, scheduled := dates.scheduledOn, dates.hasScheduledOn
	deadlineDate, deadline := dates.deadlineOn, dates.hasDeadlineOn
	switch query.View {
	case "upcoming":
		return scheduledDate, deadlineDate, scheduled && scheduledDate > query.LocalDate, nil
	case "today":
		scheduleDue := scheduled && scheduledDate <= query.LocalDate
		deadlineDue := deadline && deadlineDate <= query.LocalDate
		return scheduledDate, deadlineDate, scheduleDue || deadlineDue, nil
	case "deadlines":
		return "", deadlineDate, true, nil
	default:
		return scheduledDate, deadlineDate, true, nil
	}
}

// ProjectUIPlanningDates gives a selected issue the same dates as collection rows.
func ProjectUIPlanningDates(issue *UIIssue, query UISnapshotQuery) error {
	dates := resolvePlanningCalendarDates(string(issue.Metadata), &query.TimeZone, query.DefaultTimezone, query.DefaultTimezone)
	// A legacy schedule may be unreadable; it must not hide the selected issue.
	if dates.deadlineErr != nil {
		return dates.deadlineErr
	}
	issue.ScheduledOnDate = selectedZonedScheduleDate(issue.Metadata, dates.scheduledOn)
	issue.DeadlineOnDate = dates.deadlineOn
	return nil
}

func selectedZonedScheduleDate(raw JSONBlob, date string) string {
	var value struct {
		ScheduledOn string `json:"scheduled_on"`
	}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return ""
	}
	if _, zone := metadata.SplitScheduleZone(value.ScheduledOn); zone != "" {
		return date
	}
	return ""
}

// UILink enriches a stored link with stable display references for each end.
type UILink struct {
	Link
	FromQualifiedID string `json:"from_qualified_id"`
	FromStatus      string `json:"from_status" doc:"Current status of the from_issue_uid issue."`
	ToQualifiedID   string `json:"to_qualified_id"`
	ToStatus        string `json:"to_status" doc:"Current status of the to_issue_uid issue."`
}

// UIGraphEdge carries a canonical directed edge when one endpoint is not
// available as a UI issue. Normal materialized links remain in GraphLinks.
type UIGraphEdge struct {
	FromUID string `json:"from_uid"`
	ToUID   string `json:"to_uid"`
	Kind    string `json:"kind"`
	Layout  bool   `json:"layout"`
}

// UIGraphUnresolvedRef describes the unavailable endpoint of a graph edge.
type UIGraphUnresolvedRef struct {
	UID      string `json:"uid"`
	Side     string `json:"side"`
	Kind     string `json:"kind"`
	OtherUID string `json:"other_uid"`
}

// UISnapshotData is captured wholly inside one backend read transaction.
type UISnapshotData struct {
	CommentGraph CommentGraphData `json:"-"`
	Cursor       int64            `json:"cursor"`
	Projects     []UIProject      `json:"projects"`
	Issues       []UIIssue        `json:"issues"`
	// ClosedTotal counts matching closed issues when ClosedLimit is set.
	ClosedTotal         *int                   `json:"closed_total,omitempty"`
	CollectionLinks     []UILink               `json:"collection_links"`
	SelectedIssue       *UIIssue               `json:"selected_issue,omitempty"`
	SelectedState       string                 `json:"selected_state,omitempty"`
	Comments            []Comment              `json:"comments"`
	SelectedLabels      []IssueLabel           `json:"selected_labels"`
	SelectedLinks       []UILink               `json:"selected_links"`
	Recurrences         []Recurrence           `json:"recurrences"`
	History             []Event                `json:"history"`
	GraphIssues         []UIIssue              `json:"graph_issues"`
	GraphLinks          []UILink               `json:"graph_links"`
	GraphEdges          []UIGraphEdge          `json:"graph_edges"`
	GraphUnresolvedRefs []UIGraphUnresolvedRef `json:"graph_unresolved_refs"`
	// AuthorityReused reports that catalog and collection rows were omitted
	// because ReuseAuthorityCursor matched inside the consistent read.
	AuthorityReused bool `json:"-"`
}

// UIReferencesQuery bounds and filters browser reference choices.
type UIReferencesQuery struct {
	Query      string
	ProjectUID string
	IssueUIDs  []string
	Limit      int
	// AllowedIssueIDs has the same nil-versus-empty contract as UISnapshotQuery.
	AllowedIssueIDs []int64
	IssueScope      *APITokenScope
}

// UIReferenceHydration captures bounded issue summaries and the complete
// active issue/project scope requested for hydration in one read transaction.
type UIReferenceHydration struct {
	References   UIReferencesData
	ResolvedUIDs []string
	ProjectIDs   []int64
}

// UIIssueReference is the bounded typeahead identity for one active issue.
type UIIssueReference struct {
	UID         string `json:"uid"`
	ProjectUID  string `json:"project_uid"`
	ProjectName string `json:"project_name"`
	ShortID     string `json:"short_id"`
	QualifiedID string `json:"qualified_id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
}

// UIReferencesData contains bounded choices captured with one event cursor.
type UIReferencesData struct {
	Cursor   int64              `json:"cursor"`
	Projects []Project          `json:"projects"`
	Issues   []UIIssueReference `json:"issues"`
	Owners   []string           `json:"owners"`
	Labels   []string           `json:"labels"`
}

// RecordExposedUILinks registers both endpoints of each link a browser read
// returns. A link carries its far endpoint's UID, qualified ID and status even
// when that issue is outside the read's collection, so the project access
// release must recheck it like any issue the read scanned.
func RecordExposedUILinks(ctx context.Context, links []UILink) {
	for _, link := range links {
		RecordExposedIssue(ctx, link.FromIssueID)
		RecordExposedIssue(ctx, link.ToIssueID)
	}
}

// PageClosedUIIssues keeps every open issue in order and the limit most
// recently closed issues, newest close first, and returns how many closed
// issues there were. Stores that filter rows in Go after the query use it so
// a closed limit pages the same way as the SQL path.
func PageClosedUIIssues(issues []UIIssue, limit int) ([]UIIssue, int) {
	paged := make([]UIIssue, 0, len(issues))
	var closed []UIIssue
	for _, issue := range issues {
		if issue.Status == "closed" {
			closed = append(closed, issue)
			continue
		}
		paged = append(paged, issue)
	}
	slices.SortStableFunc(closed, func(a, b UIIssue) int {
		at, bt := closedTime(a), closedTime(b)
		if c := bt.Compare(at); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	total := len(closed)
	if len(closed) > limit {
		closed = closed[:limit]
	}
	return append(paged, closed...), total
}

func closedTime(issue UIIssue) time.Time {
	if issue.ClosedAt == nil {
		return time.Time{}
	}
	return *issue.ClosedAt
}
