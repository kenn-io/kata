package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
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

func TestClaim_IdentityModeDerivesActorWhenBodyOmitsIt(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	pid := mkProject(t, env, "github.com/test/a", "a")
	issue := mkIssue(t, env, pid, "claim me")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, issue.ID, "actions/claim"),
		map[string]any{"ttl_seconds": 300},
		map[string]string{"Authorization": "Bearer alice-token"})

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	stored, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner)
	assert.Equal(t, "alice", *stored.Owner)
}

func TestClaim_TimedAssignmentUsesDaemonTimeAndReturnsOrderedEvents(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	before := time.Now().UTC()

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "alice", "ttl_seconds": 120}, nil)
	after := time.Now().UTC()

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var out struct {
		Issue   db.Issue   `json:"issue"`
		Event   *db.Event  `json:"event"`
		Events  []db.Event `json:"events"`
		Changed bool       `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.True(t, out.Changed)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)
	assert.False(t, out.Issue.AssignmentExpiresOn.Before(before.Add(119*time.Second)))
	assert.False(t, out.Issue.AssignmentExpiresOn.After(after.Add(120*time.Second)))
	require.Len(t, out.Events, 1)
	assert.Equal(t, "issue.assigned", out.Events[0].Type)
	require.NotNil(t, out.Event)
	assert.Equal(t, out.Events[0].UID, out.Event.UID)
}

func TestClaim_TimedAssignmentRenewsAndExpiredTakeoverReturnsBothEvents(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	issue, err := env.DB.IssueByID(t.Context(), n)
	require.NoError(t, err)
	oldNow := time.Now().UTC().Add(-5 * time.Minute)
	_, err = env.DB.ClaimOwner(t.Context(), db.ClaimOwnerParams{
		IssueID: issue.ID,
		Actor:   "alice",
		TTL:     time.Minute,
		Now:     oldNow,
	})
	require.NoError(t, err)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: pid})
	defer sub.Unsub()

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "bob", "ttl_seconds": 300}, nil)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var out struct {
		Issue  db.Issue   `json:"issue"`
		Event  *db.Event  `json:"event"`
		Events []db.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Events, 2)
	assert.Equal(t, "issue.assignment_expired", out.Events[0].Type)
	assert.Equal(t, "issue.assigned", out.Events[1].Type)
	require.NotNil(t, out.Event)
	assert.Equal(t, out.Events[1].UID, out.Event.UID)
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "bob", *out.Issue.Owner)
	first := receiveMsg(t, sub.Ch, time.Second, "assignment expiry broadcast")
	second := receiveMsg(t, sub.Ch, time.Second, "replacement assignment broadcast")
	require.NotNil(t, first.Event)
	require.NotNil(t, second.Event)
	assert.Equal(t, "issue.assignment_expired", first.Event.Type)
	assert.Equal(t, "issue.assigned", second.Event.Type)
}

func TestClaim_TimedAssignmentConflictIncludesExpiry(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "alice", "ttl_seconds": 300}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "bob", "ttl_seconds": 300}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, string(raw), `"code":"already_claimed"`)
	assert.Contains(t, string(raw), `"current_owner":"alice"`)
	assert.Contains(t, string(raw), `"assignment_expires_on":`)
}

func TestClaim_TimeoutBounds(t *testing.T) {
	for _, seconds := range []int64{59, 86401} {
		t.Run(fmt.Sprintf("seconds=%d", seconds), func(t *testing.T) {
			env := testenv.New(t)
			pid, n := setupOneIssue(t, env)
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
				map[string]any{"actor": "alice", "ttl_seconds": seconds}, nil)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, string(raw), "ttl_seconds")
		})
	}
}

func TestClaim_TimedAssignmentForwardsToFederationHubAndProjectsResult(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))

	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "tester", *out.Issue.Owner)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)
	assert.Equal(t, spokeProject.ID, out.Issue.ProjectID)
	require.Len(t, out.Events, 1)
	assert.Equal(t, "issue.assigned", out.Events[0].Type)

	hubIssue, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, hubIssue.Owner)
	assert.Equal(t, "tester", *hubIssue.Owner)
	assert.Equal(t, out.Issue.AssignmentExpiresOn, hubIssue.AssignmentExpiresOn)
	spokeIssue, err := spoke.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, out.Issue.AssignmentExpiresOn, spokeIssue.AssignmentExpiresOn)
}

func TestClaim_TimedAssignmentForwardsWithExistingFederationLease(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Capabilities: "claim",
	}))
	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		claimActionPath(spokeProject.ID, issue.ShortID, "acquire"),
		map[string]any{"holder": "tester", "claim_kind": "timed", "ttl_seconds": int64(time.Hour / time.Second)}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "lease response: %s", raw)

	resp, raw = envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "assignment response: %s", raw)
}

func TestClaim_TimedAssignmentRenewalRepairsMissingPredecessor(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Capabilities: "claim",
	}))
	_, err := hub.DB.ClaimOwner(t.Context(), db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "tester", TTL: time.Hour,
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "assignment response: %s", raw)
	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "tester", *out.Issue.Owner)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)

	stored, err := spoke.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, out.Issue.Owner, stored.Owner)
	assert.Equal(t, out.Issue.AssignmentExpiresOn, stored.AssignmentExpiresOn)
}

func TestClaim_TimedAssignmentRenewalRepairsMissingIntermediateRenewal(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Capabilities: "claim",
	}))

	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 3600}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "initial assignment response: %s", raw)
	intermediate, err := hub.DB.ClaimOwner(t.Context(), db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "tester", TTL: 2 * time.Hour,
	})
	require.NoError(t, err)
	require.True(t, intermediate.Changed)
	require.Equal(t, "issue.assignment_renewed", intermediate.Events[len(intermediate.Events)-1].Type)

	resp, raw = envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "renewal response: %s", raw)
	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.AssignmentExpiresOn)

	stored, err := spoke.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	authoritative, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, authoritative.AssignmentExpiresOn, out.Issue.AssignmentExpiresOn)
	assert.Equal(t, out.Issue.Owner, stored.Owner)
	assert.Equal(t, out.Issue.AssignmentExpiresOn, stored.AssignmentExpiresOn)
}

func TestClaim_TimedAssignmentPullOnlySpokeReturnsFederatedReadOnly(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))
	binding, err := spoke.DB.FederationBindingByProject(t.Context(), spokeProject.ID)
	require.NoError(t, err)
	binding.PushEnabled = false
	_, err = spoke.DB.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)

	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)

	assert.Equalf(t, http.StatusConflict, resp.StatusCode, "body: %s", raw)
	assert.Contains(t, string(raw), `"code":"federated_read_only"`)
	hubIssue, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Nil(t, hubIssue.Owner, "pull-only spoke must not forward the timed assignment to the hub")
	assert.Nil(t, hubIssue.AssignmentExpiresOn)
	spokeIssue, err := spoke.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Nil(t, spokeIssue.Owner)
	assert.Nil(t, spokeIssue.AssignmentExpiresOn)
}

func TestClaim_TimedAssignmentFederationOfflineDoesNotMutateSpoke(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	offlineURL := fastFailClaimHubURL(t)
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))
	setClaimBindingHubURL(t, spoke, spokeProject.ID, offlineURL)

	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)

	assert.Equalf(t, http.StatusServiceUnavailable, resp.StatusCode, "body: %s", raw)
	stored, err := spoke.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Nil(t, stored.Owner)
	assert.Nil(t, stored.AssignmentExpiresOn)
}

func TestClaim_EnrollmentBearerRequiresTTLSeconds(t *testing.T) {
	hub, _, hubProject, _, issue, token := createClaimForwardingPair(t, "claim")
	_, err := hub.DB.ClaimOwner(t.Context(), db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "hub-owner", TTL: time.Hour,
	})
	require.NoError(t, err)

	headers := map[string]string{"Authorization": "Bearer " + token}
	resp, raw := envDoRaw(t, hub, http.MethodPost,
		issuePathRef(hubProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "spoke-agent", "force": true}, headers)
	require.Equalf(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
	assert.Contains(t, string(raw), `"code":"validation"`)
	assert.Contains(t, string(raw), "ttl_seconds")

	stored, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner, "rejected claim must preserve the existing owner")
	assert.Equal(t, "hub-owner", *stored.Owner)
	require.NotNil(t, stored.AssignmentExpiresOn)

	// A timed enrollment claim stays allowed: force succeeds and the
	// assignment is bounded by the requested TTL. Enrollment claims
	// attribute to the enrollment actor, not the body actor.
	resp, raw = envDoRaw(t, hub, http.MethodPost,
		issuePathRef(hubProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "spoke-agent", "force": true, "ttl_seconds": 300}, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "tester", *out.Issue.Owner)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)
}

func TestClaim_AlreadyOwnedBySameActor(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	// First claim
	resp, _ := postClaim(t, env, pid, n, "alice", false)
	require.Equal(t, 200, resp.StatusCode)

	// Second claim by same actor
	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
		map[string]any{"actor": "alice"}, nil)
	require.Equal(t, 200, resp.StatusCode)
	var out claimResp
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.False(t, out.Changed)
	assert.Nil(t, out.Event)
	assert.Nil(t, out.PreviousOwner)
	assert.Contains(t, string(raw), `"events":[]`)
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

func TestClaim_RejectsBlankActor(t *testing.T) {
	for _, actor := range []string{"", "  "} {
		t.Run(strconv.Quote(actor), func(t *testing.T) {
			env := testenv.New(t)
			pid, n := setupOneIssue(t, env)
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "actions/claim"),
				map[string]any{"actor": actor}, nil)

			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, string(raw), `"code":"validation"`)
			assert.Contains(t, string(raw), "actor is required")
		})
	}
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

func TestClaim_HonorsBrowserSessionPrincipal(t *testing.T) {
	d := openTestDB(t)
	project, err := d.db.CreateProject(t.Context(), "example-workspace")
	require.NoError(t, err)
	issue, _, err := d.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "claim through browser", Author: "setup",
	})
	require.NoError(t, err)

	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: "http://127.0.0.1:27123", InstanceID: "instance_a", Writable: true,
		Updates: "sse", Auth: config.AuthConfig{Token: "static-token"}, DB: d.db,
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(daemon.Principal{Kind: daemon.PrincipalStaticToken}, "/kata")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, WebSessions: manager, Auth: config.AuthConfig{Token: "static-token"},
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(daemon.ListenerPolicy{
		Kind: daemon.ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
	})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:27123"+issuePathRef(project.ID, issue.ShortID, "actions/claim"),
		strings.NewReader(`{"actor":"worker","ttl_seconds":300}`))
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:27123")
	request.Header.Set("X-Kata-Web-Session", issued.Session)
	request.Header.Set("X-Kata-CSRF", issued.CSRF)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
		net.Addr(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123})))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
	stored, err := d.db.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner)
	assert.Equal(t, "worker", *stored.Owner)
}

func TestClaim_HonorsLocalBrowserSessionPrincipal(t *testing.T) {
	d := openTestDB(t)
	project, err := d.db.CreateProject(t.Context(), "example-workspace")
	require.NoError(t, err)
	issue, _, err := d.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "claim through local browser", Author: "setup",
	})
	require.NoError(t, err)

	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: "http://127.0.0.1:27123", InstanceID: "instance_a", Writable: true,
		Updates: "sse", Auth: config.AuthConfig{}, DB: d.db,
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(daemon.Principal{Kind: daemon.PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, WebSessions: manager, Auth: config.AuthConfig{},
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(daemon.ListenerPolicy{
		Kind: daemon.ListenerBrowser, Origin: "http://127.0.0.1:27123",
		RequireBrowserSession: true, AllowLocalSession: true,
	})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:27123"+issuePathRef(project.ID, issue.ShortID, "actions/claim"),
		strings.NewReader(`{"actor":"worker","ttl_seconds":300}`))
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:27123")
	request.Header.Set("X-Kata-Web-Session", issued.Session)
	request.Header.Set("X-Kata-CSRF", issued.CSRF)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
		net.Addr(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123})))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
	stored, err := d.db.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner)
	assert.Equal(t, "worker", *stored.Owner)
}

type assignmentHostAccess struct{}

func (assignmentHostAccess) Authorize(
	context.Context, daemon.HostAccessRequest,
) (daemon.HostAccessDecision, error) {
	return daemon.HostAccessDecision{
		TransactionFence: func(context.Context, db.Transaction) error { return nil },
	}, nil
}

func TestClaim_HostPrincipalUsesAttributedActorForAssignment(t *testing.T) {
	d := openTestDB(t)
	project, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := d.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "claim", Author: "setup",
	})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, HostAccess: assignmentHostAccess{},
	})
	t.Cleanup(func() { _ = server.Close() })
	request := httptest.NewRequest(http.MethodPost,
		issuePathRef(project.ID, issue.ShortID, "actions/claim"),
		strings.NewReader(`{"actor":"ignored","ttl_seconds":300}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{
		Kind: daemon.PrincipalHost, Subject: "subject-a", Actor: "alice",
	}))
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	require.Equalf(t, http.StatusOK, response.Code, "response: %s", response.Body.String())
	stored, err := d.db.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner)
	assert.Equal(t, "alice", *stored.Owner)
}

func TestClaim_HonorsTrustedProxyActor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantStatus int
		wantOwner  string
	}{
		{name: "verified actor", header: "alice", wantStatus: http.StatusOK, wantOwner: "alice"},
		{name: "missing actor", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			project, err := d.db.CreateProject(t.Context(), "example-workspace")
			require.NoError(t, err)
			issue, _, err := d.db.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: project.ID, Title: "claim through proxy", Author: "setup",
			})
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{DB: d.db, StartedAt: d.now,
				Auth: config.AuthConfig{Proxy: config.ProxyConfig{
					TrustedActorHeader: "X-Kata-Actor", TrustedProxyListeners: []string{"127.0.0.1:27123"},
				}},
			})
			t.Cleanup(func() { _ = server.Close() })
			request := httptest.NewRequest(http.MethodPost,
				"http://127.0.0.1:27123"+issuePathRef(project.ID, issue.ShortID, "actions/claim"),
				strings.NewReader(`{"actor":"forged"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Kata-Actor", tc.header)
			request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
				net.Addr(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123})))
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			require.Equalf(t, tc.wantStatus, response.Code, "response: %s", response.Body.String())
			if tc.wantOwner == "" {
				return
			}
			stored, err := d.db.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.Owner)
			assert.Equal(t, tc.wantOwner, *stored.Owner)
		})
	}
}
