package federation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/kata/internal/api"
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

func (r *retainedPush) prepareCommentDependencies(ctx context.Context, event db.Event, preceding []db.Event) error {
	if event.Type == "issue.moved" {
		remote := db.FederationMoveSnapshotEvent(db.RemoteEvent{Type: event.Type, Payload: []byte(event.Payload)})
		event.Type, event.Payload = remote.Type, string(remote.Payload)
	}
	switch event.Type {
	case "issue.commented":
		payload := db.PayloadMap([]byte(event.Payload))
		parentUID, _ := db.StringValue(payload["reply_to_uid"])
		if parentUID == "" || containsCommentCreation(preceding, parentUID) {
			return nil
		}
		refs, err := r.retainedCommentChain(ctx, parentUID)
		if err != nil || len(refs) == 0 {
			return err
		}
		return r.confirmRetainedComments(ctx, refs)
	case "issue.comment_edited":
		commentUID, _ := db.StringValue(db.PayloadMap([]byte(event.Payload))["comment_uid"])
		if commentUID == "" || containsCommentCreation(preceding, commentUID) {
			return nil
		}
		refs, err := r.retainedCommentChain(ctx, commentUID)
		if err != nil || len(refs) == 0 {
			return err
		}
		return r.confirmRetainedComments(ctx, refs)
	case "issue.created", "issue.snapshot":
		var document db.IssueDocumentPayload
		if err := json.Unmarshal([]byte(event.Payload), &document); err != nil {
			return fmt.Errorf("decode retained issue discussion in %s: %w", event.UID, err)
		}
		comments := issueDocumentCreatedComments(document.Comments)
		for _, comment := range document.Comments {
			if comment.ReplyToUID == "" || comments[comment.ReplyToUID] || containsCommentCreation(preceding, comment.ReplyToUID) {
				continue
			}
			refs, err := r.retainedCommentChain(ctx, comment.ReplyToUID)
			if err != nil || len(refs) == 0 {
				if err != nil {
					return err
				}
				continue
			}
			if err := r.confirmRetainedComments(ctx, refs); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsCommentCreation(events []db.Event, commentUID string) bool {
	for _, event := range events {
		if eventCreatesComment(event, commentUID) {
			return true
		}
	}
	return false
}

func eventCreatesComment(event db.Event, commentUID string) bool {
	if event.Type == "issue.commented" {
		uid, _ := db.StringValue(db.PayloadMap([]byte(event.Payload))["comment_uid"])
		return uid == commentUID
	}
	if event.Type == "issue.moved" {
		remote := db.FederationMoveSnapshotEvent(db.RemoteEvent{Type: event.Type, Payload: []byte(event.Payload)})
		event.Type, event.Payload = remote.Type, string(remote.Payload)
	}
	if event.Type != "issue.created" && event.Type != "issue.snapshot" {
		return false
	}
	var document db.IssueDocumentPayload
	if err := json.Unmarshal([]byte(event.Payload), &document); err != nil {
		return false
	}
	return issueDocumentCreatedComments(document.Comments)[commentUID]
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
		binding := *r.binding
		if issue.ProjectID != binding.ProjectID {
			binding, err = r.store.FederationBindingByProject(ctx, issue.ProjectID)
			if errors.Is(err, db.ErrNotFound) {
				uid = comment.ReplyToUID
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		wasRetained, err := r.retainedCommentCreation(ctx, issue.UID, uid, binding)
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

func (r *retainedPush) retainedCommentCreation(ctx context.Context, issueUID, commentUID string, binding db.FederationBinding) (bool, error) {
	if binding.RetainedThroughEventID == 0 {
		return false, nil
	}
	afterID := int64(0)
	for {
		events, err := r.store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: afterID, ThroughID: binding.RetainedThroughEventID, IssueUID: issueUID,
			Types: []string{"issue.commented", "issue.created", "issue.snapshot", "issue.moved"}, Limit: federationPollLimit,
		})
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if retainedFederationEvent(event, binding, r.store.InstanceUID()) && eventCreatesComment(event, commentUID) {
				return true, nil
			}
		}
		if len(events) < federationPollLimit {
			return false, nil
		}
		next := events[len(events)-1].ID
		if next <= afterID {
			return false, errors.New("local retained comment history cursor did not advance")
		}
		afterID = next
	}
}

func (r *retainedPush) confirmRetainedComments(ctx context.Context, refs []retainedCommentRef) error {
	for _, ref := range refs {
		client, hubProjectID, cacheKey, err := r.retainedCommentHistoryClient(ctx, ref)
		if err != nil {
			return fmt.Errorf("check retained discussion comment %s: %w", ref.commentUID, err)
		}
		comments, subjectFound := r.commentHistory[cacheKey]
		if comments != nil {
			subjectFound = true
		}
		if comments == nil {
			comments, subjectFound, err = acceptedCommentHistory(ctx, client, hubProjectID, ref.issueUID)
			if err != nil {
				return fmt.Errorf("check retained discussion comment %s in hub history: %w", ref.commentUID, err)
			}
			if subjectFound {
				if r.commentHistory == nil {
					r.commentHistory = make(map[string]map[string]bool)
				}
				r.commentHistory[cacheKey] = comments
			}
		}
		if !subjectFound {
			if err := r.publishRetainedIssue(ctx, ref.issueUID); err != nil {
				return fmt.Errorf("recover task for retained discussion comment %s: %w", ref.commentUID, err)
			}
			comments, subjectFound, err = acceptedCommentHistory(ctx, client, hubProjectID, ref.issueUID)
			if err != nil {
				return fmt.Errorf("confirm retained discussion comment %s after task recovery: %w", ref.commentUID, err)
			}
			if subjectFound {
				if r.commentHistory == nil {
					r.commentHistory = make(map[string]map[string]bool)
				}
				r.commentHistory[cacheKey] = comments
			}
		}
		if !comments[ref.commentUID] {
			return &retainedDependencyError{commentUID: ref.commentUID}
		}
	}
	return nil
}

func (r *retainedPush) retainedCommentHistoryClient(ctx context.Context, ref retainedCommentRef) (*Client, int64, string, error) {
	issue, err := r.store.IssueByUID(ctx, ref.issueUID, db.IncludeDeletedYes)
	if err != nil {
		return nil, 0, "", err
	}
	binding := *r.binding
	if issue.ProjectID != binding.ProjectID {
		binding, err = r.store.FederationBindingByProject(ctx, issue.ProjectID)
		if err != nil {
			return nil, 0, "", err
		}
	}
	if issue.ProjectID == r.binding.ProjectID {
		return r.client, r.hubProjectID, fmt.Sprintf("%d:%s", r.hubProjectID, ref.issueUID), nil
	}
	client, projectID, err := r.retainedEndpointClient(ctx, ref.issueUID, binding)
	if err != nil {
		return nil, 0, "", err
	}
	return client, projectID, fmt.Sprintf("%d:%s", projectID, ref.issueUID), nil
}

func acceptedCommentHistory(ctx context.Context, client *Client, hubProjectID int64, issueUID string) (map[string]bool, bool, error) {
	comments := map[string]bool{}
	cursor := int64(0)
	for {
		page, err := client.PollIssueEvents(ctx, hubProjectID, issueUID, cursor, federationPollLimit)
		if err != nil {
			var status *HubStatusError
			if errors.As(err, &status) && status != nil && status.StatusCode == http.StatusNotFound {
				return nil, false, nil
			}
			return nil, false, err
		}
		if page.ResetRequired {
			return nil, false, ErrFederationResetRequired
		}
		for _, event := range page.Events {
			if db.EventIssueUID(event.IssueUID) != issueUID {
				return nil, false, errors.New("hub did not honor issue history filter")
			}
			for uid := range envelopeCreatedComments(event) {
				comments[uid] = true
			}
		}
		if len(page.Events) == 0 {
			return comments, true, nil
		}
		if page.NextAfterID <= cursor {
			return nil, false, errors.New("hub issue history cursor did not advance")
		}
		cursor = page.NextAfterID
	}
}

func envelopeCreatedComments(event api.EventEnvelope) map[string]bool {
	if event.Type == "issue.commented" {
		var payload struct {
			CommentUID string `json:"comment_uid"`
		}
		if json.Unmarshal([]byte(event.Payload), &payload) == nil && payload.CommentUID != "" {
			return map[string]bool{payload.CommentUID: true}
		}
		return nil
	}
	remote := db.RemoteEvent{Type: event.Type, Payload: []byte(event.Payload)}
	remote = db.FederationMoveSnapshotEvent(remote)
	if remote.Type != "issue.created" && remote.Type != "issue.snapshot" {
		return nil
	}
	var document db.IssueDocumentPayload
	if json.Unmarshal([]byte(remote.Payload), &document) != nil {
		return nil
	}
	return issueDocumentCreatedComments(document.Comments)
}
