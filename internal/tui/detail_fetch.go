package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

// fetchIssue wraps Client.GetIssueDetail for the Enter-jump path. The 5s
// ceiling matches fetchInitial so the detail view honors the same
// budget as every other read. gen tags the detail-open generation so
// applyFetched can discard the result if the user jumped or popped
// before the request finished. ref is a short_id, qualified short_id,
// or 26-char ULID; the daemon's path resolver handles all three.
func fetchIssue(api detailAPI, projectID int64, ref string, gen int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		detail, err := api.GetIssueDetail(ctx, projectID, ref)
		var issue *Issue
		var parent *IssueRef
		var children []Issue
		if detail != nil {
			issue = detail.Issue
			parent = detail.Parent
			children = detail.Children
		}
		return detailFetchedMsg{gen: gen, issue: issue, parent: parent, children: children, err: err}
	}
}

// fetchComments wraps Client.ListComments for use as a tea.Cmd. The 5s
// ceiling matches fetchInitial so the detail view honors the same
// budget as the list-fetch path. See fetchIssue for the gen rationale.
func fetchComments(api detailAPI, projectID int64, ref string, gen int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		comments, err := api.ListComments(ctx, projectID, ref)
		return commentsFetchedMsg{gen: gen, comments: comments, err: err}
	}
}

// fetchEvents wraps Client.ListEvents for use as a tea.Cmd. ref is the
// issue short_id; the client filters the project-wide poll response by
// the issue_short_id embedded in each event row.
func fetchEvents(api detailAPI, projectID int64, ref string, gen int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		events, err := api.ListEvents(ctx, projectID, ref)
		return eventsFetchedMsg{gen: gen, events: events, err: err}
	}
}

// fetchLinks wraps Client.ListLinks for use as a tea.Cmd.
func fetchLinks(api detailAPI, projectID int64, ref string, gen int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		links, err := api.ListLinks(ctx, projectID, ref)
		return linksFetchedMsg{gen: gen, links: links, err: err}
	}
}
