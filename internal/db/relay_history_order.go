package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// RelayHistoryNeedsReset reports whether retained history requires a signed
// current-state checkpoint because an issue mutation precedes its introduction.
// Callers supply events in stored numeric ID order. History is never reordered
// or rewritten; a later snapshot cannot repair an already invalid replay prefix.
// This checks introduction order only, not hashes or cross-project references.
func RelayHistoryNeedsReset(events []EventExport) (bool, error) {
	var history RelayHistoryOrderValidator
	for _, event := range events {
		if reset, err := history.Observe(event); err != nil || reset {
			return reset, err
		}
	}
	return false, nil
}

// RelayHistoryOrderValidator checks a stream in stored numeric ID order while
// retaining only introduced issue UIDs. Its zero value is ready to use.
type RelayHistoryOrderValidator struct {
	introduced map[string]struct{}
	reset      bool
}

// Observe checks the next retained event without retaining its payload. Once
// an invalid replay prefix is found, later snapshots cannot repair it.
func (history *RelayHistoryOrderValidator) Observe(event EventExport) (bool, error) {
	// Project catalog UIDs are not issue introductions.
	if history.reset || !strings.HasPrefix(event.Type, "issue.") {
		return history.reset, nil
	}
	var payload map[string]jsontext.Value
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return false, fmt.Errorf("%w: event %d has invalid payload", ErrFederationIngestValidation, event.ID)
	}
	envelopeUID := ""
	if event.IssueUID != nil {
		envelopeUID = *event.IssueUID
	}
	issueUID := resolveIssueUID(envelopeUID, payload)
	if issueUID == "" {
		return false, fmt.Errorf("%w: event %d missing issue uid", ErrFederationIngestValidation, event.ID)
	}
	switch event.Type {
	case "issue.created", "issue.snapshot":
		if history.introduced == nil {
			history.introduced = make(map[string]struct{})
		}
		history.introduced[issueUID] = struct{}{}
	default:
		_, introduced := history.introduced[issueUID]
		history.reset = !introduced
	}
	return history.reset, nil
}
