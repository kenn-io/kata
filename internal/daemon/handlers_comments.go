package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/teammate"
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
	}, withResolvedProject(cfg, func(ctx context.Context, in *api.CommentRequest) (*api.CommentResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		handle, err := teammate.Resolve(in.Body.Teammate, "")
		if err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
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
				return replayComment(ctx, cfg, in.ProjectID, match, actor, in.Body.Body, handle, in.Body.ReplyTo, in.Body.Kind)
			}
		}
		if !resolved {
			issue, err = activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
		}
		replyUID := ""
		if (in.Body.ReplyTo == "") != (in.Body.Kind == "") || (in.Body.Kind != "" && !commentref.ValidKind(in.Body.Kind)) {
			return nil, api.NewError(400, "validation", "reply_to and a valid kind must be supplied together", "", nil)
		}
		if in.Body.ReplyTo != "" {
			records, err := readCommentRecords(ctx, cfg.DB, issue.ProjectID, 0)
			if err != nil {
				return nil, err
			}
			project, err := activeProjectByID(ctx, cfg.DB, issue.ProjectID)
			if err != nil {
				return nil, err
			}
			target, err := commentref.Resolve(records, issue.UID, in.Body.ReplyTo, project.Name)
			if err != nil {
				return nil, commentReferenceError(err)
			}
			targetIssue, err := cfg.DB.IssueByID(ctx, target.IssueID)
			if err != nil {
				return nil, internalAPIError(err)
			}
			if err := authorizeIssueScopedIssue(ctx, cfg.DB, targetIssue); err != nil {
				return nil, err
			}
			replyUID = target.UID
		}
		if in.IdempotencyKey != "" {
			fingerprint = commentIdempotencyFingerprint(issue.UID, actor, in.Body.Body, handle, replyUID, in.Body.Kind)
		}
		c, evt, err := cfg.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID:    issue.ID,
			Author:     actor,
			Teammate:   handle,
			Body:       in.Body.Body,
			ReplyToUID: replyUID, ReplyKind: in.Body.Kind, ValidateReply: true, Force: in.Body.Force,
			IdempotencyKey:         in.IdempotencyKey,
			IdempotencyFingerprint: fingerprint,
		})
		if err != nil {
			if duplicate, ok := errors.AsType[*db.DuplicateCommentReplyError](err); ok {
				data := map[string]any{}
				const duplicateMessage = "a reply of this kind already exists for this author and teammate"
				message := duplicateMessage
				records, readErr := readCommentRecords(ctx, cfg.DB, issue.ProjectID, 0)
				if readErr != nil {
					return nil, readErr
				}
				for _, r := range records {
					if r.UID == duplicate.Comment.UID {
						db.RecordIssueScopeTarget(ctx, r.IssueID)
						h := r.Handle
						if r.IssueUID != issue.UID {
							h = r.IssueShortID + ":" + strings.TrimPrefix(h, "c:")
						}
						data["existing_handle"] = h
						data["existing_uid"] = r.UID
						message = duplicateMessage + " (" + h + ")"
						break
					}
				}
				return nil, api.NewError(409, "duplicate_reply", message, "use --force to create another reply", data)
			}
			if errors.Is(err, db.ErrCommentReplyInvalid) {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
			if errors.Is(err, db.ErrCommentReplyTarget) {
				return nil, api.NewError(404, "comment_not_found", "comment not found", "", nil)
			}
			if errors.Is(err, db.ErrCommentReplyClosed) {
				return nil, api.NewError(409, "issue_closed", "reopen the reply issue before adding a typed reply", "", nil)
			}
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
		projected, err := scopedMutationEvent(ctx, cfg.DB, &evt)
		if err != nil {
			return nil, err
		}
		comment, err := projectCommentMutationResponse(ctx, cfg.DB, c)
		if err != nil {
			return nil, err
		}
		out := &api.CommentResponse{}
		out.Body.Issue = updated
		out.Body.Comment = comment
		out.Body.Event = projected
		out.Body.Changed = true
		return out, nil
	}))

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
		commentUID := in.CommentRef
		if !uid.Valid(commentUID) {
			records, err := readCommentRecords(ctx, cfg.DB, issue.ProjectID, 0)
			if err != nil {
				return nil, err
			}
			project, err := activeProjectByID(ctx, cfg.DB, issue.ProjectID)
			if err != nil {
				return nil, err
			}
			resolved, err := commentref.Resolve(records, issue.UID, commentUID, project.Name)
			if err != nil {
				return nil, commentReferenceError(err)
			}
			if resolved.IssueID != issue.ID {
				return nil, api.NewError(404, "comment_not_found", "comment not found", "", nil)
			}
			commentUID = resolved.UID
		}
		// Comment creation has always sat outside the federation claim gate;
		// comment edits follow that model so redaction remains a comment-level
		// maintenance action rather than leased issue work.
		c, evt, changed, err := cfg.DB.EditComment(ctx, db.EditCommentParams{
			IssueID:    issue.ID,
			CommentUID: strings.ToUpper(commentUID),
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
		evt, err = scopedMutationEvent(ctx, cfg.DB, evt)
		if err != nil {
			return nil, err
		}
		comment, err := projectCommentMutationResponse(ctx, cfg.DB, c)
		if err != nil {
			return nil, err
		}
		out := &api.CommentResponse{}
		out.Body.Issue = updated
		out.Body.Comment = comment
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
	actor, body, teammate string,
	links ...string,
) (*api.CommentResponse, error) {
	current, err := cfg.DB.IssueByID(ctx, match.Comment.IssueID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	if current.DeletedAt != nil ||
		(routeProjectID != match.Event.ProjectID && routeProjectID != current.ProjectID) {
		return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
	}
	currentProject, err := activeProjectByID(ctx, cfg.DB, current.ProjectID)
	if err != nil {
		return nil, err
	}
	if _, err := authorizeHostProjectScope(ctx, []int64{current.ProjectID}, nil, false); err != nil {
		return nil, err
	}
	if err := authorizeIssueScopedIssue(ctx, cfg.DB, current); err != nil {
		return nil, err
	}
	replyUID, kind := "", ""
	if len(links) > 0 {
		input := links[0]
		kind = links[1]
		if input != "" {
			parsed, parseErr := commentref.Parse(input)
			same := parseErr == nil && match.Comment.ReplyToUID != ""
			if same && parsed.UID != "" {
				same = strings.EqualFold(parsed.UID, match.Comment.ReplyToUID)
			} else if same {
				same = strings.HasSuffix(strings.ToLower(match.Comment.ReplyToUID), parsed.Suffix)
				targetIssueUID := match.IssueUID
				if match.Event.RelatedIssueUID != nil {
					targetIssueUID = *match.Event.RelatedIssueUID
				}
				if parsed.IssueRef != "" {
					same = same && strings.HasSuffix(strings.ToLower(targetIssueUID), parsed.IssueRef)
				} else {
					same = same && targetIssueUID == match.IssueUID
				}
				if parsed.Project != "" {
					same = same && (parsed.Project == match.Event.ProjectName || parsed.Project == currentProject.Name)
				}
			}
			if !same {
				return nil, api.NewError(409, "idempotency_mismatch", "idempotency key matched a different reply target", "use a fresh key", nil)
			}
			replyUID = match.Comment.ReplyToUID
		}
	}
	if match.Fingerprint != commentIdempotencyFingerprint(match.IssueUID, actor, body, teammate, replyUID, kind) {
		return nil, api.NewError(409, "idempotency_mismatch",
			"idempotency key matched a prior comment with a different fingerprint",
			"use a fresh key or send the exact original comment", nil)
	}
	comment, err := projectCommentMutationResponse(ctx, cfg.DB, match.Comment)
	if err != nil {
		return nil, err
	}
	out := &api.CommentResponse{}
	out.Body.Issue = current
	out.Body.Comment = comment
	out.Body.Event = nil
	out.Body.Changed = false
	return out, nil
}

func projectCommentMutationResponse(ctx context.Context, store db.Storage, comment db.Comment) (db.Comment, error) {
	projected, _, _, err := projectScopedCommentReplies(ctx, store, []db.Comment{comment})
	if err != nil {
		return db.Comment{}, err
	}
	comment = projected[0]
	if comment.ReplyToUID == "" {
		return comment, nil
	}
	if _, hostControlled := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); !hostControlled {
		return comment, nil
	}
	issueIDs, err := store.CommentIssueIDsByUIDs(ctx, []string{comment.ReplyToUID})
	if err != nil {
		return db.Comment{}, internalAPIError(err)
	}
	issueID, exists := issueIDs[comment.ReplyToUID]
	if !exists {
		// Preserve pending and removed edges when their endpoint has no current
		// issue record, matching the graph projection's unavailable-target path.
		return comment, nil
	}
	targetIssue, err := store.IssueByID(ctx, issueID)
	if errors.Is(err, db.ErrNotFound) {
		return comment, nil
	}
	if err != nil {
		return db.Comment{}, internalAPIError(err)
	}
	project, err := store.ProjectByID(ctx, targetIssue.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		clearCommentReply(&comment)
		return comment, nil
	}
	if err != nil {
		return db.Comment{}, internalAPIError(err)
	}
	if project.DeletedAt != nil {
		clearCommentReply(&comment)
		return comment, nil
	}
	if targetIssue.DeletedAt != nil {
		// Deleted endpoints remain visible as removed evidence without exposing
		// a live project endpoint that still needs host authorization.
		return comment, nil
	}
	if err := authorizeCommentEndpoint(ctx, targetIssue.ProjectID); err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			clearCommentReply(&comment)
			return comment, nil
		}
		return db.Comment{}, err
	}
	return comment, nil
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

func commentIdempotencyFingerprint(issueUID, actor, body, teammate string, links ...string) string {
	replyUID, kind := "", ""
	if len(links) > 0 {
		replyUID, kind = links[0], links[1]
	}
	encoded, _ := json.Marshal(struct {
		IssueUID string `json:"issue_uid"`
		Actor    string `json:"actor"`
		Body     string `json:"body"`
		Teammate string `json:"teammate,omitempty"`
		ReplyTo  string `json:"reply_to_uid,omitempty"`
		Kind     string `json:"reply_kind,omitempty"`
	}{ReplyTo: replyUID, Kind: kind, IssueUID: issueUID, Actor: actor, Body: body, Teammate: teammate})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
