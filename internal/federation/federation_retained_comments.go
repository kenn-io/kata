package federation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"

	"go.kenn.io/kata/internal/db"
)

type retainedCommentRef struct {
	commentUID string
	issueUID   string
}

type retainedDependencyError struct {
	commentUID string
}

func (e *retainedDependencyError) Error() string {
	return fmt.Sprintf("retained discussion comment %s is absent from hub history", e.commentUID)
}

// prepareCommentDependencies takes an event whose move snapshot is already
// unwrapped.
func (r *retainedPush) prepareCommentDependencies(ctx context.Context, event db.Event) error {
	switch event.Type {
	case "issue.commented", "issue.comment_edited":
		key := "reply_to_uid"
		if event.Type == "issue.comment_edited" {
			key = "comment_uid"
		}
		commentUID, _ := db.StringValue(db.PayloadMap([]byte(event.Payload))[key])
		return r.confirmRetainedComment(ctx, commentUID)
	case "issue.created", "issue.snapshot":
		var document db.IssueDocumentPayload
		if err := json.Unmarshal([]byte(event.Payload), &document); err != nil {
			return fmt.Errorf("decode retained issue discussion in %s: %w", event.UID, err)
		}
		comments := issueDocumentCreatedComments(document.Comments)
		for _, comment := range document.Comments {
			if comments[comment.ReplyToUID] {
				continue
			}
			if err := r.confirmRetainedComment(ctx, comment.ReplyToUID); err != nil {
				return err
			}
		}
	}
	return nil
}

// createdComments returns the comments an event creates. A document entry
// without created_at only carries an edit and creates nothing.
func createdComments(eventType string, payload []byte) map[string]bool {
	if eventType == "issue.commented" {
		uid, _ := db.StringValue(db.PayloadMap(payload)["comment_uid"])
		return map[string]bool{uid: uid != ""}
	}
	remote := db.FederationMoveSnapshotEvent(db.RemoteEvent{Type: eventType, Payload: payload})
	if remote.Type != "issue.created" && remote.Type != "issue.snapshot" {
		return nil
	}
	var document db.IssueDocumentPayload
	if json.Unmarshal([]byte(remote.Payload), &document) != nil {
		return nil
	}
	return issueDocumentCreatedComments(document.Comments)
}

func issueDocumentCreatedComments(comments []db.IssueDocumentComment) map[string]bool {
	created := make(map[string]bool, len(comments))
	for _, comment := range comments {
		if comment.CommentUID != "" && comment.CreatedAt != "" {
			created[comment.CommentUID] = true
		}
	}
	return created
}

// confirmRetainedComment checks that the hub has a comment and its reply
// ancestors when rejoin skipped their creation.
func (r *retainedPush) confirmRetainedComment(ctx context.Context, commentUID string) error {
	if commentUID == "" {
		return nil
	}
	refs, err := r.retainedCommentChain(ctx, commentUID)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		comments, err := r.hubComments(ctx, ref.issueUID)
		if err != nil {
			return fmt.Errorf("check retained discussion comment %s in hub history: %w", ref.commentUID, err)
		}
		if comments == nil {
			if err := r.publishRetainedIssue(ctx, ref.issueUID); err != nil {
				return fmt.Errorf("recover task for retained discussion comment %s: %w", ref.commentUID, err)
			}
			if comments, err = r.hubComments(ctx, ref.issueUID); err != nil {
				return fmt.Errorf("confirm retained discussion comment %s after task recovery: %w", ref.commentUID, err)
			}
		}
		if !comments[ref.commentUID] {
			return &retainedDependencyError{commentUID: ref.commentUID}
		}
	}
	return nil
}

func (r *retainedPush) retainedCommentChain(ctx context.Context, commentUID string) ([]retainedCommentRef, error) {
	var retained []retainedCommentRef
	seen := map[string]bool{}
	for uid := commentUID; uid != "" && !seen[uid]; {
		seen[uid] = true
		owners, err := r.store.CommentIssueIDsByUIDs(ctx, []string{uid})
		if err != nil {
			return nil, err
		}
		issueID, found := owners[uid]
		if !found {
			break
		}
		issue, err := r.store.IssueByID(ctx, issueID)
		if err != nil {
			return nil, err
		}
		comments, err := r.store.CommentsByIssue(ctx, issueID)
		if err != nil {
			return nil, err
		}
		var comment *db.Comment
		for i := range comments {
			if comments[i].UID == uid {
				comment = &comments[i]
				break
			}
		}
		if comment == nil {
			break
		}
		wasRetained, err := db.RetainedIssueEvent(ctx, r.store, *r.binding, 0, issue.UID,
			[]string{"issue.commented", "issue.created", "issue.snapshot", "issue.moved"},
			func(event db.Event) bool { return createdComments(event.Type, []byte(event.Payload))[uid] })
		if err != nil {
			return nil, err
		}
		if wasRetained {
			retained = append(retained, retainedCommentRef{commentUID: uid, issueUID: issue.UID})
		}
		uid = comment.ReplyToUID
	}
	return retained, nil
}

// hubComments returns the comments the hub's history of a task creates, or
// nil when the hub has no history for it. Found histories are reused for the
// rest of this push turn.
func (r *retainedPush) hubComments(ctx context.Context, issueUID string) (map[string]bool, error) {
	if comments := r.commentHistory[issueUID]; comments != nil {
		return comments, nil
	}
	history, err := r.client.issueHistoryThrough(ctx, r.hubProjectID, issueUID, math.MaxInt64)
	if err != nil || len(history) == 0 {
		return nil, err
	}
	comments := map[string]bool{}
	for _, event := range history {
		maps.Copy(comments, createdComments(event.Type, []byte(event.Payload)))
	}
	if r.commentHistory == nil {
		r.commentHistory = make(map[string]map[string]bool)
	}
	r.commentHistory[issueUID] = comments
	return comments, nil
}
