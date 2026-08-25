package rootbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

func (r *Reconciler) applyInboundComments(
	ctx context.Context,
	snapshot reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	if !snapshot.binding.ReceiveComments {
		return result, nil
	}
	mapped := make(map[string]bool, len(snapshot.mappings))
	for _, mapping := range snapshot.mappings {
		if mapping.ObjectType == "comment" {
			mapped[mapping.ExternalID] = true
		}
	}
	published := make(map[string]db.ImportMapping, len(snapshot.publishedCommentMappings))
	for _, mapping := range snapshot.publishedCommentMappings {
		if mapping.ObjectType == "comment" {
			published[mapping.ExternalID] = mapping
		}
	}
	pendingCommentsToWithhold := pendingExternalCommentsToWithhold(snapshot)
	comments := append([]connector.Comment(nil), snapshot.comments...)
	sort.SliceStable(comments, func(left, right int) bool {
		if comments[left].CreatedAt.Equal(comments[right].CreatedAt) {
			return comments[left].ID < comments[right].ID
		}
		return comments[left].CreatedAt.Before(comments[right].CreatedAt)
	})
	for _, comment := range comments {
		if pendingCommentsToWithhold[comment.ID] {
			continue
		}
		if err := validateInboundComment(comment); err != nil {
			return result, err
		}
		revisionID := db.ExternalCommentRevisionMappingExternalID(comment.ID, comment.Revision)
		if snapshot.seenCommentRevisions[revisionID] && !mapped[comment.ID] {
			continue
		}
		if !snapshot.commentIdentityFrontier && snapshot.binding.ReceiveCommentsAfter != nil &&
			!comment.CreatedAt.After(*snapshot.binding.ReceiveCommentsAfter) {
			continue
		}
		updatedAt := comment.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = comment.CreatedAt
		}
		if !snapshot.commentIdentityFrontier && !mapped[comment.ID] {
			if publication, ok := published[comment.ID]; ok && publication.SourceUpdatedAt != nil &&
				!updatedAt.After(*publication.SourceUpdatedAt) {
				continue
			}
		}
		_, event, changed, err := r.store.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
			BindingID: snapshot.binding.ID, ClaimToken: claimToken,
			ExternalID: comment.ID, ExternalRevision: comment.Revision, Body: comment.Body,
			ExternalActorID: comment.Author.ID, ExternalActorName: comment.Author.DisplayName,
			ExternalCreatedAt: comment.CreatedAt, ExternalUpdatedAt: updatedAt,
			Deleted: comment.Deleted, IntegrationActor: integrationActor(snapshot.binding),
		})
		if err != nil {
			return result, err
		}
		if changed {
			if mapped[comment.ID] {
				result.CommentsEdited++
			} else {
				result.CommentsCreated++
			}
		}
		mapped[comment.ID] = true
		snapshot.seenCommentRevisions[revisionID] = true
		if event != nil {
			result.retainEvents(*event)
		}
	}
	return result, nil
}

func validateInboundComment(comment connector.Comment) error {
	if !comment.Deleted && strings.TrimSpace(comment.Body) == "" {
		return connectorProtocolFailure()
	}
	if strings.TrimSpace(comment.Revision) == "" || strings.TrimSpace(comment.Revision) != comment.Revision {
		return connectorProtocolFailure()
	}
	return nil
}

func (r *Reconciler) applyOutboundComments(
	ctx context.Context,
	snapshot reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	if !snapshot.binding.PublishComments {
		return result, nil
	}
	if !snapshot.publishReady {
		if snapshot.binding.PendingCommentUID != "" {
			return result, errors.Join(ErrCommentPublishingUnavailable, ErrPendingCommentResolutionRequired)
		}
		return result, ErrCommentPublishingUnavailable
	}
	if snapshot.binding.PendingCommentUID != "" {
		return r.resolvePendingComment(ctx, snapshot, claimToken, result)
	}
	if snapshot.binding.PublishCommentsAfter == nil {
		return result, fmt.Errorf("%w: publish-comments frontier is missing", db.ErrExternalRootValidation)
	}
	for _, comment := range snapshot.kataComments {
		if comment.IssueID != snapshot.binding.IssueID || snapshot.mappedComments[comment.ID] {
			continue
		}
		if comment.CreatedAt.Before(*snapshot.binding.PublishCommentsAfter) {
			continue
		}
		pendingAt := r.timestamp()
		if _, err := r.store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
			BindingID: snapshot.binding.ID, ClaimToken: claimToken,
			CommentUID: comment.UID, At: pendingAt,
		}); err != nil {
			return result, err
		}
		if err := r.renewBeforeExternalCall(ctx, snapshot.binding.ID, claimToken); err != nil {
			return result, err
		}
		published, err := snapshot.client.PublishComment(ctx, connector.PublishCommentParams{
			RootKey:     snapshot.binding.ExternalRootKey,
			Body:        comment.Body,
			OperationID: publicationOperationID(snapshot.binding, comment),
		})
		if err != nil {
			return result, err
		}
		if err := validatePublishedComment(comment, published, snapshot.description.SelfActorID); err != nil {
			return result, err
		}
		mapping, err := externalCommentMapping(snapshot.binding, comment, published)
		if err != nil {
			return result, err
		}
		_, event, err := r.store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
			BindingID: snapshot.binding.ID, ClaimToken: claimToken, CommentUID: comment.UID,
			ExpectedBody: comment.Body, Action: "published", Actor: integrationActor(snapshot.binding), At: r.timestamp(), Mapping: &mapping,
			ExternalRevision: published.Revision,
		})
		if err != nil {
			return result, err
		}
		result.retainEvents(event)
		snapshot.mappedComments[comment.ID] = true
	}
	return result, nil
}

func validatePublishedComment(
	local db.Comment,
	published connector.Comment,
	selfActorID string,
) error {
	valid := strings.TrimSpace(published.ID) != "" &&
		strings.TrimSpace(published.Revision) != "" && strings.TrimSpace(published.Revision) == published.Revision &&
		published.Body == local.Body &&
		strings.TrimSpace(selfActorID) != "" &&
		published.Author.ID == selfActorID &&
		!published.Deleted &&
		!published.CreatedAt.IsZero() &&
		!published.UpdatedAt.IsZero() &&
		!published.UpdatedAt.Before(published.CreatedAt)
	if !valid {
		return connectorProtocolFailure()
	}
	return nil
}

func (r *Reconciler) resolvePendingComment(
	ctx context.Context,
	snapshot reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	local, ok := pendingKataComment(snapshot)
	if !ok {
		return result, ErrPendingCommentResolutionRequired
	}
	if strings.TrimSpace(snapshot.description.SelfActorID) == "" {
		return result, errors.Join(ErrCommentPublishingUnavailable, ErrPendingCommentResolutionRequired)
	}
	if err := r.renewBeforeExternalCall(ctx, snapshot.binding.ID, claimToken); err != nil {
		return result, err
	}
	published, err := snapshot.client.PublishComment(ctx, connector.PublishCommentParams{
		RootKey:     snapshot.binding.ExternalRootKey,
		Body:        local.Body,
		OperationID: publicationOperationID(snapshot.binding, local),
	})
	if err != nil {
		return result, err
	}
	if err := validatePublishedComment(local, published, snapshot.description.SelfActorID); err != nil {
		return result, err
	}
	mapping, err := externalCommentMapping(snapshot.binding, local, published)
	if err != nil {
		return result, err
	}
	_, event, err := r.store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
		BindingID: snapshot.binding.ID, ClaimToken: claimToken, CommentUID: local.UID,
		ExpectedBody: local.Body, Action: "published", Actor: integrationActor(snapshot.binding), At: r.timestamp(), Mapping: &mapping,
		ExternalRevision: published.Revision,
	})
	if err != nil {
		return result, err
	}
	result.retainEvents(event)
	return result, nil
}

func pendingExternalCommentsToWithhold(snapshot reconcileSnapshot) map[string]bool {
	withheld := make(map[string]bool)
	local, ok := pendingKataComment(snapshot)
	if !ok {
		return withheld
	}
	mapped := make(map[string]bool, len(snapshot.mappings)+len(snapshot.publishedCommentMappings))
	for _, mapping := range snapshot.mappings {
		if mapping.ObjectType == "comment" {
			mapped[mapping.ExternalID] = true
		}
	}
	for _, mapping := range snapshot.publishedCommentMappings {
		if mapping.ObjectType == "comment" {
			mapped[mapping.ExternalID] = true
		}
	}
	for _, external := range snapshot.comments {
		if mapped[external.ID] || external.ID == "" || external.Deleted || external.Body != local.Body {
			continue
		}
		if selfActorID := strings.TrimSpace(snapshot.description.SelfActorID); selfActorID != "" &&
			external.Author.ID != selfActorID {
			continue
		}
		withheld[external.ID] = true
	}
	return withheld
}

func pendingKataComment(snapshot reconcileSnapshot) (db.Comment, bool) {
	for _, comment := range snapshot.kataComments {
		if comment.UID == snapshot.binding.PendingCommentUID {
			return comment, true
		}
	}
	return db.Comment{}, false
}

func matchesManualPendingExternalComment(external connector.Comment, selfActorID string) bool {
	return external.ID != "" && !external.Deleted && external.Author.ID == selfActorID
}

func externalCommentMapping(
	binding db.ExternalRootBinding,
	local db.Comment,
	external connector.Comment,
) (db.ImportMappingParams, error) {
	if strings.TrimSpace(external.ID) == "" {
		return db.ImportMappingParams{}, fmt.Errorf("%w: published comment identity is missing", db.ErrExternalRootValidation)
	}
	issueID, commentID := binding.IssueID, local.ID
	mapping := db.ImportMappingParams{
		Source: db.ExternalRootPublishedCommentMappingSource(binding), ExternalID: external.ID,
		ObjectType: "comment", ProjectID: binding.ProjectID,
		IssueID: &issueID, CommentID: &commentID,
	}
	updatedAt := external.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = external.CreatedAt
	}
	if !updatedAt.IsZero() {
		updatedAt = updatedAt.UTC()
		mapping.SourceUpdatedAt = &updatedAt
	}
	return mapping, nil
}

func skippedCommentMapping(
	binding db.ExternalRootBinding,
	local db.Comment,
) db.ImportMappingParams {
	issueID, commentID := binding.IssueID, local.ID
	return db.ImportMappingParams{
		Source: "connector-skip:" + binding.ConnectorInstance, ExternalID: local.UID,
		ObjectType: "comment", ProjectID: binding.ProjectID,
		IssueID: &issueID, CommentID: &commentID,
	}
}

func publicationOperationID(binding db.ExternalRootBinding, comment db.Comment) string {
	digest := sha256.Sum256([]byte("kata.external-comment.v1\x00" + binding.UID + "\x00" + comment.UID))
	return hex.EncodeToString(digest[:])
}
