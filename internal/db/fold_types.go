package db

import "encoding/json"

// FoldEvent is the portable event shape consumed by the fold engine. It excludes
// local SQLite row IDs by design.
type FoldEvent struct {
	UID               string
	OriginInstanceUID string
	ProjectUID        string
	IssueUID          string
	RelatedIssueUID   string
	Type              string
	Actor             string
	HLCPhysicalMS     int64
	HLCCounter        int64
	CreatedAt         string
	Payload           json.RawMessage
}

// FoldClock is an event's deterministic last-writer timestamp.
type FoldClock struct {
	HLCPhysicalMS     int64
	HLCCounter        int64
	OriginInstanceUID string
	EventUID          string
}

// FoldProjection is the folded state derived from a set of portable events.
type FoldProjection struct {
	Issues          map[string]FoldIssue
	Comments        map[string]FoldComment
	Labels          map[FoldLabelKey]FoldElementState
	Links           map[FoldLinkKey]FoldElementState
	IssueMetadata   map[string]json.RawMessage
	ProjectMetadata map[string]json.RawMessage
	Warnings        []string
}

// FoldIssue is the replayed issue state keyed by stable issue UID.
type FoldIssue struct {
	UID          string
	ShortID      string
	Title        string
	Body         string
	Author       string
	Owner        *string
	Priority     *int64
	Status       string
	ClosedReason *string
	ClosedAt     *string
	DeletedAt    *string
	ProjectUID   string
	CreatedAt    string
	UpdatedAt    string
}

// FoldComment is the replayed comment state keyed by stable comment UID.
type FoldComment struct {
	UID       string
	IssueUID  string
	Author    string
	Body      string
	CreatedAt string
	Clock     FoldClock
}

// FoldLabelKey identifies one issue-label edge.
type FoldLabelKey struct {
	IssueUID string
	Label    string
}

// FoldLinkKey identifies one issue-link edge. Related links are canonicalized by UID.
type FoldLinkKey struct {
	FromUID string
	ToUID   string
	Type    string
}

// FoldElementState stores an add/remove edge's current tombstone state.
type FoldElementState struct {
	Present bool
	Clock   FoldClock
	Author  string
}
