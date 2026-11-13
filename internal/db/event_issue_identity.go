package db

import (
	"fmt"
	"strings"
)

// EventIssueUID returns the issue an event names: its header issue_uid, or ""
// for an event that names no issue. The payload never supplies this identity.
func EventIssueUID(header *string) string {
	if header == nil {
		return ""
	}
	return *header
}

// RemoteEventIssueUID returns the issue an incoming event names and rejects an
// event whose shape names it anywhere but the header. Every issue event must
// carry issue_uid in its header. Creations and snapshots repeat it as payload
// uid, and label changes as payload issue_uid; a repeated value must equal the
// header. Events of other types return their header value unchanged.
func RemoteEventIssueUID(event RemoteEvent) (string, error) {
	header := EventIssueUID(event.IssueUID)
	if !strings.HasPrefix(event.Type, "issue.") {
		return header, nil
	}
	if header == "" {
		return "", fmt.Errorf("%w: %s has no issue_uid in its header", ErrFederationIngestValidation, event.Type)
	}
	payload := PayloadMap(event.Payload)
	for _, field := range []string{"uid", "issue_uid"} {
		if repeated, ok := StringValue(payload[field]); ok && repeated != header {
			return "", fmt.Errorf("%w: %s payload %s differs from its header issue_uid", ErrFederationIngestValidation, event.Type, field)
		}
	}
	return header, nil
}
