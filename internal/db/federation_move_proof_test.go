package db

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFederationMoveHistoryProvesComments(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	history := []FoldEvent{{UID: "event", IssueUID: "issue", Type: "issue.commented", Actor: "old-actor", HLCPhysicalMS: 1, CreatedAt: created.Format(time.RFC3339Nano), Payload: jsontext.Value(`{"comment_uid":"comment","author":"old-actor","body":"original","created_at":"2026-01-01T00:00:00Z"}`)}}
	comment := Comment{UID: "comment", Author: "old-actor", Body: "original", CreatedAt: created}
	require.True(t, FederationMoveHistoryProvesComments("issue", "source", history, []Comment{comment}))
	comment.Author = "other-actor"
	require.False(t, FederationMoveHistoryProvesComments("issue", "source", history, []Comment{comment}))
	comment.Author, comment.Body = "old-actor", "newer"
	edited := created.Add(time.Hour)
	comment.EditedAt = &edited
	require.False(t, FederationMoveHistoryProvesComments("issue", "source", history, []Comment{comment}))
}

func TestFederationMoveHistoryProvesCommentsUsesDepartureBaseline(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	history := []FoldEvent{
		{UID: "comment", ProjectUID: "source", IssueUID: "issue", Type: "issue.commented", Actor: "old-actor", HLCPhysicalMS: 1, CreatedAt: created.Format(time.RFC3339Nano), Payload: jsontext.Value(`{"comment_uid":"comment","author":"old-actor","body":"original","created_at":"2026-01-01T00:00:00Z"}`)},
		{UID: "snapshot", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"uid":"issue","comments":[{"comment_uid":"comment","author":"new-actor","body":"original","created_at":"2026-01-01T00:00:00Z"}]}`)},
	}
	comment := Comment{UID: "comment", Author: "old-actor", Body: "original", CreatedAt: created}
	require.False(t, FederationMoveHistoryProvesComments("issue", "source", history, []Comment{comment}))
}
