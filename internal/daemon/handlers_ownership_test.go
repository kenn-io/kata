package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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

func TestAssign_IdentityModeOverridesBodyActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	pid := mkProject(t, env, "github.com/test/a", "a")
	issue := mkIssue(t, env, pid, "assign me")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost, issuePath(pid, issue.ID, "actions/assign"),
		map[string]string{"actor": "someone_else", "owner": "bob"},
		map[string]string{"Authorization": "Bearer alice-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var out struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Event)
	assert.Equal(t, "alice", out.Event.Actor)
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

func TestUnassign_ExpectedOwnerMatches(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postAssign(t, env, pid, n, "tester", "agent-a")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/unassign"),
		map[string]any{"actor": "tester", "expected_owner": "agent-a"}, nil)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(raw))
	var out ownerResp
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.True(t, out.Changed)
	assert.Nil(t, out.Issue.Owner)
}

func TestUnassign_ExpectedOwnerMismatch(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postAssign(t, env, pid, n, "tester", "agent-b")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/unassign"),
		map[string]any{"actor": "tester", "expected_owner": "agent-a"}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, string(raw), `"code":"owner_mismatch"`)
	assert.Contains(t, string(raw), `"expected_owner":"agent-a"`)
	assert.Contains(t, string(raw), `"current_owner":"agent-b"`)
}

func TestUnassign_ExpectedOwnerMismatchWhenUnowned(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/unassign"),
		map[string]any{"actor": "tester", "expected_owner": "agent-a"}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, string(raw), `"code":"owner_mismatch"`)
	assert.Contains(t, string(raw), `"current_owner":null`)
}

func TestUnassign_BlankExpectedOwnerIs400(t *testing.T) {
	for _, expected := range []string{"", "   "} {
		t.Run("value="+expected, func(t *testing.T) {
			env := testenv.New(t)
			pid, n := setupOneIssue(t, env)
			resp, _ := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/unassign"),
				map[string]any{"actor": "tester", "expected_owner": expected}, nil)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
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

func TestClaim_IfUnownedClaimsUnownedIssue(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "alice", "if_unowned": true}, nil)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(raw))
	var out claimResp
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.True(t, out.Changed)
	require.NotNil(t, out.Event)
}

func TestClaim_IfUnownedRejectsSameActorWithoutForceHint(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "alice", "if_unowned": true}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, string(raw), `"code":"already_claimed"`)
	assert.Contains(t, string(raw), `"current_owner":"alice"`)
	assert.NotContains(t, string(raw), "--force")
}

func TestClaim_IfUnownedRejectsDifferentActor(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "bob", "if_unowned": true}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestClaim_ForceAndIfUnownedIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "alice", "force": true, "if_unowned": true}, nil)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
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
