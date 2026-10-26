package tui

import (
	"context"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// nextDetailFetchRequestSeq orders detail requests independently of the
// detail-open generation. A generation identifies the issue lifetime, while
// this sequence distinguishes overlapping refetches within that lifetime.
var nextDetailFetchRequestSeq atomic.Uint64

// fetchIssue wraps Client.GetIssueDetail for the Enter-jump path. The 5s
// ceiling matches fetchInitial so the detail view honors the same
// budget as every other read. gen tags the detail-open generation so
// applyFetched can discard the result if the user jumped or popped
// before the request finished. ref is a short_id, qualified short_id,
// or 26-char ULID; the daemon's path resolver handles all three.
func fetchIssue(api detailAPI, projectID int64, ref string, gen int64, parents ...context.Context) tea.Cmd {
	requestSeq := nextDetailFetchRequestSeq.Add(1)
	return func() tea.Msg {
		ctx, cancel := detailReadContext(parents)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return detailFetchedMsg{gen: gen, requestSeq: requestSeq, err: err}
		}
		detail, err := api.GetIssueDetail(ctx, projectID, ref)
		var issue *Issue
		var parent *IssueRef
		var children []Issue
		if detail != nil {
			issue = detail.Issue
			parent = detail.Parent
			children = detail.Children
		}
		return detailFetchedMsg{
			gen: gen, requestSeq: requestSeq,
			issue: issue, parent: parent, children: children, err: err,
		}
	}
}

// fetchComments wraps Client.ListComments for use as a tea.Cmd. The 5s
// ceiling matches fetchInitial so the detail view honors the same
// budget as the list-fetch path. See fetchIssue for the gen rationale.
func fetchComments(api detailAPI, projectID int64, ref string, gen int64, parents ...context.Context) tea.Cmd {
	requestSeq := nextDetailFetchRequestSeq.Add(1)
	return func() tea.Msg {
		ctx, cancel := detailReadContext(parents)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return commentsFetchedMsg{gen: gen, requestSeq: requestSeq, err: err}
		}
		comments, err := api.ListComments(ctx, projectID, ref)
		return commentsFetchedMsg{gen: gen, requestSeq: requestSeq, comments: comments, err: err}
	}
}

// fetchEvents wraps Client.ListEvents for use as a tea.Cmd. ref is the
// issue reference; the daemon filters history before pagination.
func fetchEvents(api detailAPI, projectID int64, ref string, gen int64, parents ...context.Context) tea.Cmd {
	requestSeq := nextDetailFetchRequestSeq.Add(1)
	return func() tea.Msg {
		ctx, cancel := detailReadContext(parents)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return eventsFetchedMsg{gen: gen, requestSeq: requestSeq, err: err}
		}
		events, err := api.ListEvents(ctx, projectID, ref)
		return eventsFetchedMsg{gen: gen, requestSeq: requestSeq, events: events, err: err}
	}
}

// fetchLinks wraps Client.ListLinks for use as a tea.Cmd.
func fetchLinks(api detailAPI, projectID int64, ref string, gen int64, parents ...context.Context) tea.Cmd {
	requestSeq := nextDetailFetchRequestSeq.Add(1)
	return func() tea.Msg {
		ctx, cancel := detailReadContext(parents)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return linksFetchedMsg{gen: gen, requestSeq: requestSeq, err: err}
		}
		links, err := api.ListLinks(ctx, projectID, ref)
		return linksFetchedMsg{gen: gen, requestSeq: requestSeq, links: links, err: err}
	}
}

func detailReadContext(parents []context.Context) (context.Context, context.CancelFunc) {
	parent := context.Background()
	if len(parents) > 0 {
		parent = parents[0]
	}
	return context.WithTimeout(parent, 5*time.Second)
}
