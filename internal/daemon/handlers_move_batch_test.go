package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type batchReply struct {
	Results []struct {
		Index           int    `json:"index"`
		Ref             string `json:"ref"`
		Result          string `json:"result"`
		Code            string `json:"code"`
		EventID         int64  `json:"event_id"`
		NewShortID      string `json:"new_short_id"`
		ShortIDExtended bool   `json:"short_id_extended"`
		To              string `json:"to"`
	} `json:"results"`
	NextIndex int `json:"next_index"`
}

func postMoveBatch(t *testing.T, env *testenv.Env, body string) batchReply {
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
func TestMoveIssuesBatch_ExtendedShortIDAtBothProjects(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	source, target, _ := seedMovePair(t, env)
	for _, item := range []struct {
		projectID int64
		uid       string
	}{
		{source.ID, "01HZNQ7VFPK1XGD8R5MABCD4EX"},
		{target.ID, "01HZNQ7VFPK1XGD8R5MABYD4EX"},
	} {
		_, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: item.projectID, UID: item.uid, Title: "collision", Author: "tester"})
		require.NoError(t, err)
	}
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "move", Author: "tester"})
	require.NoError(t, err)
	require.Len(t, issue.ShortID, 5)
	out := postMoveBatch(t, env, fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"items":[{"ref":%q}]}`, target.UID, issue.UID))
	require.Len(t, out.Results, 1)
	require.Equal(t, "moved", out.Results[0].Result)
	require.Len(t, out.Results[0].NewShortID, 5)
	require.True(t, out.Results[0].ShortIDExtended)
}

func TestMoveIssuesBatch_DuplicateConflict(t *testing.T) {
	for _, mode := range []string{"duplicate", "conflict"} {
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
			body := fmt.Sprintf(`{"actor":"tester","default_project_id":%d,"items":[{"ref":%q,"to_project_uid":%q},{"ref":%q,"to_project_uid":%q}]}`, src.ID, i.UID, a.UID, i.UID, target)
			out := postMoveBatch(t, env, body)
			require.Len(t, out.Results, 2)
			if mode == "conflict" {
				for _, r := range out.Results {
					require.Equal(t, "refused", r.Result)
					require.Equal(t, "conflicting_target", r.Code)
				}
			} else {
				require.Equal(t, "duplicate_item", out.Results[1].Code)
				require.Equal(t, "unchanged", out.Results[1].Result)
				require.Equal(t, "moved", out.Results[0].Result)
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
	body := fmt.Sprintf(`{"actor":"tester","subtree":true,"to_project_uid":%q,"items":[{"ref":%q},{"ref":%q}]}`, target.UID, root.UID, child.UID)
	resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", body, "")
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
	require.Contains(t, string(raw), "subtree move accepts one item")
	for _, issue := range []db.Issue{root, child} {
		got, err := env.DB.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, src.ID, got.ProjectID)
	}
}

func TestMoveIssuesBatch_InvalidRowsDoNotConflict(t *testing.T) {
	for _, mode := range []string{"empty", "negative", "stale first", "stale second"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			_, target, issue := seedMovePair(t, env)
			other, err := env.DB.CreateProject(t.Context(), "other-target")
			require.NoError(t, err)
			var body string
			switch mode {
			case "empty":
				body = fmt.Sprintf(`{"actor":"tester","comment":"routed","to_project_uid":%q,"items":[{"ref":""},{"ref":%q}]}`, target.UID, issue.UID)
			case "negative":
				body = fmt.Sprintf(`{"actor":"tester","comment":"routed","items":[{"ref":%q,"to_project_uid":%q,"expected_revision":-1},{"ref":%q,"to_project_uid":%q}]}`, issue.UID, other.UID, issue.UID, target.UID)
			default:
				owner := "changed"
				changed, _, _, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "tester"})
				require.NoError(t, err)
				require.Greater(t, changed.Revision, issue.Revision)
				bad := fmt.Sprintf(`{"ref":%q,"to_project_uid":%q,"expected_revision":%d}`, issue.UID, other.UID, issue.Revision)
				good := fmt.Sprintf(`{"ref":%q,"to_project_uid":%q}`, issue.UID, target.UID)
				if mode == "stale second" {
					good, bad = bad, good
				}
				body = fmt.Sprintf(`{"actor":"tester","comment":"routed","items":[%s,%s]}`, bad, good)
			}
			out := postMoveBatch(t, env, body)
			stale, valid := 0, 1
			if mode == "stale second" {
				stale, valid = 1, 0
			}
			require.Equal(t, "refused", out.Results[stale].Result)
			want := "revision_conflict"
			if mode == "negative" || mode == "empty" {
				want = "validation"
			}
			require.Equal(t, want, out.Results[stale].Code)
			require.Equal(t, "moved", out.Results[valid].Result)
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, got.ProjectID)
		})
	}
}

func TestMoveIssuesBatch_RejectsInvalidOptions(t *testing.T) {
	for _, option := range []string{`"comment":"  \t "`, `"comment":"routed","teammate":"@invalid"`} {
		t.Run(option, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, target, issue := seedMovePair(t, env)
			body := fmt.Sprintf(`{"actor":"tester",%s,"to_project_uid":%q,"items":[{"ref":%q}]}`, option, target.UID, issue.UID)
			resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", body, "")
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, source.ID, got.ProjectID)
			comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Empty(t, comments)
		})
	}
}

func TestMoveIssuesBatch_ContinuationAcknowledgesMove(t *testing.T) {
	for _, mode := range []string{"duplicate", "edited", "empty"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.MoveBatchBudget = time.Nanosecond }) //nolint:kennlint // deadlinetest: Yield after one chunk.
			source, target, issue := seedMovePair(t, env)
			items := []map[string]any{{"ref": source.Name + "#" + issue.ShortID, "expected_revision": issue.Revision}, {"ref": issue.UID, "expected_revision": issue.Revision}}
			request := map[string]any{"actor": "tester", "comment": "routed", "to_project_uid": target.UID, "items": items}
			post := func() (int, struct {
				Results   []api.MoveIssuesResult `json:"results"`
				NextIndex int                    `json:"next_index"`
			}) {
				raw, err := json.Marshal(request)
				require.NoError(t, err)
				resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", string(raw), "")
				defer func() { _ = resp.Body.Close() }()
				var result struct {
					Results   []api.MoveIssuesResult `json:"results"`
					NextIndex int                    `json:"next_index"`
				}
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
				return resp.StatusCode, result
			}
			status, first := post()
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, 1, first.NextIndex)
			require.Equal(t, "moved", first.Results[0].Result)
			request["start_index"] = first.NextIndex
			request["completed"] = []api.MoveIssuesCompleted{{Result: first.Results[0].Result, UID: first.Results[0].UID, Revision: first.Results[0].Revision}}
			if mode == "edited" {
				owner := "changed"
				updated, _, _, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "tester"})
				require.NoError(t, err)
				require.Greater(t, updated.Revision, first.Results[0].Revision)
			}
			if mode == "empty" {
				request["completed"] = []api.MoveIssuesCompleted{}
			}
			status, second := post()
			if mode == "empty" {
				require.Equal(t, http.StatusBadRequest, status)
				return
			}
			require.Equal(t, http.StatusOK, status)
			if mode == "edited" {
				require.Equal(t, "revision_conflict", second.Results[0].Code)
			} else {
				require.Equal(t, "unchanged", second.Results[0].Result)
				require.Equal(t, "duplicate_item", second.Results[0].Code)
			}
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, got.ProjectID)
			comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
		})
	}
}

func TestMoveIssuesBatch_ThreePageDuplicateKeepsOriginalRevision(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.MoveBatchBudget = time.Nanosecond }) //nolint:kennlint // deadlinetest: Yield after each duplicate.
	_, target, issue := seedMovePair(t, env)
	request := api.MoveIssuesRequest{}
	request.Body.Actor, request.Body.ToProjectUID, request.Body.Comment = "tester", target.UID, "routed"
	for page := range 3 {
		if page > 0 {
			for range 49 {
				request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{})
			}
		}
		request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{Ref: issue.UID, ExpectedRevision: issue.Revision})
	}
	for page := range 3 {
		raw, err := json.Marshal(request.Body)
		require.NoError(t, err)
		resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", string(raw), "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out api.MoveIssuesResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out.Body))
		require.NoError(t, resp.Body.Close())
		require.Equal(t, min((page+1)*50, len(request.Body.Items)), out.Body.NextIndex)
		require.NotEmpty(t, out.Body.Results)
		result := out.Body.Results[0]
		if page == 0 {
			require.Equal(t, "moved", result.Result)
		} else {
			require.Equal(t, "duplicate_item", result.Code, "page %d result %q", page+1, result.Result)
			require.Equal(t, "unchanged", result.Result)
		}
		request.Body.StartIndex = out.Body.NextIndex
		for _, completed := range out.Body.Results {
			request.Body.Completed = append(request.Body.Completed, api.MoveIssuesCompleted{Result: completed.Result, Code: completed.Code, UID: completed.UID, Revision: completed.Revision})
		}
	}
	comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestMoveIssuesBatch_ThreePageDuplicateAfterEdit(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.MoveBatchBudget = time.Nanosecond }) //nolint:kennlint // deadlinetest: Yield after each chunk.
	_, target, issue := seedMovePair(t, env)
	request := api.MoveIssuesRequest{}
	request.Body.Actor, request.Body.ToProjectUID = "tester", target.UID
	for page := range 3 {
		if page > 0 {
			for range 49 {
				request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{})
			}
		}
		request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{Ref: issue.UID})
	}
	request.Body.Items[0].ExpectedRevision = issue.Revision
	for page := range 3 {
		raw, err := json.Marshal(request.Body)
		require.NoError(t, err)
		resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", string(raw), "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out api.MoveIssuesResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out.Body))
		require.NoError(t, resp.Body.Close())
		require.Equal(t, min((page+1)*50, len(request.Body.Items)), out.Body.NextIndex)
		result := out.Body.Results[0]
		switch page {
		case 0:
			require.Equal(t, "moved", result.Result)
			request.Body.Items[100].ExpectedRevision = result.Revision
			owner := "changed"
			updated, _, _, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "tester"})
			require.NoError(t, err)
			require.Greater(t, updated.Revision, result.Revision)
		case 1:
			require.Equal(t, "already_in_target", result.Code)
		case 2:
			require.Equal(t, "revision_conflict", result.Code)
		}
		request.Body.StartIndex = out.Body.NextIndex
		for _, completed := range out.Body.Results {
			request.Body.Completed = append(request.Body.Completed, api.MoveIssuesCompleted{Result: completed.Result, Code: completed.Code, UID: completed.UID, Revision: completed.Revision})
		}
	}
}
