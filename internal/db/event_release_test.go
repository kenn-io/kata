package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A relayed event can name its subject by UID alone. Releasing it registers
// the issue the UID resolves to inside the caller's boundary. A UID the
// boundary cannot resolve fails closed into a reset and registers nothing.
func TestEventIssueLookupRegistersSubjectNamedByUID(t *testing.T) {
	const projectUID = "00000000000000000000000001"
	visibleUID, hiddenUID := "00000000000000000000000002", "00000000000000000000000003"
	lookups := 0
	lookup := NewEventIssueLookup(func(uid string) (Issue, error) {
		lookups++
		if uid == visibleUID {
			return Issue{ID: 41, UID: visibleUID, ProjectUID: projectUID}, nil
		}
		return Issue{}, ErrNotFound
	}, func(string, string) (Issue, error) { return Issue{}, ErrNotFound })
	ctx := WithExposedIssues(WithAuthorizedProjects(context.Background(), []string{projectUID}))

	for _, tc := range []struct {
		uid   string
		reset bool
	}{{visibleUID, false}, {hiddenUID, true}, {visibleUID, false}} {
		reset, err := lookup.RequiresProjectScopeReset(ctx, Event{
			ProjectUID: projectUID, IssueUID: &tc.uid, Type: "issue.snapshot", Payload: `{}`,
		})
		require.NoError(t, err)
		assert.Equal(t, tc.reset, reset, tc.uid)
	}
	assert.Equal(t, []int64{41}, ExposedIssues(ctx))
	assert.Equal(t, 2, lookups, "each UID resolves once per read")

	purgedUID := "00000000000000000000000004"
	reset, err := lookup.RequiresProjectScopeReset(context.Background(), Event{
		ProjectUID: projectUID, IssueUID: &purgedUID, Type: "issue.snapshot", Payload: `{}`,
	})
	require.NoError(t, err)
	assert.False(t, reset, "an unrestricted read has no boundary to fail closed against")
	assert.Equal(t, 2, lookups, "an unrestricted read without a registry resolves nothing extra")
}
