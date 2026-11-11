package db

import (
	"context"
	"database/sql"
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
	ReadyAt          string
	CalendarAt       string
	CalendarBoundary *time.Time
	DefaultTimezone  string
	Limit            int
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
	ProjectName        string   `json:"project_name"`
	QualifiedID        string   `json:"qualified_id"`
	Labels             []string `json:"labels"`
	ScheduledOnDate    string   `json:"scheduled_on_date,omitempty"`
	DeadlineOnDate     string   `json:"deadline_on_date,omitempty"`
	ScheduledOnDisplay string   `json:"scheduled_on_display,omitempty"`
	DeadlineOnDisplay  string   `json:"deadline_on_display,omitempty"`
	ScheduledOnToday   string   `json:"scheduled_on_today,omitempty"`
	DeadlineOnToday    string   `json:"deadline_on_today,omitempty"`
	ScheduleError      string   `json:"schedule_error,omitempty" doc:"Why this issue's saved scheduled_on or deadline_on cannot be read, when a newer build wrote rules or members this build does not know; the issue stays listed without that date."`
}

// MatchUICalendarView evaluates date-sensitive browser collection predicates
// after planning dates have been projected into their saved zones, or the
// browser timezone for legacy strings.
// scheduledDefaultTimezone can include the legacy recurrence fallback;
// deadlines always use the daemon default from query.
func MatchUICalendarView(
	raw string,
	query UISnapshotQuery,
	scheduledDefaultTimezone string,
) (string, string, string, string, bool, error) {
	now, _ := time.Parse(time.RFC3339Nano, query.CalendarAt)
	dates := resolvePlanningCalendarDates(raw, &query.TimeZone, scheduledDefaultTimezone, query.DefaultTimezone, now, query.CalendarBoundary)
	if err := dates.unsupported(); err != nil {
		return "", "", "", "", false, err
	}
	scheduledDate, scheduled := dates.scheduledOn, dates.hasScheduledOn
	deadlineDate, deadline := dates.deadlineOn, dates.hasDeadlineOn
	scheduledToday, deadlineToday := dates.scheduledToday, dates.deadlineToday
	if scheduledToday == "" {
		scheduledToday = query.LocalDate
	}
	if deadlineToday == "" {
		deadlineToday = query.LocalDate
	}
	switch query.View {
	case "upcoming":
		return scheduledDate, deadlineDate, scheduledToday, deadlineToday, scheduled && scheduledDate > scheduledToday, nil
	case "today":
		scheduleDue := scheduled && scheduledDate <= scheduledToday
		deadlineDue := deadline && deadlineDate <= deadlineToday
		return scheduledDate, deadlineDate, scheduledToday, deadlineToday, scheduleDue || deadlineDue, nil
	case "deadlines":
		return "", deadlineDate, scheduledToday, deadlineToday, true, nil
	default:
		return scheduledDate, deadlineDate, scheduledToday, deadlineToday, true, nil
	}
}

// ApplyUICalendarView sets a collection row's planning dates and reports whether
// the row belongs in the view. A saved schedule this build cannot read keeps the
// row listed, with the reason on ScheduleError, so it never hides other rows.
func ApplyUICalendarView(issue *UIIssue, query UISnapshotQuery, scheduledDefaultTimezone string) (bool, error) {
	scheduled, deadline, scheduledToday, deadlineToday, matches, err := MatchUICalendarView(string(issue.Metadata), query, scheduledDefaultTimezone)
	if metadata.IsUnsupportedSchedule(err) {
		issue.ScheduleError = err.Error()
		return true, nil
	}
	issue.ScheduledOnDate, issue.DeadlineOnDate = scheduled, deadline
	issue.ScheduledOnToday, issue.DeadlineOnToday = scheduledToday, deadlineToday
	return matches, err
}

// ProjectUIPlanningDates gives a selected issue the same dates as collection rows.
func ProjectUIPlanningDates(ctx context.Context, tx *sql.Tx, issue *UIIssue, query UISnapshotQuery) error {
	scheduledDefault := query.DefaultTimezone
	if issue.RecurrenceID != nil {
		zone, err := IssueRecurrenceTimezone(ctx, tx, issue.Issue)
		if err != nil {
			return err
		}
		if zone != "" {
			scheduledDefault = zone
		}
	}
	now, _ := time.Parse(time.RFC3339Nano, query.CalendarAt)
	dates := resolvePlanningCalendarDates(string(issue.Metadata), &query.TimeZone, scheduledDefault, query.DefaultTimezone, now, query.CalendarBoundary)
	if err := dates.unsupported(); err != nil {
		issue.ScheduleError = err.Error()
	}
	issue.ScheduledOnDate = dates.scheduledOn
	issue.DeadlineOnDate = dates.deadlineOn
	issue.ScheduledOnToday, issue.DeadlineOnToday = dates.scheduledToday, dates.deadlineToday
	issue.ScheduledOnDisplay, issue.DeadlineOnDisplay = dates.scheduledDisplay, dates.deadlineDisplay
	return nil
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
	CalendarBoundary    time.Time              `json:"-"`
	CommentGraph        CommentGraphData       `json:"-"`
	Cursor              int64                  `json:"cursor"`
	Projects            []UIProject            `json:"projects"`
	Issues              []UIIssue              `json:"issues"`
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
