package db

import (
	"context"
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

	projectScoped := WithAuthorizedProjects(context.Background(), []string{event.ProjectUID})
	reset, err := EventRequiresProjectScopeReset(projectScoped, event, issueByUID, nil)
	require.NoError(t, err)
	require.True(t, reset, "ordinary project-scoped readers hide unresolved peer identities")

	federationStream := WithFederationEventStream(projectScoped)
	reset, err = EventRequiresProjectScopeReset(federationStream, event, issueByUID, nil)
	require.NoError(t, err)
	require.False(t, reset, "federation peers need the link UID before the peer issue reaches the hub")
}
