package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func movedPurgeResetFixture(t *testing.T, opts ...testenv.Option) (*testenv.Env, []db.Project, db.Issue, int64) {
	t.Helper()
	env := testenv.New(t, opts...)
	projects := make([]db.Project, 4)
	for i, name := range []string{"reset-source", "reset-middle", "reset-target", "reset-unrelated"} {
		var err error
		projects[i], err = env.DB.CreateProject(t.Context(), name)
		require.NoError(t, err)
	}
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Cached issue before departure", Author: "worker"})
	require.NoError(t, err)
	cursor, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	for i := range 2 {
		moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[i].ID, ToProjectID: projects[i+1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		issue = moved.Issue
	}
	return env, projects, issue, cursor
}

func TestMovedIssuePurgeResetsFormerProjectPollAndResume(t *testing.T) {
	for _, kind := range []string{"poll", "resume"} {
		t.Run(kind, func(t *testing.T) {
			env, projects, issue, beforeMove := movedPurgeResetFixture(t, testenv.WithAuthToken("bootstrap-example-token"), testenv.WithRequireTokenIdentity())
			_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "source-viewer-token", Actor: "viewer", AdminActor: "admin"})
			require.NoError(t, err)
			team, _, err := env.DB.CreateTeam(t.Context(), "target-readers", "admin")
			require.NoError(t, err)
			_, err = env.DB.SetTeamMembership(t.Context(), team.UID, "other-viewer", true, "admin")
			require.NoError(t, err)
			_, _, err = env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: projects[2].UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
			purged, err := env.DB.PurgeIssue(t.Context(), issue.ID, "worker", nil)
			require.NoError(t, err)
			require.NotNil(t, purged.PurgeResetAfterEventID)
			resetID := *purged.PurgeResetAfterEventID
			if kind == "poll" {
				response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", projects[0].ID, beforeMove), nil, bearer("source-viewer-token"))
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				var body struct {
					ResetRequired bool             `json:"reset_required"`
					ResetAfterID  int64            `json:"reset_after_id"`
					NextAfterID   int64            `json:"next_after_id"`
					Events        []jsontext.Value `json:"events"`
				}
				require.NoError(t, json.Unmarshal(raw, &body))
				require.True(t, body.ResetRequired)
				require.Equal(t, resetID, body.ResetAfterID)
				require.Equal(t, resetID, body.NextAfterID)
				require.Empty(t, body.Events)
				require.NotContains(t, string(raw), issue.UID)
				require.NotContains(t, string(raw), projects[2].UID)
				return
			}
			response := openSSE(t, env, fmt.Sprintf("project_id=%d", projects[0].ID), http.Header{"Authorization": {"Bearer source-viewer-token"}, "Last-Event-Id": {strconv.FormatInt(beforeMove, 10)}})
			defer func() { _ = response.Body.Close() }()
			require.Equal(t, http.StatusOK, response.StatusCode)
			frame, ok := newSSEFramer(response.Body).Next(t, 2*time.Second)
			require.True(t, ok, "old source reconnect requires a durable reset")
			require.Equal(t, "sync.reset_required", frame.event)
			require.Equal(t, strconv.FormatInt(resetID, 10), frame.id)
			require.NotContains(t, frame.data, issue.UID)
			require.NotContains(t, frame.data, projects[2].UID)
			require.JSONEq(t, fmt.Sprintf(`{"event_id":%d,"reset_after_id":%d}`, resetID, resetID), frame.data)
		})
	}
}

type purgeBaselineFailureStore struct{ db.Storage }

func (s purgeBaselineFailureStore) RefreshProjectFederationBaseline(context.Context, int64, string) (db.FederationBinding, bool, error) {
	return db.FederationBinding{}, false, errors.New("baseline unavailable")
}

func TestMovedIssuePurgePublishesEveryAffectedProjectReset(t *testing.T) {
	for _, baselineFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("baseline-fails-%t", baselineFails), func(t *testing.T) {
			var opts []testenv.Option
			if baselineFails {
				opts = append(opts, func(cfg *daemon.ServerConfig) { cfg.DB = purgeBaselineFailureStore{cfg.DB} })
			}
			env, projects, issue, _ := movedPurgeResetFixture(t, opts...)
			subscriptions := make([]daemon.Subscription, len(projects))
			for i, project := range projects {
				subscriptions[i] = env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
				defer subscriptions[i].Unsub()
			}
			response, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/purge", projects[2].ID, issue.UID), map[string]string{"actor": "worker"}, map[string]string{"X-Kata-Confirm": "PURGE " + projects[2].Name + "#" + issue.ShortID})
			status := http.StatusOK
			if baselineFails {
				status = http.StatusInternalServerError
			}
			require.Equal(t, status, response.StatusCode, string(raw))
			_, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound, "purge committed before the response-only baseline refresh")
			for i, subscription := range subscriptions[:3] {
				select {
				case message := <-subscription.Ch:
					require.Equal(t, daemon.StreamKindReset, message.Kind)
					require.Equal(t, projects[i].ID, message.ProjectID)
					require.Positive(t, message.ResetID)
				case <-time.After(time.Second):
					t.Errorf("project %s did not receive its committed purge reset", projects[i].Name)
				}
			}
			select {
			case message := <-subscriptions[3].Ch:
				t.Fatalf("unrelated project received purge reset: %+v", message)
			default:
			}
		})
	}
}

func TestMovedIssuePurgeWakesLiveFormerProjectStream(t *testing.T) {
	env, projects, issue, _ := movedPurgeResetFixture(t)
	// Consume a retained source event so the stream has completed its drain.
	_, sentinel, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Live source sentinel", Author: "worker"})
	require.NoError(t, err)
	response := openSSE(t, env, fmt.Sprintf("project_id=%d&after_id=%d", projects[0].ID, sentinel.ID-1), nil)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	framer := newSSEFramer(response.Body)
	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok)
	require.Equal(t, strconv.FormatInt(sentinel.ID, 10), first.id)
	purgeResponse, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/purge", projects[2].ID, issue.UID), map[string]string{"actor": "worker"}, map[string]string{"X-Kata-Confirm": "PURGE " + projects[2].Name + "#" + issue.ShortID})
	require.Equal(t, http.StatusOK, purgeResponse.StatusCode, string(raw))
	reset, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "the former project must wake immediately, before a heartbeat")
	require.Equal(t, "sync.reset_required", reset.event)
	require.NotContains(t, reset.data, issue.UID)
}
