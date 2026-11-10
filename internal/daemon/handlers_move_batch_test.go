package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type batchReply struct {
	Results []struct {
		Index      int    `json:"index"`
		Ref        string `json:"ref"`
		Result     string `json:"result"`
		Code       string `json:"code"`
		EventID    int64  `json:"event_id"`
		NewShortID string `json:"new_short_id"`
		To         string `json:"to"`
	} `json:"results"`
	NextIndex int `json:"next_index"`
}

func postMoveBatch(t testing.TB, env *testenv.Env, body string) batchReply {
	t.Helper()
	resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", body, "")
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var out batchReply
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}
func TestMoveIssuesBatch_MixedTargetsAndRerun(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, a, first := seedMovePair(t, env)
	b, err := env.DB.CreateProject(t.Context(), "other-target")
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	body := fmt.Sprintf(`{"actor":"tester","comment":"routed","default_project_id":%d,"items":[{"ref":%q,"to_project_uid":%q},{"ref":%q,"to_project_uid":%q}]}`, src.ID, src.Name+"#"+first.ShortID, a.UID, second.UID, b.UID)
	out := postMoveBatch(t, env, body)
	require.Equal(t, 2, out.NextIndex)
	require.Len(t, out.Results, 2)
	for _, r := range out.Results {
		require.Equal(t, "moved", r.Result)
		require.NotZero(t, r.EventID)
	}
	again := postMoveBatch(t, env, body)
	for _, r := range again.Results {
		require.Equal(t, "unchanged", r.Result)
		require.Equal(t, "already_in_target", r.Code)
	}
	for _, i := range []db.Issue{first, second} {
		comments, err := env.DB.CommentsByIssue(t.Context(), i.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
	}
}
func TestMoveIssuesBatch_DuplicateConflictDryRun(t *testing.T) {
	for _, mode := range []string{"duplicate", "conflict", "dry"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			src, a, i := seedMovePair(t, env)
			b, err := env.DB.CreateProject(t.Context(), "other-target")
			require.NoError(t, err)
			target := a.UID
			if mode == "conflict" {
				target = b.UID
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			body := fmt.Sprintf(`{"actor":"tester","dry_run":%t,"default_project_id":%d,"items":[{"ref":%q,"to_project_uid":%q},{"ref":%q,"to_project_uid":%q}]}`, mode == "dry", src.ID, i.UID, a.UID, i.UID, target)
			out := postMoveBatch(t, env, body)
			require.Len(t, out.Results, 2)
			if mode == "conflict" {
				for _, r := range out.Results {
					require.Equal(t, "refused", r.Result)
					require.Equal(t, "conflicting_target", r.Code)
				}
			} else {
				require.Equal(t, "duplicate_item", out.Results[1].Code)
				if mode == "dry" {
					require.Equal(t, "would_move", out.Results[0].Result)
				} else {
					require.Equal(t, "moved", out.Results[0].Result)
				}
			}
			if mode != "duplicate" {
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
		})
	}
}

func TestMoveIssuesBatch_RejectMultipleSubtreeInputs(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, target, root := seedMovePair(t, env)
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "child", Author: "tester", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	for _, dry := range []bool{false, true} {
		body := fmt.Sprintf(`{"actor":"tester","dry_run":%t,"subtree":true,"to_project_uid":%q,"items":[{"ref":%q},{"ref":%q}]}`, dry, target.UID, root.UID, child.UID)
		resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", body, "")
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	for _, issue := range []db.Issue{root, child} {
		got, err := env.DB.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, src.ID, got.ProjectID)
	}
}
