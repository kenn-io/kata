package db

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	// ErrCommentReplyClosed reports a closed reply source issue.
	ErrCommentReplyClosed = errors.New("reply issue must be open")
	// ErrCommentReplyTarget reports an unavailable or cross-project target.
	ErrCommentReplyTarget = errors.New("reply target must be a visible comment in the same project")
	// ErrCommentReplyInvalid reports malformed reply fields or insufficient evidence.
	ErrCommentReplyInvalid = errors.New("invalid comment reply")
)

// DuplicateCommentReplyError identifies a prior assertion by the same actor
// and teammate. API callers must authorize its source before exposing it.
type DuplicateCommentReplyError struct{ Comment Comment }

func (e *DuplicateCommentReplyError) Error() string {
	return "a reply of this kind already exists for this actor and teammate"
}

// ValidateLocalCommentReply applies local write contracts to rows read inside
// the comment-create transaction. A nil target means it could not be found.
// Duplicate detection is project-wide and uses the effective attributed actor.
func ValidateLocalCommentReply(commentUID string, p CreateCommentParams, source Issue, target *Issue, duplicate *Comment) error {
	if !p.ValidateReply {
		return nil
	}
	if strings.TrimSpace(p.Body) == "" {
		return fmt.Errorf("%w: comment body is required", ErrCommentReplyInvalid)
	}
	if err := ValidateCommentReply(commentUID, p.ReplyToUID, p.ReplyKind); err != nil {
		return fmt.Errorf("%w: %v", ErrCommentReplyInvalid, err)
	}
	if p.ReplyToUID == "" {
		return nil
	}
	if source.Status != "open" {
		return ErrCommentReplyClosed
	}
	if target == nil || target.ProjectID != source.ProjectID || target.DeletedAt != nil {
		return ErrCommentReplyTarget
	}
	if (p.ReplyKind == "confirm" || p.ReplyKind == "refute") && utf8.RuneCountInString(strings.Join(strings.Fields(p.Body), " ")) < 40 {
		return fmt.Errorf("%w: %s body needs at least 40 characters after whitespace normalization", ErrCommentReplyInvalid, p.ReplyKind)
	}
	if duplicate != nil && !p.Force {
		return &DuplicateCommentReplyError{Comment: *duplicate}
	}
	return nil
}
