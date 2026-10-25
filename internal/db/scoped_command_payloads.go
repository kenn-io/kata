package db

import (
	"encoding/json/jsontext"
)

type NativeCreatedLinkPayload struct {
	Type       string `json:"type"`
	ToShortID  string `json:"to_short_id,omitempty"`
	ToIssueUID string `json:"to_issue_uid,omitempty"`
	Incoming   bool   `json:"incoming,omitzero"`
	Author     string `json:"author,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

type NativeIssueSnapshotCommentPayload struct {
	ReplyToUID string `json:"reply_to_uid,omitempty"`
	ReplyKind  string `json:"reply_kind,omitempty"`
	EditedAt   string `json:"edited_at,omitempty"`
	CommentUID string `json:"comment_uid"`
	Author     string `json:"author"`
	Teammate   string `json:"teammate,omitempty"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}

type NativeIssueCreatedPayload struct {
	CloseState             *CanonicalCloseState                `json:"close_state,omitempty"`
	UID                    string                              `json:"uid"`
	ShortID                string                              `json:"short_id"`
	Title                  string                              `json:"title"`
	Body                   string                              `json:"body"`
	Author                 string                              `json:"author"`
	Owner                  *string                             `json:"owner,omitempty"`
	AssignmentExpiresOn    *string                             `json:"assignment_expires_on,omitempty"`
	Priority               *int64                              `json:"priority,omitempty"`
	Status                 string                              `json:"status"`
	ClosedReason           *string                             `json:"closed_reason,omitempty"`
	ClosedAt               *string                             `json:"closed_at,omitempty"`
	DeletedAt              *string                             `json:"deleted_at,omitempty"`
	Metadata               jsontext.Value                      `json:"metadata"`
	Labels                 []string                            `json:"labels,omitempty"`
	Links                  []NativeCreatedLinkPayload          `json:"links,omitempty"`
	Comments               []NativeIssueSnapshotCommentPayload `json:"comments,omitempty"`
	CreatedAt              string                              `json:"created_at"`
	UpdatedAt              string                              `json:"updated_at,omitempty"`
	Revision               int64                               `json:"revision,omitzero"`
	IdempotencyKey         string                              `json:"idempotency_key,omitempty"`
	IdempotencyFingerprint string                              `json:"idempotency_fingerprint,omitempty"`
	RecurrenceUID          string                              `json:"recurrence_uid,omitempty"`
	OccurrenceKey          string                              `json:"occurrence_key,omitempty"`
	Source                 string                              `json:"source,omitempty"`
	ExternalID             string                              `json:"external_id,omitempty"`
}
