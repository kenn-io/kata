package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMoveBatchMappingPreviewMatchesWrite(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, boundary := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/boundary=%t", reverse, boundary), func(t *testing.T) {
				env := testenv.New(t, testenv.WithAuthToken("tok"))
				_, target, first := seedMovePair(t, env)
				secondSource, err := env.DB.CreateProject(t.Context(), "second-source")
				require.NoError(t, err)
				if reverse {
					first, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "resident", Author: "tester"})
					require.NoError(t, err)
				}
				second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: secondSource.ID, Title: "second", Author: "tester"})
				require.NoError(t, err)
				for _, issue := range []db.Issue{first, second} {
					_, err = env.DB.UpsertImportMapping(t.Context(), db.ImportMappingParams{Source: "connector", ExternalID: "same", ObjectType: "issue", ProjectID: issue.ProjectID, IssueID: &issue.ID})
					require.NoError(t, err)
				}
				firstTarget := target
				if reverse {
					firstTarget, err = env.DB.CreateProject(t.Context(), "outgoing-target")
					require.NoError(t, err)
				}
				items := []map[string]any{{"ref": first.UID, "to_project_uid": firstTarget.UID}}
				if boundary {
					for range 49 {
						items = append(items, map[string]any{"ref": ""})
					}
				}
				items = append(items, map[string]any{"ref": second.UID, "to_project_uid": target.UID})
				request := map[string]any{"actor": "tester", "comment": "routed", "dry_run": true, "items": items}
				post := func() batchReply {
					raw, err := json.Marshal(request)
					require.NoError(t, err)
					return postMoveBatch(t, env, string(raw))
				}
				eventID, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				accessRevision, err := env.DB.ProjectAccessRevision(t.Context())
				require.NoError(t, err)
				sub := env.Broadcaster.Subscribe(daemon.SubFilter{})
				defer sub.Unsub()
				preview := post()
				require.Equal(t, len(items), preview.NextIndex)
				require.Zero(t, preview.Results[0].EventID)
				require.Empty(t, preview.Results[0].NewShortID)
				for _, issue := range []db.Issue{first, second} {
					got, err := env.DB.IssueByID(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Equal(t, issue.ProjectID, got.ProjectID)
					comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				}
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, eventID, after)
				revision, err := env.DB.ProjectAccessRevision(t.Context())
				require.NoError(t, err)
				require.Equal(t, accessRevision, revision)
				select {
				case <-sub.Ch:
					t.Fatal("preview published an event")
				default:
				}
				request["dry_run"] = false
				written := post()
				last := len(items) - 1
				require.Equal(t, "would_move", preview.Results[0].Result)
				require.Equal(t, "moved", written.Results[0].Result)
				if reverse {
					require.Equal(t, "would_move", preview.Results[last].Result)
					require.Equal(t, "moved", written.Results[last].Result)
				} else {
					require.Equal(t, "refused", preview.Results[last].Result)
					require.Equal(t, "issue_move_import_mapping_collision", preview.Results[last].Code)
					require.Equal(t, preview.Results[last].Code, written.Results[last].Code)
				}
			})
		}
	}
}

func TestMoveBatchDuplicatePreviewMatchesWrite(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		target, err := store.CreateProject(t.Context(), "duplicate-target")
		require.NoError(t, err)
		items := []map[string]any{{"ref": f.visible.UID}, {"ref": f.visible.UID}}
		body := map[string]any{"to_project_uid": target.UID, "items": items, "dry_run": true, "comment": "routed"}
		status, _, raw := f.request(t, http.MethodPost, "/api/v1/issues/actions/move", "nonmember", body, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview batchReply
		require.NoError(t, json.Unmarshal(raw, &preview))
		body["dry_run"] = false
		status, _, raw = f.request(t, http.MethodPost, "/api/v1/issues/actions/move", "nonmember", body, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var written batchReply
		require.NoError(t, json.Unmarshal(raw, &written))
		require.Equal(t, "would_move", preview.Results[0].Result)
		require.Equal(t, "moved", written.Results[0].Result)
		require.Equal(t, "unchanged", preview.Results[1].Result)
		require.Equal(t, preview.Results[1].Result, written.Results[1].Result)
		require.Equal(t, "duplicate_item", preview.Results[1].Code)
		require.Equal(t, preview.Results[1].Code, written.Results[1].Code)
		comments, err := store.CommentsByIssue(t.Context(), f.visible.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
	})
}
