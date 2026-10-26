package federation_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func TestRelayQueuedEventsPreservePerSourceTeammateAndHash(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	local, err := leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var sources []db.Event
	for i, handle := range []string{"builder-1", "reviewer-2", ""} {
		title := []string{"First edit", "Second edit", "Untagged edit"}[i]
		_, event, changed, err := leaf.store.EditIssue(db.WithEventTeammate(t.Context(), handle), db.EditIssueParams{IssueID: local.ID, Title: &title, Actor: leaf.account})
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, event)
		require.Equal(t, handle, event.Teammate)
		sources = append(sources, *event)
	}
	// A later runner's identity cannot overwrite contributors in a queued batch.
	for _, node := range []*relayMatrixNode{leaf, relay, relay, leaf} {
		binding, err := node.store.FederationBindingByProject(t.Context(), node.project.ID)
		require.NoError(t, err)
		require.NoError(t, federation.SyncFederationOnce(db.WithEventTeammate(t.Context(), "unrelated-runner"), node.store, binding, node.credential))
	}
	for _, node := range []*relayMatrixNode{root, relay, leaf} {
		for _, source := range sources {
			events, err := node.store.EventsByUIDs(t.Context(), node.project.ID, []string{source.UID})
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, source.Teammate, events[0].Teammate)
			require.Equal(t, source.Payload, events[0].Payload)
			require.Equal(t, source.ContentHash, events[0].ContentHash)
		}
	}
}
