package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	Summary struct {
		Moved     int `json:"moved"`
		Unchanged int `json:"unchanged"`
	} `json:"summary"`
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

func TestMoveIssuesBatch_HTTPRejectsMoreThan50(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	source, target, issue := seedMovePair(t, env)
	items := make([]map[string]string, 51)
	for i := range items {
		items[i] = map[string]string{"ref": issue.UID}
	}
	body, err := json.Marshal(map[string]any{"actor": "tester", "to_project_uid": target.UID, "items": items})
	require.NoError(t, err)
	resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", string(body), "")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func TestMoveIssuesBatch_RejectsSubtreeAndDryRun(t *testing.T) {
	for _, field := range []string{"subtree", "dry_run"} {
		t.Run(field, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, target, issue := seedMovePair(t, env)
			body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"%s":true,"items":[{"ref":%q}]}`, target.UID, field, issue.UID)
			resp := doPostWithIfMatch(t, env, env.URL+"/api/v1/issues/actions/move", body, "")
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, source.ID, got.ProjectID)
		})
	}
}

func TestMoveIssuesBatch_DuplicateDifferentTargets(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprint(reversed), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, firstTarget, issue := seedMovePair(t, env)
			secondTarget, err := env.DB.CreateProject(t.Context(), "other-target")
			require.NoError(t, err)
			if reversed {
				firstTarget, secondTarget = secondTarget, firstTarget
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			body := fmt.Sprintf(`{"actor":"tester","comment":"routed","items":[{"ref":%q,"to_project_uid":%q},{"ref":%q,"to_project_uid":%q}]}`, issue.UID, firstTarget.UID, source.Name+"#"+issue.ShortID, secondTarget.UID)
			out := postMoveBatch(t, env, body)
			require.Len(t, out.Results, 2)
			require.Equal(t, "moved", out.Results[0].Result)
			require.Equal(t, "refused", out.Results[1].Result)
			require.Equal(t, "duplicate_conflicting_target", out.Results[1].Code)
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, firstTarget.ID, got.ProjectID)
			comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: firstTarget.ID, AfterID: before, Limit: 100})
			require.NoError(t, err)
			moves := 0
			for _, event := range events {
				if event.Type == "issue.moved" {
					moves++
				}
			}
			require.Equal(t, 1, moves)
		})
	}
	t.Run("same target", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken("tok"))
		_, target, issue := seedMovePair(t, env)
		body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"items":[{"ref":%q},{"ref":%q}]}`, target.UID, issue.UID, issue.UID)
		out := postMoveBatch(t, env, body)
		require.Len(t, out.Results, 2)
		require.Equal(t, "moved", out.Results[0].Result)
		require.Equal(t, "unchanged", out.Results[1].Result)
		require.Equal(t, "duplicate_item", out.Results[1].Code)
		require.Equal(t, 1, out.Summary.Moved)
		require.Equal(t, 1, out.Summary.Unchanged)
	})
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
				body = fmt.Sprintf(`{"actor":"tester","comment":"routed","items":[{"ref":"","to_project_uid":%q},{"ref":%q,"to_project_uid":%q}]}`, target.UID, issue.UID, target.UID)
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

func TestMoveIssuesBatch_InvalidJSONItemDoesNotBlockNeighbor(t *testing.T) {
	for _, invalid := range []string{`{"ref":42}`, `{"ref":true}`, `{"ref":"abc4","to_project_uid":42}`, `{"ref":"abc4","expected_revision":"1"}`, `{"ref":"$issue","expected_revision":null}`, `42`} {
		t.Run(invalid, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			_, target, issue := seedMovePair(t, env)
			body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"items":[%s,{"ref":%q}]}`, target.UID, strings.ReplaceAll(invalid, "$issue", issue.UID), issue.UID)
			out := postMoveBatch(t, env, body)
			require.Len(t, out.Results, 2)
			require.Equal(t, "refused", out.Results[0].Result)
			require.Equal(t, "validation", out.Results[0].Code)
			require.Equal(t, "moved", out.Results[1].Result)
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
			body := fmt.Sprintf(`{"actor":"tester",%s,"items":[{"ref":%q,"to_project_uid":%q}]}`, option, issue.UID, target.UID)
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
