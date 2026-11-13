package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFederationEventStreamPreservesPendingCrossProjectLink(t *testing.T) {
	peerUID := "00000000000000000000000002"
	event := Event{
		ProjectUID:      "00000000000000000000000001",
		RelatedIssueUID: &peerUID,
		Type:            "issue.linked",
	}
	issueByUID := func(string) (Issue, error) {
		return Issue{}, ErrNotFound
	}

	projectScoped := WithAuthorizedProjects(t.Context(), []string{event.ProjectUID})
	reset, err := EventRequiresProjectScopeReset(projectScoped, event, issueByUID, nil, nil)
	require.NoError(t, err)
	require.True(t, reset, "ordinary project-scoped readers hide unresolved peer identities")

	federationStream := WithFederationEventStream(projectScoped)
	reset, err = EventRequiresProjectScopeReset(federationStream, event, issueByUID, nil, nil)
	require.NoError(t, err)
	require.False(t, reset, "federation peers need the link UID before the peer issue reaches the hub")
}

// A whole-issue copy carries comments like a snapshot. A reply to a comment in
// a project the reader cannot see must not reach that reader.
func TestRepeatedImportEventHidesAReplyToAPrivateComment(t *testing.T) {
	visibleProject, privateProject := "00000000000000000000000001", "00000000000000000000000009"
	issueUID := "00000000000000000000000002"
	event := Event{
		ProjectUID: visibleProject,
		IssueUID:   &issueUID,
		Type:       RepeatedImportEventType,
		Payload:    `{"title":"t","status":"open","updated_at":"2026-06-01T09:00:00.000Z","comments":[{"comment_uid":"00000000000000000000000003","author":"alice","body":"b","created_at":"2026-05-01T10:00:00.000Z","reply_to_uid":"00000000000000000000000004","reply_kind":"reply"}]}`,
	}
	issueByUID := func(string) (Issue, error) { return Issue{ProjectUID: visibleProject}, nil }
	ctx := WithAuthorizedProjects(t.Context(), []string{visibleProject})

	reset, err := EventRequiresProjectScopeReset(ctx, event, issueByUID, nil, func(string) (Issue, error) {
		return Issue{ProjectUID: privateProject}, nil
	})
	require.NoError(t, err)
	require.True(t, reset, "the reply target is in a project the reader cannot see")

	reset, err = EventRequiresProjectScopeReset(ctx, event, issueByUID, nil, func(string) (Issue, error) {
		return Issue{ProjectUID: visibleProject}, nil
	})
	require.NoError(t, err)
	require.False(t, reset, "a reply inside the visible project stays readable")
}

// A spoke bound to one actor cannot name another author in a pushed whole
// copy's comments, as it cannot in a snapshot.
func TestBoundActorCannotForgeCommentAuthorsInARepeatedImportEvent(t *testing.T) {
	event := RemoteEvent{
		EventUID: "00000000000000000000000005",
		Type:     RepeatedImportEventType,
		Payload:  []byte(`{"title":"t","status":"open","updated_at":"2026-06-01T09:00:00.000Z","comments":[{"comment_uid":"00000000000000000000000003","author":"mallory","body":"b","created_at":"2026-05-01T10:00:00.000Z"}]}`),
	}
	err := ValidateFederationBoundActorPayload(t.Context(), event, FederationBoundActorCheck{BoundActor: "alice"})
	require.ErrorIs(t, err, ErrFederationIngestValidation)
	require.ErrorContains(t, err, "does not match bound actor")

	require.NoError(t, ValidateFederationBoundActorPayload(t.Context(), event, FederationBoundActorCheck{BoundActor: "alice", AllowSnapshotAuthorPreservation: true}))
	require.NoError(t, ValidateFederationBoundActorPayload(t.Context(), event, FederationBoundActorCheck{BoundActor: "mallory"}))
}
