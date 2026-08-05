package db

import "context"

// UIStore is the narrow coherent-read contract used by the browser API.
type UIStore interface {
	UIEventCursor(context.Context) (int64, error)
	ReadUISnapshot(context.Context, UISnapshotQuery) (UISnapshotData, error)
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
	Limit            int
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
	ProjectName string   `json:"project_name"`
	QualifiedID string   `json:"qualified_id"`
	Labels      []string `json:"labels"`
}

// UILink enriches a stored link with stable display references for each end.
type UILink struct {
	Link
	FromQualifiedID string `json:"from_qualified_id"`
	FromStatus      string `json:"from_status"`
	ToQualifiedID   string `json:"to_qualified_id"`
	ToStatus        string `json:"to_status"`
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
	Limit      int
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
