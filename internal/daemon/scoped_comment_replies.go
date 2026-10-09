package daemon

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// projectScopedCommentReplies clears reply endpoints that are unresolved or
// outside the caller's current issue subtree.
func projectScopedCommentReplies(
	ctx context.Context,
	store db.Storage,
	comments []db.Comment,
) ([]db.Comment, map[int64]struct{}, bool, error) {
	projected := make([]db.Comment, len(comments))
	copy(projected, comments)
	if issueScopeFromContext(ctx) == nil {
		return projected, nil, false, nil
	}
	targetUIDs := make([]string, 0, len(comments))
	seen := make(map[string]struct{}, len(comments))
	for _, comment := range comments {
		if comment.ReplyToUID == "" {
			continue
		}
		if _, exists := seen[comment.ReplyToUID]; exists {
			continue
		}
		seen[comment.ReplyToUID] = struct{}{}
		targetUIDs = append(targetUIDs, comment.ReplyToUID)
	}
	targetIssueIDs := map[string]int64{}
	if len(targetUIDs) > 0 {
		var err error
		targetIssueIDs, err = store.CommentIssueIDsByUIDs(ctx, targetUIDs)
		if err != nil {
			return nil, nil, false, internalAPIError(err)
		}
	}
	allowedIssueIDs, scoped, err := issueScopedAllowedIDSet(ctx, store)
	if err != nil {
		return nil, nil, false, err
	}
	if !scoped || len(projected) == 0 || len(targetUIDs) == 0 {
		return projected, allowedIssueIDs, scoped, nil
	}

	for i := range projected {
		if projected[i].ReplyToUID == "" {
			continue
		}
		issueID, exists := targetIssueIDs[projected[i].ReplyToUID]
		if !exists {
			clearCommentReply(&projected[i])
			continue
		}
		if _, allowed := allowedIssueIDs[issueID]; !allowed {
			clearCommentReply(&projected[i])
			continue
		}
		db.RecordIssueScopeTarget(ctx, issueID)
	}
	return projected, allowedIssueIDs, scoped, nil
}

func clearCommentReply(comment *db.Comment) {
	comment.ReplyToUID = ""
	comment.ReplyKind = ""
}
