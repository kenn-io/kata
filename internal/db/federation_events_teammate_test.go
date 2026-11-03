package db

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalizeFederationSnapshotAuthorsPreservesCommentTeammate(t *testing.T) {
	issueUID := "01HZZZZZZZZZZZZZZZZZZZZZ01"
	event := RemoteEvent{
		EventUID:          "01HZZZZZZZZZZZZZZZZZZZZZ02",
		OriginInstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ03",
		ProjectUID:        "01HZZZZZZZZZZZZZZZZZZZZZ04",
		ProjectName:       "example-project",
		IssueUID:          &issueUID,
		Type:              "issue.snapshot",
		Actor:             "original-author",
		HLCPhysicalMS:     1,
		CreatedAt:         time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		Payload:           jsontext.Value(`{"author":"original-author","comments":[{"comment_uid":"comment-1","author":"original-author","teammate":"reviewer-7","body":"review","created_at":"2026-09-13T12:00:00Z"}]}`),
	}
	var err error
	event.ContentHash, err = EventContentHash(EventHashInput{
		UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		CreatedAt: event.CreatedAt.Format(EventTimestampFormat), Payload: event.Payload,
	})
	require.NoError(t, err)

	got, err := CanonicalizeFederationSnapshotAuthors(event, "adoption-agent")
	require.NoError(t, err)
	var payload struct {
		Comments []struct {
			Author   string `json:"author"`
			Teammate string `json:"teammate"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(got.Payload, &payload))
	require.Len(t, payload.Comments, 1)
	assert.Equal(t, "adoption-agent", payload.Comments[0].Author)
	assert.Equal(t, "reviewer-7", payload.Comments[0].Teammate)
	wantHash, err := EventContentHash(EventHashInput{
		UID: got.EventUID, OriginInstanceUID: got.OriginInstanceUID,
		ProjectUID: got.ProjectUID, ProjectName: got.ProjectName, IssueUID: got.IssueUID,
		Type: got.Type, Actor: got.Actor, HLCPhysicalMS: got.HLCPhysicalMS,
		CreatedAt: got.CreatedAt.Format(EventTimestampFormat), Payload: got.Payload,
	})
	require.NoError(t, err)
	assert.Equal(t, wantHash, got.ContentHash)
	assert.NotEqual(t, event.ContentHash, got.ContentHash)
}

func TestValidateFederationEntries(t *testing.T) {
	for _, test := range []struct {
		name      string
		eventType string
		payload   string
		wantError bool
	}{
		{name: "comment teammate absent", eventType: "issue.commented", payload: `{"comment_uid":"comment-1"}`},
		{name: "comment teammate null", eventType: "issue.commented", payload: `{"comment_uid":"comment-1","teammate":null}`},
		{name: "comment teammate empty", eventType: "issue.commented", payload: `{"comment_uid":"comment-1","teammate":""}`},
		{name: "snapshot teammate absent", eventType: "issue.snapshot", payload: `{"comments":[{"comment_uid":"comment-1"}]}`},
		{name: "snapshot teammate null", eventType: "issue.snapshot", payload: `{"comments":[{"comment_uid":"comment-1","teammate":null}]}`},
		{name: "snapshot teammate empty", eventType: "issue.snapshot", payload: `{"comments":[{"comment_uid":"comment-1","teammate":""}]}`},
		{name: "created valid", eventType: "issue.created", payload: `{"comments":[{"comment_uid":"comment-1","teammate":"reviewer-7"}]}`},
		{name: "created teammate absent", eventType: "issue.created", payload: `{"comments":[{"comment_uid":"comment-1"}]}`},
		{name: "created teammate null", eventType: "issue.created", payload: `{"comments":[{"comment_uid":"comment-1","teammate":null}]}`},
		{name: "created teammate empty", eventType: "issue.created", payload: `{"comments":[{"comment_uid":"comment-1","teammate":""}]}`},
		{name: "metadata remains generic", eventType: "issue.snapshot", payload: `{"metadata":{"teammate":"@opaque"}}`},
		{name: "comment invalid handle", eventType: "issue.commented", payload: `{"comment_uid":"comment-1","teammate":"@invalid"}`, wantError: true},
		{name: "comment non-string", eventType: "issue.commented", payload: `{"comment_uid":"comment-1","teammate":42}`, wantError: true},
		{name: "snapshot invalid handle", eventType: "issue.snapshot", payload: `{"comments":[{"comment_uid":"comment-1","teammate":"@invalid"}]}`, wantError: true},
		{name: "snapshot non-string", eventType: "issue.snapshot", payload: `{"comments":[{"comment_uid":"comment-1","teammate":42}]}`, wantError: true},
		{name: "created invalid handle", eventType: "issue.created", payload: `{"comments":[{"comment_uid":"comment-1","teammate":"@invalid"}]}`, wantError: true},
		{name: "created non-string", eventType: "issue.created", payload: `{"comments":[{"comment_uid":"comment-1","teammate":42}]}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateFederationEntries(test.eventType, "event-1", jsontext.Value(test.payload))
			if test.wantError {
				assert.ErrorIs(t, err, ErrFederationIngestValidation)
				assert.ErrorContains(t, err, "teammate")
				assert.ErrorContains(t, err, test.eventType)
				return
			}
			require.NoError(t, err)
		})
	}
}
