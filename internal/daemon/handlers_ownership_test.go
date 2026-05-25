package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestAssign_HappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, out := postAssign(t, env, pid, n, "tester", "alice")
	require.Equal(t, 200, resp.StatusCode)
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "alice", *out.Issue.Owner)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.assigned", out.Event.Type)
	assert.True(t, out.Changed)
}

func TestAssign_SameOwnerIsNoOp(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postAssign(t, env, pid, n, "tester", "alice")
	require.Equal(t, 200, resp.StatusCode)

	resp, out := postAssign(t, env, pid, n, "tester", "alice")
	require.Equal(t, 200, resp.StatusCode)
	assert.Nil(t, out.Event)
	assert.False(t, out.Changed)
}

func TestAssign_BlankOwnerIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postAssign(t, env, pid, n, "tester", "   ")
	assert.Equal(t, 400, resp.StatusCode)
}

func TestAssign_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postAssign(t, env, pid, n, "   ", "alice")
	assert.Equal(t, 400, resp.StatusCode)
}

func TestAssign_TrimsActorAndOwner(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, out := postAssign(t, env, pid, n, " tester ", " alice ")
	require.Equal(t, 200, resp.StatusCode)
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "alice", *out.Issue.Owner)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.assigned", out.Event.Type)
}

func TestUnassign_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postUnassign(t, env, pid, n, "   ")
	assert.Equal(t, 400, resp.StatusCode)
}

func TestUnassign_HappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postAssign(t, env, pid, n, "tester", "alice")
	require.Equal(t, 200, resp.StatusCode)

	resp, out := postUnassign(t, env, pid, n, "tester")
	require.Equal(t, 200, resp.StatusCode)
	assert.Nil(t, out.Issue.Owner)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.unassigned", out.Event.Type)
	assert.True(t, out.Changed)
}

func TestClaim_UnownedIssue(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, out := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, 200, resp.StatusCode)
	assert.True(t, out.Changed)
	assert.Nil(t, out.PreviousOwner)
	require.NotNil(t, out.Event)
}

func TestClaim_AlreadyOwnedBySameActor(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	// First claim
	resp, _ := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, 200, resp.StatusCode)

	// Second claim by same actor
	resp, out := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, 200, resp.StatusCode)
	assert.False(t, out.Changed)
	assert.Nil(t, out.Event)
	assert.Nil(t, out.PreviousOwner)
}

func TestClaim_AlreadyOwnedByDifferentActor(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	// Claim by alice
	resp, _ := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, 200, resp.StatusCode)

	// Try to claim by bob without force
	resp, _ = postClaim(t, env, pid, n, "bob", false)
	assert.Equal(t, 409, resp.StatusCode)
}

func TestClaim_ForceReassign(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	// Claim by alice
	resp, _ := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, 200, resp.StatusCode)

	// Claim by bob with force
	resp, out := postClaim(t, env, pid, n, "bob", true)
	require.Equal(t, 200, resp.StatusCode)
	assert.True(t, out.Changed)
	require.NotNil(t, out.PreviousOwner)
	assert.Equal(t, "alice", *out.PreviousOwner)
	require.NotNil(t, out.Event)
}

func TestClaim_TrimsActorBeforePersistingOwner(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, out := postClaim(t, env, pid, n, " alice ", false)
	require.Equal(t, 200, resp.StatusCode)
	assert.True(t, out.Changed)

	issue, err := env.DB.IssueByID(t.Context(), n)
	require.NoError(t, err)
	var owner *string
	require.NoError(t, env.DB.QueryRowContext(t.Context(),
		`SELECT owner FROM issues WHERE project_id = ? AND short_id = ?`,
		pid, issue.ShortID).Scan(&owner))
	require.NotNil(t, owner)
	assert.Equal(t, "alice", *owner)
}
