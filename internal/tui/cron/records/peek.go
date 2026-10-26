package records

import "time"

// IssuePeek is the selected issue's current readable context. ProjectID is
// the issue's current project, which differs from the run's after a move.
type IssuePeek struct {
	IssueUID, ShortID, Title string
	ProjectID                int64
	Latest                   *PeekComment
}

// PeekComment retains the latest comment's native identity and timestamp.
type PeekComment struct {
	UID, Author, Body string
	CreatedAt         time.Time
}
