package daemon_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMovedIssueLeaseRoutesUseCurrentIdentity(t *testing.T) {
	for _, refKind := range []string{"short ID", "ULID"} {
		for _, action := range []string{"claim", "renew", "release", "status"} {
			t.Run(refKind+"/"+action, func(t *testing.T) {
				env := testenv.New(t)
				source, target, issue := moveLocalIssueForLeaseTests(t, env)
				body := map[string]any{"holder": "worker", "client_kind": "cli", "claim_kind": "timed", "ttl_seconds": 300}
				if action != "claim" {
					response := claimPost(t, env, target.ID, issue.UID, "claim", body, nil, nil)
					require.Equal(t, http.StatusOK, response.StatusCode)
				}
				ref := issue.ShortID
				if refKind == "ULID" {
					ref = issue.UID
				}
				if action == "status" {
					response, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(source.ID, ref), nil, nil)
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					var result struct {
						Held bool `json:"held"`
					}
					require.NoError(t, json.Unmarshal(raw, &result))
					require.True(t, result.Held)
				} else {
					var result claimResponseBody
					response := claimPost(t, env, source.ID, ref, action, body, nil, &result)
					require.Equal(t, http.StatusOK, response.StatusCode)
					require.True(t, result.Granted)
				}
			})
		}
	}
}

func TestMovedIssueLeaseRejectsFormerProjectEnrollment(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-token"))
	source, target, issue := moveLocalIssueForLeaseTests(t, env)
	enrollment := createClaimEnrollment(t, env, source.ID, federationTestSpokeUID, "claim")
	for _, action := range []string{"claim", "renew", "release", "status"} {
		t.Run(action, func(t *testing.T) {
			method, path := http.MethodPost, claimActionPath(source.ID, issue.ShortID, action)
			var body any = map[string]any{"holder": "worker", "claim_kind": "hard"}
			if action == "status" {
				method, path, body = http.MethodGet, claimStatusPath(source.ID, issue.ShortID), nil
			}
			response, raw := envDoRaw(t, env, method, path, body, bearer(enrollment.Token))
			require.Equal(t, http.StatusForbidden, response.StatusCode, string(raw))
		})
	}
	status, err := env.DB.ClaimStatusReadOnly(t.Context(), target.ID, issue.UID, time.Now())
	require.NoError(t, err)
	require.False(t, status.Held)
}

func TestMovedIssueLeaseAllowsEnrollmentAuthorizedForBothProjects(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-token"))
	source, _, issue := moveLocalIssueForLeaseTests(t, env)
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "example-enrollment-token", SpokeInstanceUID: federationTestSpokeUID, Capabilities: "claim", Actor: "tester"})
	require.NoError(t, err)
	var result claimResponseBody
	response := claimPost(t, env, source.ID, issue.ShortID, "claim", map[string]any{"holder": "ignored", "claim_kind": "hard"}, bearer(enrollment.Token), &result)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.True(t, result.Granted)
	require.Equal(t, "tester", result.Holder.Holder)
	require.Equal(t, federationTestSpokeUID, result.Holder.HolderInstanceUID)
	status, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(source.ID, issue.ShortID), nil, bearer(enrollment.Token))
	require.Equal(t, http.StatusOK, status.StatusCode, string(raw))
}

// Add federation bindings after the move so lease routing is tested
// independently of support for moving already federated issues.
func moveLocalIssueForLeaseTests(t *testing.T, env *testenv.Env) (db.Project, db.Project, db.Issue) {
	t.Helper()
	source, target, issue := seedMovePair(t, env)
	_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "worker")
		require.NoError(t, err)
	}
	return source, target, issue
}
