package daemon_test

import (
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

func movedAdoptionResetFixture(t *testing.T, opts ...testenv.Option) (*testenv.Env, db.Project, db.Project, int64) {
	t.Helper()
	env := testenv.New(t, opts...)
	source, err := env.DB.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Departed issue", Author: "worker"})
	require.NoError(t, err)
	cursor, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	return env, source, target, cursor
}

func adoptMovedProjectHTTP(t *testing.T, env *testenv.Env, target db.Project, status int) {
	t.Helper()
	response, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url": "http://127.0.0.1:7373", "hub_project_id": 42,
		"hub_project_uid": "01HZNQ7VFPK1XGD8R5MABCD4EX", "project_name": target.Name,
		"replay_horizon_event_id": 1,
		"actor":                   "worker", "token": "push-token", "capabilities": "pull,push",
		"push_enabled": true, "adopt_existing": true,
	}, nil)
	require.Equal(t, status, response.StatusCode, string(raw))
}

func TestMovedIssueAdoptionResetsFormerProjectPollAndResume(t *testing.T) {
	for _, kind := range []string{"poll", "resume"} {
		t.Run(kind, func(t *testing.T) {
			env, source, target, beforeMove := movedAdoptionResetFixture(t)
			adoptMovedProjectHTTP(t, env, target, http.StatusOK)
			if kind == "poll" {
				response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", source.ID, beforeMove), nil, nil)
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				var body struct {
					ResetRequired bool  `json:"reset_required"`
					ResetAfterID  int64 `json:"reset_after_id"`
				}
				require.NoError(t, json.Unmarshal(raw, &body))
				require.True(t, body.ResetRequired, string(raw))
				require.Greater(t, body.ResetAfterID, beforeMove)
				return
			}
			response := openSSE(t, env, fmt.Sprintf("project_id=%d", source.ID), http.Header{"Last-Event-Id": {strconv.FormatInt(beforeMove, 10)}})
			defer func() { _ = response.Body.Close() }()
			frame, ok := newSSEFramer(response.Body).Next(t, 2*time.Second)
			require.True(t, ok, "source reconnect must reset after its departure disappears")
			require.Equal(t, "sync.reset_required", frame.event)
		})
	}
}

func TestMovedIssueAdoptionWakesLiveFormerProjectBeforeCredentialFailure(t *testing.T) {
	for _, failCredentials := range []bool{false, true} {
		t.Run(fmt.Sprintf("credential-fails-%t", failCredentials), func(t *testing.T) {
			credentials := newReplicaCredentialStore()
			if failCredentials {
				credentials.storeErr = errors.New("credential persistence unavailable")
			}
			env, source, target, _ := movedAdoptionResetFixture(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
			_, sentinel, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Stream ready", Author: "worker"})
			require.NoError(t, err)
			response := openSSE(t, env, fmt.Sprintf("project_id=%d&after_id=%d", source.ID, sentinel.ID-1), nil)
			defer func() { _ = response.Body.Close() }()
			framer := newSSEFramer(response.Body)
			first, ok := framer.Next(t, 2*time.Second)
			require.True(t, ok)
			require.Equal(t, strconv.FormatInt(sentinel.ID, 10), first.id)
			status := http.StatusOK
			if failCredentials {
				status = http.StatusInternalServerError
			}
			adoptMovedProjectHTTP(t, env, target, status)
			frame, ok := framer.Next(t, 2*time.Second)
			require.True(t, ok, "committed adoption must reset the source before credential persistence can fail")
			require.Equal(t, "sync.reset_required", frame.event)
		})
	}
}
