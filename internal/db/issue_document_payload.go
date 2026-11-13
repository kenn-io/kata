package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// IssueDocumentPayload is the payload of an issue.created or issue.snapshot
// event: the whole issue as its writer held it. Both stores write it and the
// fold reads it. Field order and tags are the wire format, and event content
// hashes cover the marshaled bytes, so do not reorder or retag fields.
//
// UID, ShortID, Title, Body, Author, Status, Metadata and CreatedAt are
// required: every writer since the first federation release sends them.
type IssueDocumentPayload struct {
	UID                    string                 `json:"uid"`
	ShortID                string                 `json:"short_id"`
	Title                  string                 `json:"title"`
	Body                   string                 `json:"body"`
	Author                 string                 `json:"author"`
	Owner                  *string                `json:"owner,omitempty"`
	AssignmentExpiresOn    *string                `json:"assignment_expires_on,omitempty"`
	Priority               *int64                 `json:"priority,omitempty"`
	Status                 string                 `json:"status"`
	ClosedReason           *string                `json:"closed_reason,omitempty"`
	ClosedAt               *string                `json:"closed_at,omitempty"`
	DeletedAt              *string                `json:"deleted_at,omitempty"`
	Metadata               jsontext.Value         `json:"metadata"`
	Labels                 []string               `json:"labels,omitempty"`
	Links                  []IssueDocumentLink    `json:"links,omitempty"`
	Comments               []IssueDocumentComment `json:"comments,omitempty"`
	CreatedAt              string                 `json:"created_at"`
	UpdatedAt              string                 `json:"updated_at,omitempty"`
	Revision               int64                  `json:"revision,omitzero"`
	IdempotencyKey         string                 `json:"idempotency_key,omitempty"`
	IdempotencyFingerprint string                 `json:"idempotency_fingerprint,omitempty"`
	RecurrenceUID          string                 `json:"recurrence_uid,omitempty"`
	OccurrenceKey          string                 `json:"occurrence_key,omitempty"`
	Source                 string                 `json:"source,omitempty"`
	ExternalID             string                 `json:"external_id,omitempty"`
}

// IssueDocumentLink is one link in an IssueDocumentPayload.
type IssueDocumentLink struct {
	Type       string `json:"type"`
	ToShortID  string `json:"to_short_id,omitempty"`
	ToIssueUID string `json:"to_issue_uid,omitempty"`
	Incoming   bool   `json:"incoming,omitzero"`
	Author     string `json:"author,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// IssueDocumentComment is one comment in an IssueDocumentPayload.
type IssueDocumentComment struct {
	ReplyToUID string `json:"reply_to_uid,omitempty"`
	ReplyKind  string `json:"reply_kind,omitempty"`
	EditedAt   string `json:"edited_at,omitempty"`
	CommentUID string `json:"comment_uid"`
	Author     string `json:"author"`
	Teammate   string `json:"teammate,omitempty"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}

// issueDocumentRequiredFields lists the fields every issue.created and
// issue.snapshot payload carries.
var issueDocumentRequiredFields = []string{
	"uid", "short_id", "title", "body", "author", "status", "metadata", "created_at",
}

// validateIssueDocumentPayload rejects an incoming issue.created or
// issue.snapshot payload that lacks a required field, leaves one empty other
// than the body, does not decode as an IssueDocumentPayload, or names a
// different issue than the event header.
func validateIssueDocumentPayload(event RemoteEvent) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s payload %s",
			ErrFederationIngestValidation, event.Type, fmt.Sprintf(format, args...))
	}
	fields := PayloadMap(event.Payload)
	for _, field := range issueDocumentRequiredFields {
		if raw := fields[field]; len(raw) == 0 || string(raw) == "null" {
			return invalid("has no %s", field)
		}
	}
	var document IssueDocumentPayload
	if err := json.Unmarshal(event.Payload, &document); err != nil {
		return invalid("is not an issue document: %v", err)
	}
	for field, value := range map[string]string{
		"uid": document.UID, "short_id": document.ShortID, "title": document.Title,
		"author": document.Author, "status": document.Status, "created_at": document.CreatedAt,
	} {
		if value == "" {
			return invalid("has an empty %s", field)
		}
	}
	if document.Metadata.Kind() != '{' {
		return invalid("metadata is not an object")
	}
	if document.UID != EventIssueUID(event.IssueUID) {
		return invalid("uid differs from its header issue_uid")
	}
	return nil
}
