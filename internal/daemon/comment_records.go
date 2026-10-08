package daemon

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
)

func commentReferenceError(err error) error {
	if errors.Is(err, commentref.ErrNotFound) {
		return api.NewError(404, "comment_not_found", "comment not found", "", nil)
	}
	if errors.Is(err, commentref.ErrAmbiguous) {
		return api.NewError(409, "ambiguous_comment", err.Error(), "use a longer suffix or the full comment UID", nil)
	}
	return api.NewError(400, "validation", err.Error(), "", nil)
}
func graphRecord(r db.CommentGraphRecord) commentref.Record {
	return commentref.Record{Comment: r.Comment, IssueUID: r.IssueUID, IssueShortID: r.IssueShortID, ProjectUID: r.ProjectUID, ProjectID: r.ProjectID, ProjectName: r.ProjectName}
}

// Endpoint authorization uses an isolated host decision so an invisible moved
// target cannot contaminate authorization of the source or another target.
func authorizeCommentEndpoint(ctx context.Context, projectID int64) error {
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		isolated := *state
		isolated.request.Operation.ProjectIDs = slices.Clone(state.request.Operation.ProjectIDs)
		isolated.request.Operation.ProjectUIDs = slices.Clone(state.request.Operation.ProjectUIDs)
		ctx = context.WithValue(ctx, hostAccessStateContextKey{}, &isolated)
	}
	_, err := authorizeHostProjectScope(ctx, []int64{projectID}, nil, false)
	return err
}
func projectCommentGraph(ctx context.Context, data db.CommentGraphData) ([]commentref.Record, error) {
	records := make([]commentref.Record, 0, len(data.Comments))
	for _, r := range data.Comments {
		records = append(records, graphRecord(r))
	}
	states := map[string]commentref.TargetState{}
	for id, state := range data.Targets {
		projected := commentref.TargetState{Status: state.Status}
		if state.Record != nil {
			if err := authorizeCommentEndpoint(ctx, state.Record.ProjectID); err != nil {
				var apiErr *api.APIError
				if errors.As(err, &apiErr) && apiErr.Status == 404 {
					projected.Status = "hidden"
				} else {
					return nil, err
				}
			} else {
				r := graphRecord(*state.Record)
				if suffix := commentref.Handles(data.CommentUIDsByProject[r.ProjectID])[r.UID]; suffix != "" {
					r.Handle = "c:" + suffix
				}
				projected.Target = &r
			}
		}
		states[id] = projected
	}
	return commentref.Project(records, states), nil
}
func readCommentRecords(ctx context.Context, store db.Storage, projectID int64, extra ...db.CommentGraphRecord) ([]commentref.Record, error) {
	data, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: projectID, IssueScope: issueScopeFromContext(ctx)})
	if err != nil {
		return nil, internalAPIError(err)
	}
	data.Comments = append(data.Comments, extra...)
	return projectCommentGraph(ctx, data)
}

// Graph reads authorize inside their read transaction. Retain the issues that
// appear in the selected response so the response fence also catches a move
// after that transaction, including endpoints exposed only through backlinks.
func recordCommentResponseScope(ctx context.Context, records, selected []commentref.Record) {
	issueByComment := make(map[string]int64, len(records))
	for _, record := range records {
		issueByComment[record.UID] = record.IssueID
	}
	for _, record := range selected {
		db.RecordIssueScopeTarget(ctx, record.IssueID)
		if record.Reply != nil {
			if issueID, ok := issueByComment[record.Reply.UID]; ok {
				db.RecordIssueScopeTarget(ctx, issueID)
			}
		}
		for _, link := range record.Backlinks {
			if issueID, ok := issueByComment[link.UID]; ok {
				db.RecordIssueScopeTarget(ctx, issueID)
			}
		}
		if record.BacklinksTruncated {
			// Omitted contributors still determine the partial-evidence flag.
			for _, contributor := range records {
				if contributor.ReplyToUID == record.UID {
					db.RecordIssueScopeTarget(ctx, contributor.IssueID)
				}
			}
		}
	}
}
