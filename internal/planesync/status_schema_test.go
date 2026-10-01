package planesync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlaneTargetsFollowWorkflowSequenceAndValidatePinnedGroups(t *testing.T) {
	states := []State{{ID: completedStateID, Group: "completed", Sequence: 8}, {ID: testUserID, Group: "completed", Sequence: 3}, {ID: unstartedStateID, Group: "unstarted"}, {ID: testStateID, Group: "started"}, {ID: cancelledStateID, Group: "cancelled"}}
	c := testConfig()
	c.StatusSync = "two-way"
	schema, err := resolveStatusSchema(states)
	require.NoError(t, err)
	target, err := schema.target(c, "closed")
	require.NoError(t, err)
	require.Equal(t, testUserID, target)
	require.NoError(t, ValidateStatusTargets(c, states))
	c.ClosedStateID = completedStateID
	target, err = schema.target(c, "closed")
	require.NoError(t, err)
	require.Equal(t, completedStateID, target)
	c.ClosedStateID = cancelledStateID
	require.Error(t, ValidateStatusTargets(c, states))
	c.ClosedStateID = ""
	c.OpenStateID = testStateID
	require.Error(t, ValidateStatusTargets(c, states))
	c.OpenStateID = ""
	require.Error(t, ValidateStatusTargets(c, states[:2]))
	c.StatusSync = "one-way"
	require.NoError(t, ValidateStatusTargets(c, states[:2]), "one-way reads do not require write targets")
}

func TestPlaneTriageRemainsOpen(t *testing.T) {
	c := testConfig()
	batch, err := BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "triage"}}, []WorkItem{testWorkItem()})
	require.NoError(t, err)
	require.Equal(t, "open", batch.Items[0].Status)
}

func TestPlaneOneWayRetainsOverridesWithoutLiveTargets(t *testing.T) {
	for _, states := range [][]State{
		{{ID: testStateID, Group: "started"}},
		{{ID: completedStateID, Group: "started"}, {ID: unstartedStateID, Group: "completed"}},
	} {
		c := testConfig()
		c.StatusSync = "one-way"
		c.ClosedStateID = completedStateID
		c.OpenStateID = unstartedStateID
		require.NoError(t, ValidateStatusTargets(c, states), "one-way does not deliver writes to retained targets")
		c.StatusSync = "two-way"
		require.Error(t, ValidateStatusTargets(c, states))
		c.StatusSync = "one-way"
		c.ClosedStateID = "invalid"
		require.Error(t, ValidateStatusTargets(c, states), "retained overrides must still be canonical UUIDs")
	}
}
