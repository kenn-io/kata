package daemon_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func movedForceReleaseFixture(t *testing.T, sourceFederated bool) (*testenv.Env, db.Project, db.Project, db.Issue) {
	t.Helper()
	env := testenv.New(t, testenv.WithAuthToken("owner-token"))
	source, target, issue := seedMovePair(t, env)
	_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	projects := []db.Project{target}
	if sourceFederated {
		projects = append(projects, source)
	}
	for _, project := range projects {
		_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "worker")
		require.NoError(t, err)
	}
	_, err = env.DB.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: target.ID, IssueRef: issue.UID, Principal: claimGatePrincipal(env, "other-holder"), ClaimKind: "hard", Now: time.Now()})
	require.NoError(t, err)
	return env, source, target, issue
}

func TestMovedIssueForceReleaseUsesCurrentHub(t *testing.T) {
	for _, sourceFederated := range []bool{false, true} {
		for _, refKind := range []string{"alias", "ULID"} {
			name := "local source/" + refKind
			if sourceFederated {
				name = "hub source/" + refKind
			}
			t.Run(name, func(t *testing.T) {
				env, source, target, issue := movedForceReleaseFixture(t, sourceFederated)
				subscription := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: target.ID})
				defer subscription.Unsub()
				ref := issue.ShortID
				if refKind == "ULID" {
					ref = issue.UID
				}
				var result claimResponseBody
				response := claimPost(t, env, source.ID, ref, "force_release", map[string]any{"actor": "administrator", "reason": "handoff"}, bearer("owner-token"), &result)
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.True(t, result.Granted)
				require.NotNil(t, result.Claim)
				require.Equal(t, issue.UID, result.Claim.IssueUID)
				require.NotNil(t, result.Claim.ReleasedAt)
				require.NotNil(t, result.Claim.ReleaseReason)
				require.Equal(t, "handoff", *result.Claim.ReleaseReason)
				message := receiveMsg(t, subscription.Ch, time.Second, "current-project force-release event")
				require.NotNil(t, message.Event)
				require.Equal(t, "claim.force_released", message.Event.Type)
				require.Equal(t, target.ID, message.Event.ProjectID)
				status, err := env.DB.ClaimStatusReadOnly(t.Context(), target.ID, issue.UID, time.Now())
				require.NoError(t, err)
				require.False(t, status.Held)
			})
		}
	}
}

func TestMovedIssueLeaseStealThroughHistoricalRef(t *testing.T) {
	env, source, target, issue := movedForceReleaseFixture(t, false)
	response := claimPost(t, env, source.ID, issue.ShortID, "force_release", map[string]any{"actor": "new-holder", "reason": "handoff"}, bearer("owner-token"), nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result claimResponseBody
	response = claimPost(t, env, source.ID, issue.ShortID, "claim", map[string]any{"holder": "new-holder", "claim_kind": "hard"}, bearer("owner-token"), &result)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.True(t, result.Granted)
	status, err := env.DB.ClaimStatusReadOnly(t.Context(), target.ID, issue.UID, time.Now())
	require.NoError(t, err)
	require.True(t, status.Held)
	require.Equal(t, "new-holder", status.Holder.Holder)
}

func TestMovedIssueForceReleaseRecoversSoftDeletedHardLease(t *testing.T) {
	env, source, target, issue := movedForceReleaseFixture(t, false)
	headers := bearer("owner-token")
	headers["X-Kata-Confirm"] = "DELETE " + target.Name + "#" + issue.ShortID
	response, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(target.ID, issue.UID, "actions/delete"),
		map[string]string{"actor": "other-holder"}, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))

	restorePath := issuePathRef(target.ID, issue.UID, "actions/restore")
	response, raw = envDoRaw(t, env, http.MethodPost, restorePath,
		map[string]string{"actor": "new-holder"}, bearer("owner-token"))
	assertAPIError(t, response.StatusCode, raw, http.StatusConflict, "claim_denied")

	response, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(source.ID, issue.ShortID, "force_release"),
		map[string]string{"actor": "administrator", "reason": "unavailable holder recovery"}, bearer("owner-token"))
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	response, raw = envDoRaw(t, env, http.MethodPost, restorePath,
		map[string]string{"actor": "new-holder"}, bearer("owner-token"))
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	restored, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, restored.ProjectID)
	require.Nil(t, restored.DeletedAt)
}
