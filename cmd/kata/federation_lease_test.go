package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestClaim_DefaultsToHardClaimPostsActor(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "claim target")

	out := runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)
	assert.Equal(t, "acquired lease on "+ref+" as alice", out)

	status := fetchClaimStatus(t, env, pid, ref)
	require.NotNil(t, status.Claim)
	assert.True(t, status.Held)
	assert.Equal(t, "alice", status.Holder.Holder)
	assert.Equal(t, "alice", status.Claim.Holder)
	assert.Equal(t, "hard", status.Claim.ClaimKind)
	assert.Nil(t, status.Claim.ExpiresAt)
}

func TestClaim_TTLPostsTimedClaim(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "timed target")

	out := runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref, "--ttl", "30m")
	assert.Equal(t, "acquired lease on "+ref+" as alice", out)

	status := fetchClaimStatus(t, env, pid, ref)
	require.NotNil(t, status.Claim)
	require.NotNil(t, status.Claim.ExpiresAt)
	assert.Equal(t, "timed", status.Claim.ClaimKind)
	left := status.Claim.ExpiresAt.Sub(status.Claim.AcquiredAt)
	assert.GreaterOrEqual(t, left, 29*time.Minute)
	assert.LessOrEqual(t, left, 31*time.Minute)
}

func TestClaim_RejectsBareNumericTTL(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "bad ttl")

	_, err := runCLICapture(t, env, dir, "federation", "lease", "acquire", ref, "--ttl", "30")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duration unit")
}

func TestClaim_RejectsTTLBounds(t *testing.T) {
	for _, ttl := range []string{"30s", "25h", "36028797018964028s"} {
		t.Run(ttl, func(t *testing.T) {
			env, dir, _, ref := setupFederatedHubIssue(t, "bad ttl "+ttl)

			_, err := runCLICapture(t, env, dir, "federation", "lease", "acquire", ref, "--ttl", ttl)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "timed lease TTL must be between 60s and 24h")
		})
	}
}

func TestClaim_RejectsUnsupportedTTLUnitsAndFractions(t *testing.T) {
	for _, ttl := range []string{"60000000000ns", "60000ms", "60.5s"} {
		t.Run(ttl, func(t *testing.T) {
			env, dir, _, ref := setupFederatedHubIssue(t, "bad ttl "+ttl)

			_, err := runCLICapture(t, env, dir, "federation", "lease", "acquire", ref, "--ttl", ttl)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ttl must be a whole number followed by s, m, or h")
		})
	}
}

func TestRelease_PostsRelease(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "release target")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	out := runCLIAs(t, env, dir, "alice", "federation", "lease", "release", ref)
	assert.Equal(t, "released lease on "+ref, out)

	status := fetchClaimStatus(t, env, pid, ref)
	assert.False(t, status.Held)
	assert.Nil(t, status.Claim)
}

func TestClaimForceRelease_PostsAdminForceRelease(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "force release target")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	out := runCLIAs(t, env, dir, "admin", "federation", "lease", "force-release", ref, "--reason", "stale holder")
	assert.Equal(t, "force-released lease on "+ref+" from alice", out)

	status := fetchClaimStatus(t, env, pid, ref)
	assert.False(t, status.Held)
	assert.Nil(t, status.Claim)
}

func TestClaimForceRelease_RequiresExplicitActorAndReason(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "force release validation")

	_, err := runCLICapture(t, env, dir, "federation", "lease", "force-release", ref, "--reason", "stale holder")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as is required")

	_, err = runCLICapture(t, env, dir, "--as", "admin", "federation", "lease", "force-release", ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--reason is required")
}

func TestClaimForceRelease_EnrollmentBearerCannotForceRelease(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	dir := t.TempDir()
	project, err := env.DB.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	require.NoError(t, config.WriteProjectConfig(dir, project.Name))
	enableHubClaims(t, env, project.ID)
	issue, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "force release auth",
		Author:    "tester",
	})
	require.NoError(t, err)
	ref := issue.ShortID
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	created, err := env.DB.CreateFederationEnrollment(context.Background(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "enrollment-token",
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4AB",
		ProjectID:        &project.ID,
		Capabilities:     "claim",
	})
	require.NoError(t, err)
	t.Setenv("KATA_AUTH_TOKEN", created.Token)

	_, err = runCLICapture(t, env, dir, "--as", "admin", "federation", "lease", "force-release", ref, "--reason", "stale holder")
	require.Error(t, err)
	var cli *cliError
	require.True(t, errors.As(err, &cli))
	assert.Equal(t, ExitInternal, cli.ExitCode)
	assert.Contains(t, strings.ToLower(cli.Error()), "token")
}

func TestClaimSteal_ReleasesExistingClaimThenClaimsAsActor(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "steal target")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	out := runCLIAs(t, env, dir, "bob", "federation", "lease", "steal", ref, "--reason", "handoff")
	assert.Equal(t, "stole lease on "+ref+" from alice as bob", out)

	status := fetchClaimStatus(t, env, pid, ref)
	require.NotNil(t, status.Claim)
	assert.True(t, status.Held)
	assert.Equal(t, "bob", status.Holder.Holder)
	assert.Equal(t, "bob", status.Claim.Holder)
}

func TestClaimSteal_JSONIncludesReleasedAndNewHolders(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "json steal target")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	out := runCLIAs(t, env, dir, "bob", "--json", "federation", "lease", "steal", ref, "--reason", "handoff")

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	assert.Equal(t, float64(1), body["kata_api_version"])
	assert.Equal(t, "alice", body["released_holder"])
	assert.Equal(t, "bob", body["new_holder"])
	released, ok := body["released"].(map[string]any)
	require.True(t, ok)
	releasedClaim, ok := released["claim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", releasedClaim["holder"])
	claimed, ok := body["claimed"].(map[string]any)
	require.True(t, ok)
	claimedClaim, ok := claimed["claim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "bob", claimedClaim["holder"])
}

func TestClaimSteal_JSONPartialSuccessIncludesReleasedClaim(t *testing.T) {
	resetFlags(t)
	dir := t.TempDir()
	var forceReleaseCalled bool
	var claimCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 99, "name": "hub"},
			})
		case "/api/v1/projects/99/issues/abcd/lease/actions/force_release":
			forceReleaseCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"granted": true,
				"holder":  map[string]any{"holder": "alice", "client_kind": "cli"},
				"claim":   map[string]any{"holder": "alice", "claim_kind": "hard", "released_at": time.Now().UTC()},
			})
		case "/api/v1/projects/99/issues/abcd/lease/actions/acquire":
			claimCalled = true
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": http.StatusConflict,
				"error":  map[string]any{"code": "claim_denied", "message": "charlie already holds abcd"},
			})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cmd := newRootCmd()
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--workspace", dir, "--project", "hub", "--as", "bob", "--json", "federation", "lease", "steal", "abcd", "--reason", "handoff"})
	cmd.SetContext(contextWithBaseURL(context.Background(), server.URL))
	err := cmd.Execute()
	require.Error(t, err)
	assert.True(t, forceReleaseCalled)
	assert.True(t, claimCalled)
	assert.Contains(t, err.Error(), "force-release succeeded but lease failed")

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &body))
	assert.Equal(t, true, body["partial_success"])
	assert.Equal(t, "alice", body["released_holder"])
	released, ok := body["released"].(map[string]any)
	require.True(t, ok)
	releasedClaim, ok := released["claim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", releasedClaim["holder"])
}

func TestClaimSteal_JSONPartialSuccessWhenSecondClaimDeniedWithOK(t *testing.T) {
	resetFlags(t)
	dir := t.TempDir()
	var forceReleaseCalled bool
	var claimCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 99, "name": "hub"},
			})
		case "/api/v1/projects/99/issues/abcd/lease/actions/force_release":
			forceReleaseCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"granted": true,
				"holder":  map[string]any{"holder": "alice", "client_kind": "cli"},
				"claim":   map[string]any{"holder": "alice", "claim_kind": "hard", "released_at": time.Now().UTC()},
			})
		case "/api/v1/projects/99/issues/abcd/lease/actions/acquire":
			claimCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"granted": false,
				"pending": false,
				"claim":   map[string]any{"holder": "charlie", "claim_kind": "hard"},
			})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cmd := newRootCmd()
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--workspace", dir, "--project", "hub", "--as", "bob", "--json", "federation", "lease", "steal", "abcd", "--reason", "handoff"})
	cmd.SetContext(contextWithBaseURL(context.Background(), server.URL))
	err := cmd.Execute()
	require.Error(t, err)
	assert.True(t, forceReleaseCalled)
	assert.True(t, claimCalled)
	assert.Contains(t, err.Error(), "force-release succeeded but lease failed")

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &body))
	assert.Equal(t, true, body["partial_success"])
	assert.Equal(t, "alice", body["released_holder"])
	assert.Equal(t, "bob", body["new_holder"])
	claimErr, ok := body["claim_error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "claim_denied", claimErr["code"])
	assert.Contains(t, claimErr["message"], "charlie")
	claimed, ok := body["claimed"].(map[string]any)
	require.True(t, ok)
	claimedClaim, ok := claimed["claim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "charlie", claimedClaim["holder"])
}

func TestClaim_JSONPreservesDaemonResponse(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "json target")

	out := runCLIAs(t, env, dir, "alice", "--json", "federation", "lease", "acquire", ref)

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	assert.Equal(t, float64(1), body["kata_api_version"])
	assert.Equal(t, true, body["granted"])
	holder, ok := body["holder"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", holder["holder"])
	claim, ok := body["claim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", claim["holder"])
	assert.Equal(t, "hard", claim["claim_kind"])
}

func TestClaim_HumanPendingLineConcise(t *testing.T) {
	env, dir, _, ref := setupFederatedSpokeIssue(t, "pending target")

	out := runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)
	assert.Equal(t, "lease pending for "+ref+" as alice", out)
}

func TestClaim_DenialExitsNonZeroWithClaimDenied(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "denied target")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	_, err := runCLICapture(t, env, dir, "--as", "bob", "federation", "lease", "acquire", ref)
	require.Error(t, err)
	var cli *cliError
	require.True(t, errors.As(err, &cli))
	assert.Equal(t, "claim_denied", cli.Code)
	assert.Equal(t, ExitConflict, cli.ExitCode)
	assert.True(t, strings.Contains(cli.Error(), "lease denied") ||
		strings.Contains(cli.Error(), "already leased"))
}

func TestClaim_JSONDenialPreservesDaemonResponse(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "json denied target")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	out, err := runCLICapture(t, env, dir, "--as", "bob", "--json", "federation", "lease", "acquire", ref)
	require.Error(t, err)
	var cli *cliError
	require.True(t, errors.As(err, &cli))
	assert.Equal(t, "claim_denied", cli.Code)
	assert.Equal(t, ExitConflict, cli.ExitCode)

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	assert.Equal(t, float64(1), body["kata_api_version"])
	assert.Equal(t, false, body["granted"])
	holder, ok := body["holder"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", holder["holder"])
	claim, ok := body["claim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", claim["holder"])
	assert.Equal(t, "hard", claim["claim_kind"])
}

func setupFederatedHubIssue(t *testing.T, title string) (*testenv.Env, string, int64, string) {
	t.Helper()
	env, dir, pid := setupCLIWorkspace(t)
	enableHubClaims(t, env, pid)
	ref := createIssue(t, env, pid, title)
	return env, dir, pid, ref
}

func setupFederatedSpokeIssue(t *testing.T, title string) (*testenv.Env, string, int64, string) {
	t.Helper()
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, title)
	enableSpokeClaims(t, env, pid)
	return env, dir, pid, ref
}

func enableHubClaims(t *testing.T, env *testenv.Env, pid int64) {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.ProjectByID(ctx, pid)
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:     pid,
		Role:          db.FederationRoleHub,
		HubProjectUID: project.UID,
		Enabled:       true,
	})
	require.NoError(t, err)
}

func enableSpokeClaims(t *testing.T, env *testenv.Env, pid int64) {
	t.Helper()
	enableSpokeClaimsTo(t, env, pid, "http://127.0.0.1:1", 42)
}

func enableSpokeClaimsTo(t *testing.T, env *testenv.Env, pid int64, hubURL string, hubProjectID int64) {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.ProjectByID(ctx, pid)
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:     pid,
		Role:          db.FederationRoleSpoke,
		HubURL:        hubURL,
		HubProjectID:  hubProjectID,
		HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4AA",
		Enabled:       true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hubURL,
		HubProjectID: hubProjectID,
		Token:        "claim-token",
		Capabilities: "claim",
	}))
}

func fetchClaimStatus(t *testing.T, env *testenv.Env, pid int64, ref string) api.ClaimStatusBody {
	t.Helper()
	return getJSON[api.ClaimStatusBody](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref+"/lease")
}
