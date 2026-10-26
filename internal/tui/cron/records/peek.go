package records

import "time"

// IssuePeek is the selected issue's current readable context.
type IssuePeek struct {
	IssueUID, ShortID, Title string
	Latest                   *PeekComment
}

// PeekComment retains the latest comment's native identity and timestamp.
type PeekComment struct {
	UID, Author, Body string
	CreatedAt         time.Time
}
