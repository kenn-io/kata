package mcpserver

import "go.kenn.io/kata/pkg/client/generated"

// issueFromShow narrows the show-issue record to the plain issue shape the
// summary helpers take. The generated client flattens ShowIssueOut instead of
// embedding Issue, so the fields are copied explicitly; Labels is dropped
// because show callers read the top-level labels array.
func issueFromShow(shown generated.ShowIssueOut) generated.Issue {
	return generated.Issue{
		AssignmentExpiresOn: shown.AssignmentExpiresOn,
		Author:              shown.Author,
		Body:                shown.Body,
		ClosedAt:            shown.ClosedAt,
		ClosedReason:        shown.ClosedReason,
		CreatedAt:           shown.CreatedAt,
		DeletedAt:           shown.DeletedAt,
		ID:                  shown.ID,
		Metadata:            shown.Metadata,
		OccurrenceKey:       shown.OccurrenceKey,
		Owner:               shown.Owner,
		Priority:            shown.Priority,
		ProjectID:           shown.ProjectID,
		ProjectUID:          shown.ProjectUID,
		RecurrenceID:        shown.RecurrenceID,
		Revision:            shown.Revision,
		ShortID:             shown.ShortID,
		Status:              shown.Status,
		Title:               shown.Title,
		UID:                 shown.UID,
		UpdatedAt:           shown.UpdatedAt,
	}
}
