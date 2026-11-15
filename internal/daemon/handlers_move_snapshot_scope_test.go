package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMoveSnapshotHiddenLinkResetsScopedEventPoll(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-token"))
	source, target, issue := seedMovePair(t, env)
	hidden, err := env.DB.CreateProject(t.Context(), "hidden-link-project")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target, hidden} {
		_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "worker")
		require.NoError(t, err)
	}
	peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: hidden.ID, Title: "restricted link endpoint", Author: "worker"})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "worker"})
	require.NoError(t, err)
	moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	headers := bearer("owner-token")
	headers[requestProjectScopeHeader] = requestScopeValue(t, source.UID, target.UID)
	response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/events?after_id=%d", moved.EventID-1), nil, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	var result api.PollEventsBody
	require.NoError(t, json.Unmarshal(raw, &result))
	require.True(t, result.ResetRequired, "move snapshot links must respect the explicit project scope")
	require.Equal(t, moved.EventID, result.ResetAfterID)
	require.Empty(t, result.Events)
	for _, identity := range []string{peer.UID, peer.ShortID, hidden.UID, issue.UID} {
		require.NotContains(t, string(raw), identity)
	}
}
