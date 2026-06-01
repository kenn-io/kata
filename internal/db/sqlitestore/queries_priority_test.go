package sqlitestore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCreateIssue_WithInitialPriority(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	for _, prio := range []int64{0, 1, 2, 3, 4} {
		prio := prio
		t.Run(fmt.Sprintf("%s_p%d", t.Name(), prio), func(t *testing.T) {
			issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: p.ID, Title: "x", Author: "tester",
				Priority: &prio,
			})
			require.NoError(t, err)
			require.NotNil(t, issue.Priority)
			assert.Equal(t, prio, *issue.Priority)

			payload := unmarshalPayload[struct {
				Priority *int64 `json:"priority"`
			}](t, evt.Payload)
			require.NotNil(t, payload.Priority)
			assert.Equal(t, prio, *payload.Priority)
		})
	}
}

func TestCreateIssue_NoPriorityIsNullAndOmittedFromPayload(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)
	assert.Nil(t, issue.Priority)
	payload := unmarshalPayload[map[string]any](t, evt.Payload)
	assert.NotContains(t, payload, "priority")
}

func TestCreateIssue_RejectsPriorityOutOfRange(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	for _, bad := range []int64{-1, 5, 99} {
		bad := bad
		_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID, Title: "x", Author: "tester",
			Priority: &bad,
		})
		require.Error(t, err, "priority %d should violate CHECK", bad)
		// The schema CHECK fires at INSERT; surface includes "CHECK" in the
		// driver-level message regardless of the specific wrapper kata adds.
		assert.True(t, strings.Contains(strings.ToLower(err.Error()), "check"),
			"error %q should mention CHECK constraint", err.Error())
	}
}

func TestIssueByNumber_SurfacesPriority(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	prio := int64(2)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Priority: &prio,
	})
	require.NoError(t, err)

	got, err := d.IssueByID(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Priority)
	assert.Equal(t, prio, *got.Priority)
}

func TestUpdatePriority_SetsAndEmitsPrioritySetEvent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)
	require.Nil(t, created.Priority)

	newPrio := int64(1)
	updated, evt, changed, err := d.UpdatePriority(ctx, created.ID, &newPrio, "tester")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.priority_set", evt.Type)
	require.NotNil(t, updated.Priority)
	assert.Equal(t, newPrio, *updated.Priority)

	payload := unmarshalPayload[struct {
		Priority    int64  `json:"priority"`
		OldPriority *int64 `json:"old_priority,omitempty"`
	}](t, evt.Payload)
	assert.Equal(t, newPrio, payload.Priority)
	assert.Nil(t, payload.OldPriority, "old_priority omitted when prior value was unset")
}

func TestUpdatePriority_ChangesAndEmitsOldPriorityInPayload(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	old := int64(3)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Priority: &old,
	})
	require.NoError(t, err)

	newPrio := int64(0)
	_, evt, changed, err := d.UpdatePriority(ctx, created.ID, &newPrio, "tester")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.priority_set", evt.Type)

	payload := unmarshalPayload[struct {
		Priority    int64  `json:"priority"`
		OldPriority *int64 `json:"old_priority,omitempty"`
	}](t, evt.Payload)
	assert.Equal(t, newPrio, payload.Priority)
	require.NotNil(t, payload.OldPriority)
	assert.Equal(t, old, *payload.OldPriority)
}

func TestUpdatePriority_ClearsAndEmitsPriorityClearedEvent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	old := int64(2)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Priority: &old,
	})
	require.NoError(t, err)

	updated, evt, changed, err := d.UpdatePriority(ctx, created.ID, nil, "tester")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.priority_cleared", evt.Type)
	assert.Nil(t, updated.Priority)

	payload := unmarshalPayload[struct {
		OldPriority int64 `json:"old_priority"`
	}](t, evt.Payload)
	assert.Equal(t, old, payload.OldPriority)
}

func TestUpdatePriority_NoOpWhenSameValue(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	prio := int64(1)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Priority: &prio,
	})
	require.NoError(t, err)

	same := int64(1)
	_, evt, changed, err := d.UpdatePriority(ctx, created.ID, &same, "tester")
	require.NoError(t, err)
	assert.False(t, changed, "same priority must not record a change")
	assert.Nil(t, evt, "no event when nothing changed")
}

func TestUpdatePriority_NoOpWhenClearingUnset(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)

	_, evt, changed, err := d.UpdatePriority(ctx, created.ID, nil, "tester")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, evt)
}

func TestUpdatePriority_RejectsOutOfRange(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)

	for _, bad := range []int64{-1, 5, 99} {
		bad := bad
		_, _, _, err := d.UpdatePriority(ctx, created.ID, &bad, "tester")
		require.Error(t, err, "priority %d should violate CHECK", bad)
	}
}
