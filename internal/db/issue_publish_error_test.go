package db

import (
	"strings"
	"testing"
)

func TestIssuePublicationPendingErrorOmitsRepeatedAttemptID(t *testing.T) {
	const attemptUID = "attempt-example"
	err := &IssuePublicationPendingError{
		AttemptUID: attemptUID,
		Provider:   "notion",
		IssueUID:   "issue-example",
		IssueRef:   "spoke-project#abc4",
		Phase:      "dispatching",
	}

	message := err.Error()
	if !strings.HasPrefix(message, "unresolved publication (dispatching);") {
		t.Fatalf("recovery message should begin with its phase, got %q", message)
	}
	if got := strings.Count(message, attemptUID); got != 1 {
		t.Fatalf("recovery message should include attempt ID only in --attempt, got %d occurrences in %q", got, message)
	}
	if !strings.Contains(message, "--attempt "+attemptUID) {
		t.Fatalf("recovery message should retain the --attempt argument, got %q", message)
	}
}
