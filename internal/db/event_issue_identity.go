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
// issue event whose shape no writer produces. Every issue event must carry
// issue_uid in its header, and a payload uid or issue_uid on any issue event
// must equal that header. Creations and snapshots must also carry a whole issue
// document (see validateIssueDocumentPayload). Events of other types return
// their header value unchanged.
//
// Call it only where events enter over federation. Stored events and imports
// are not checked again.
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
	if event.Type == "issue.created" || event.Type == "issue.snapshot" {
		if err := validateIssueDocumentPayload(event); err != nil {
			return "", err
		}
	}
	return header, nil
}
