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
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/uid"
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
		var issue db.Issue
		resolved := false
		fingerprint := ""
		if in.IdempotencyKey != "" {
			routeProject, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
			if err != nil {
				return nil, err
			}
			// Comment keys are scoped to one issue UID. A full ULID ref needs
			// no resolution, so its retry survives a project move; any other
			// ref form resolves inside the route project first and falls back
			// to the receipt this project already holds for the key.
			issueUID := ""
			if uid.Valid(in.Ref) {
				issueUID = strings.ToUpper(in.Ref)
			} else {
				issue, err = activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
				if err == nil {
					resolved = true
					issueUID = issue.UID
				} else if issueUID, err = receiptIssueUID(
					ctx, cfg, routeProject, in.Ref, in.IdempotencyKey, err,
				); err != nil {
					return nil, err
				}
			}
			// Project IDs change when an issue moves. Zero is outside the
			// persisted project ID range and gives every keyed comment one
			// stable, backend-wide lock scope.
			release, err := cfg.DB.AcquireIdempotencyLock(ctx, 0, issueUID+"\x00"+in.IdempotencyKey)
			if err != nil {
				return nil, internalAPIError(err)
			}
			defer func() { _ = release() }()

			match, err := cfg.DB.LookupCommentIdempotency(
				ctx, issueUID, in.IdempotencyKey, time.Now().Add(-idempotencyWindow))
			if err != nil {
				return nil, internalAPIError(err)
			}
			if match != nil {
				return replayComment(ctx, cfg, in.ProjectID, match, actor, in.Body.Body)
			}
		}
		if !resolved {
			issue, err = activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
		}
		if in.IdempotencyKey != "" {
			fingerprint = commentIdempotencyFingerprint(issue.UID, actor, in.Body.Body)
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

// replayComment returns a committed comment receipt to an exact retry. The
// route must be the project the comment was written in or the project the
// issue lives in now, and that current project must still be active and
// inside the caller's host scope, because the reply exposes current state.
func replayComment(
	ctx context.Context,
	cfg ServerConfig,
	routeProjectID int64,
	match *db.CommentIdempotencyMatch,
	actor, body string,
) (*api.CommentResponse, error) {
	if match.Fingerprint != commentIdempotencyFingerprint(match.IssueUID, actor, body) {
		return nil, api.NewError(409, "idempotency_mismatch",
			"idempotency key matched a prior comment with a different fingerprint",
			"use a fresh key or send the exact original comment", nil)
	}
	current, err := cfg.DB.IssueByID(ctx, match.Comment.IssueID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	if current.DeletedAt != nil ||
		(routeProjectID != match.Event.ProjectID && routeProjectID != current.ProjectID) {
		return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
	}
	if _, err := activeProjectByID(ctx, cfg.DB, current.ProjectID); err != nil {
		return nil, err
	}
	if _, err := authorizeHostProjectScope(ctx, []int64{current.ProjectID}, nil, false); err != nil {
		return nil, err
	}
	out := &api.CommentResponse{}
	out.Body.Issue = current
	out.Body.Comment = match.Comment
	out.Body.Event = nil
	out.Body.Changed = false
	return out, nil
}

// receiptIssueUID recovers the issue a short-id retry addresses after that
// issue moved out of the route project. The receipt written in this project
// names the issue; the ref must still be a suffix of that issue's ULID, and a
// qualifier must name this project, so a key cannot steer a retry elsewhere.
// Any other outcome returns the original resolution error.
func receiptIssueUID(
	ctx context.Context,
	cfg ServerConfig,
	routeProject db.Project,
	ref, key string,
	resolveErr error,
) (string, error) {
	parsed, err := shortid.Parse(ref)
	if err != nil || parsed.ShortID == "" ||
		(parsed.Project != "" && parsed.Project != routeProject.Name) {
		return "", resolveErr
	}
	match, err := cfg.DB.LookupIssueMutationIdempotency(
		ctx, routeProject.ID, "issue.commented", key, time.Now().Add(-idempotencyWindow))
	if err != nil {
		return "", internalAPIError(err)
	}
	if match == nil {
		return "", resolveErr
	}
	derived, err := shortid.Derive(match.IssueUID, len(parsed.ShortID))
	if err != nil || derived != parsed.ShortID {
		return "", resolveErr
	}
	return match.IssueUID, nil
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
