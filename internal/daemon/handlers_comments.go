package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

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
		fingerprint := ""
		if in.IdempotencyKey != "" {
			release, err := cfg.DB.AcquireIdempotencyLock(ctx, in.ProjectID, in.IdempotencyKey)
			if err != nil {
				return nil, internalAPIError(err)
			}
			defer func() { _ = release() }()

			fingerprint = commentIdempotencyFingerprint(issue.UID, actor, in.Body.Body)
			match, err := cfg.DB.LookupCommentIdempotency(
				ctx, in.ProjectID, in.IdempotencyKey, time.Now().Add(-idempotencyWindow))
			if err != nil {
				return nil, internalAPIError(err)
			}
			if match != nil {
				if match.Fingerprint != fingerprint {
					return nil, api.NewError(409, "idempotency_mismatch",
						"idempotency key matched a prior comment with a different fingerprint",
						"use a fresh key or send the exact original comment", nil)
				}
				updated, err := cfg.DB.IssueByID(ctx, issue.ID)
				if err != nil {
					return nil, internalAPIError(err)
				}
				out := &api.CommentResponse{}
				out.Body.Issue = updated
				out.Body.Comment = match.Comment
				out.Body.Event = nil
				out.Body.Changed = false
				return out, nil
			}
		}
		c, evt, err := cfg.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID:                issue.ID,
			Author:                 actor,
			Body:                   in.Body.Body,
			IdempotencyKey:         in.IdempotencyKey,
			IdempotencyFingerprint: fingerprint,
		})
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		cfg.Publish().Event(in.ProjectID, evt)
		updated, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, internalAPIError(err)
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
		if errors.Is(err, db.ErrExternalCommentContentOwned) {
			return nil, api.NewError(409, "external_comment_content_owned",
				"the comment body is owned by an active external root binding",
				"edit the comment at its external source or unbind the external root before editing it locally", nil)
		}
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if changed && evt != nil {
			cfg.Publish().Event(in.ProjectID, *evt)
		}
		updated, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.CommentResponse{}
		out.Body.Issue = updated
		out.Body.Comment = c
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})
}

func commentIdempotencyFingerprint(issueUID, actor, body string) string {
	encoded, _ := json.Marshal(struct {
		IssueUID string `json:"issue_uid"`
		Actor    string `json:"actor"`
		Body     string `json:"body"`
	}{IssueUID: issueUID, Actor: actor, Body: body})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
