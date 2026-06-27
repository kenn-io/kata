package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerCommentsHandlers installs POST /comments. CreateComment writes the
// comment row and an issue.commented event in one tx; we re-read the issue via
// IssueByID to surface the freshly-bumped updated_at in the response envelope.
func registerCommentsHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "createComment",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/comments",
	}, func(ctx context.Context, in *api.CommentRequest) (*api.CommentResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		c, evt, err := cfg.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: issue.ID,
			Author:  actor,
			Body:    in.Body.Body,
		})
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(evt)
		updated, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.CommentResponse{}
		out.Body.Issue = updated
		out.Body.Comment = c
		out.Body.Event = &evt
		out.Body.Changed = true
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "editComment",
		Method:      "PATCH",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/comments/{comment_ref}",
	}, func(ctx context.Context, in *api.EditCommentRequest) (*api.CommentResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Body.Body) == "" {
			return nil, api.NewError(400, "validation", "comment body is required", "", nil)
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		// Comment creation has always sat outside the federation claim gate;
		// comment edits follow that model so redaction remains a comment-level
		// maintenance action rather than leased issue work.
		c, evt, changed, err := cfg.DB.EditComment(ctx, db.EditCommentParams{
			IssueID:    issue.ID,
			CommentUID: in.CommentRef,
			Actor:      actor,
			Body:       in.Body.Body,
		})
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "comment_not_found", "comment not found", "", nil)
		}
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if changed && evt != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(*evt)
		}
		updated, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.CommentResponse{}
		out.Body.Issue = updated
		out.Body.Comment = c
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})
}
