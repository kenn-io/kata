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
				require.Equal(t, "unchanged", out.Results[1].Result)
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
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dry=%t", mode, dry), func(t *testing.T) {
				env := testenv.New(t, testenv.WithAuthToken("tok"))
				src, target, issue := seedMovePair(t, env)
				other, err := env.DB.CreateProject(t.Context(), "other-target")
				require.NoError(t, err)
				var body string
				switch mode {
				case "empty":
					body = fmt.Sprintf(`{"actor":"tester","comment":"routed","dry_run":%t,"to_project_uid":%q,"items":[{"ref":""},{"ref":%q}]}`, dry, target.UID, issue.UID)
				case "negative":
					body = fmt.Sprintf(`{"actor":"tester","comment":"routed","dry_run":%t,"items":[{"ref":%q,"to_project_uid":%q,"expected_revision":-1},{"ref":%q,"to_project_uid":%q}]}`, dry, issue.UID, other.UID, issue.UID, target.UID)
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
					body = fmt.Sprintf(`{"actor":"tester","comment":"routed","dry_run":%t,"items":[%s,%s]}`, dry, bad, good)
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
				outcome := "moved"
				projectID := target.ID
				if dry {
					outcome, projectID = "would_move", src.ID
				}
				require.Equal(t, outcome, out.Results[valid].Result)
				got, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, projectID, got.ProjectID)
				if dry {
					comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				}
			})
		}
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
