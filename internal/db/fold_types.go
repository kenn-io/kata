package db

import "encoding/json/jsontext"

// FoldEvent is the portable event shape consumed by the fold engine. It excludes
// backend-local row IDs by design.
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
	Payload           jsontext.Value
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
	CronRuns        map[string]FoldCronRun
	CronJobs        map[string]FoldCronJob
	CronWorkflows   map[string]FoldCronWorkflow
	Issues          map[string]FoldIssue
	Comments        map[string]FoldComment
	Labels          map[FoldLabelKey]FoldElementState
	Links           map[FoldLinkKey]FoldElementState
	IssueMetadata   map[string]jsontext.Value
	ProjectMetadata map[string]jsontext.Value
	Warnings        []string
	cronAuthors     map[string]foldCronAuthorCandidate
}

// FoldCronJob retains project identity alongside the winning job document.
type FoldCronJob struct {
	CronJob
	ProjectUID string
}

// FoldCronWorkflow retains project identity alongside the winning workflow document.
type FoldCronWorkflow struct {
	CronWorkflow
	ProjectUID string
}

// FoldIssue is the replayed issue state keyed by stable issue UID.
type FoldIssue struct {
	UID                 string
	ShortID             string
	Title               string
	Body                string
	Author              string
	Owner               *string
	AssignmentExpiresOn *string
	Priority            *int64
	Status              string
	ClosedReason        *string
	ClosedAt            *string
	DeletedAt           *string
	ProjectUID          string
	CreatedAt           string
	UpdatedAt           string

	// StatusIntentUID is in-memory provenance, not exported snapshot state.
	// Callers may enqueue only newly accepted explicit events or retain their
	// existing pending UID; folding history alone never grants write authority.
	StatusIntentUID string `json:"-"`
	// StatusClock is the latest status-bearing writer, including same-state
	// restatements. Newly accepted intent must outrank the previous projection's
	// writer; an older delayed event cannot manufacture a pending pointer.
	StatusClock FoldClock `json:"-"`
}

// FoldComment is the replayed comment state keyed by stable comment UID.
type FoldComment struct {
	UID       string
	IssueUID  string
	Author    string
	Teammate  string
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
	// CreatedAt is the original link date when supplied by a snapshot.
	// Empty means the event did not record it; labels do not use this field.
	CreatedAt string
}

// FoldLinkEdge pairs one link key with its tombstone state. It is the element
// type of FoldProjection.PresentLinks, which the backends consume when they
// materialize link rows.
type FoldLinkEdge struct {
	Key   FoldLinkKey
	State FoldElementState
}
