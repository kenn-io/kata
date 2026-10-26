package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

func TestPropertyRecordAgreement(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, signer, pin := dbtest.RecordTeammateRoot(t, store)
		definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
		require.NoError(t, err)
		job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "coordinator", Definition: definition})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}, RootSigning: daemon.NewRootSigning(signer)})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		check := func(tb testing.TB, c dbtest.RecordTeammateCase) {
			for _, kind := range []string{"create", "comment", "cron"} {
				if c.Null && kind != "cron" {
					continue
				}
				body := map[string]any{"actor": "coordinator"}
				path := fmt.Sprintf("/api/v1/projects/%d/issues", project.ID)
				method := http.MethodPost
				switch kind {
				case "create":
					body["title"] = "Example task"
					body["force_new"] = true
					if c.Present {
						body["metadata"] = map[string]any{"teammate": c.Body}
					}
				case "comment":
					issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Comment task", Author: "coordinator"})
					require.NoError(tb, err)
					path = issueURLRef(project.ID, issue.ShortID, "comments")
					body["body"] = "Finding"
					if c.Present {
						body["teammate"] = c.Body
						if c.Null {
							body["teammate"] = nil
						}
					}
				case "cron":
					id, err := uid.New()
					require.NoError(tb, err)
					path = fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, id)
					method = http.MethodPut
					body["job_uid"] = job.UID
					body["definition_event_uid"] = job.DefinitionEventUID
					body["status"] = "running"
					body["summary"] = map[string]any{"version": 1}
					body["expected_revision"] = 0
					if c.Present {
						body["teammate"] = c.Body
						if c.Null {
							body["teammate"] = nil
						}
					}
				}
				raw, err := json.Marshal(body)
				require.NoError(tb, err)
				request := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer test-token")
				request.Header.Set("X-Kata-Teammate", c.Header)
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				require.Equal(tb, 200, response.Code, "%s header=%q present=%v body=%q: %s", kind, c.Header, c.Present, c.Body, response.Body.String())
				var result struct {
					Event  db.Event   `json:"event"`
					Events []db.Event `json:"events"`
				}
				require.NoError(tb, json.Unmarshal(response.Body.Bytes(), &result))
				var event db.Event
				if kind == "cron" {
					require.Len(tb, result.Events, 1)
					event = result.Events[0]
				} else {
					event = result.Event
				}

				dbtest.AssertRecordTeammate(tb, store, project, pin, event, c.Want())
				if kind == "create" {
					saved, err := store.IssueByID(t.Context(), *event.IssueID)
					require.NoError(tb, err)
					var metadata map[string]jsontext.Value
					require.NoError(tb, json.Unmarshal([]byte(saved.Metadata), &metadata))
					if c.Present {
						raw, err := json.Marshal(c.Body)
						require.NoError(tb, err)
						require.Equal(tb, string(raw), string(metadata["teammate"]))
					} else if c.Header == "" {
						require.NotContains(tb, metadata, "teammate")
					}
				}
			}
		}
		for i, c := range dbtest.RecordTeammateCases() {
			name := fmt.Sprintf("matrix_%d_header_%s_present_%v_body_%s", i, c.Header, c.Present, c.Body)
			if c.Header == "" && c.Present && c.Body == "" {
				name = "empty_suppression_minimal"
			}
			t.Run(name, func(t *testing.T) { check(t, c) })
		}
		t.Run("header_only_minimal", func(t *testing.T) { check(t, dbtest.RecordTeammateCase{Header: "0"}) })
		for _, header := range []string{"", "codex"} {
			t.Run("cron_null_"+header, func(t *testing.T) { check(t, dbtest.RecordTeammateCase{Header: header, Present: true, Null: true}) })
		}
		t.Run("maximum_handles", func(t *testing.T) {
			check(t, dbtest.RecordTeammateCase{Header: strings.Repeat("Z", 64), Body: "0" + strings.Repeat("._-", 21), Present: true})
		})
		t.Run("invalid_metadata_teammate", func(t *testing.T) {
			before, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			for _, value := range []any{nil, true, 1, []any{}, map[string]any{}} {
				raw, err := json.Marshal(map[string]any{"actor": "coordinator", "title": "Invalid task", "force_new": true, "metadata": map[string]any{"teammate": value}})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), strings.NewReader(string(raw)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-token")
				req.Header.Set("X-Kata-Teammate", "codex")
				out := httptest.NewRecorder()
				server.Handler().ServeHTTP(out, req)
				require.Equal(t, 400, out.Code, out.Body.String())
			}
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
		hegel.Test(t, func(ht *hegel.T) {
			c := dbtest.DrawRecordTeammate(ht)
			check(ht, c)
			if hegel.Draw(ht, hegel.Booleans()) {
				c.Present = true
				c.Null = true
				check(ht, c)
			}
		}, hegel.WithTestCases(32), hegel.WithDatabase(t.TempDir()))
	})
}

func TestCommentIdempotencyUsesEffectiveTeammate(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, signer, _ := dbtest.RecordTeammateRoot(t, store)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "coordinator"})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}, RootSigning: daemon.NewRootSigning(signer)})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		for _, kind := range []string{"comment", "create", "cron"} {
			t.Run(kind, func(t *testing.T) {
				body := map[string]any{"actor": "coordinator"}
				method := http.MethodPost
				path := issueURLRef(project.ID, issue.ShortID, "comments")
				switch kind {
				case "comment":
					body["body"] = "Finding"
				case "create":
					path = fmt.Sprintf("/api/v1/projects/%d/issues", project.ID)
					body["title"] = "Created task"
					body["force_new"] = true
				case "cron":
					definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
					require.NoError(t, err)
					job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "coordinator", Definition: definition})
					require.NoError(t, err)
					id, err := uid.New()
					require.NoError(t, err)
					path = fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, id)
					method = http.MethodPut
					body["job_uid"] = job.UID
					body["definition_event_uid"] = job.DefinitionEventUID
					body["status"] = "running"
					body["summary"] = map[string]any{"version": 1}
					body["expected_revision"] = 0
				}
				retryKey := kind + "-key"
				send := func(header string) *httptest.ResponseRecorder {
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer test-token")
					req.Header.Set("X-Kata-Teammate", header)
					req.Header.Set("Idempotency-Key", retryKey)
					out := httptest.NewRecorder()
					server.Handler().ServeHTTP(out, req)
					return out
				}
				first := send("codex")
				require.Equal(t, 200, first.Code, first.Body.String())
				if kind == "create" {
					body["metadata"] = map[string]any{"teammate": "codex"}
				} else {
					body["teammate"] = "codex"
				}
				cursor, err := store.MaxEventID(t.Context())
				require.NoError(t, err)
				receipts, err := store.AttributionReceiptsAfter(t.Context(), project.UID, 1, 0, 1000)
				require.NoError(t, err)
				unchanged := func() {
					t.Helper()
					after, err := store.MaxEventID(t.Context())
					require.NoError(t, err)
					require.Equal(t, cursor, after, "retries must not append events")
					afterReceipts, err := store.AttributionReceiptsAfter(t.Context(), project.UID, 1, 0, 1000)
					require.NoError(t, err)
					require.Equal(t, receipts, afterReceipts, "retries must not append or replace receipts")
				}
				replay := send("teammate-1")
				require.Equal(t, 200, replay.Code, replay.Body.String())
				var a, b map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(first.Body.Bytes(), &a))
				require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &b))
				switch kind {
				case "cron":
					require.Equal(t, string(a["run"]), string(b["run"]))
					require.Equal(t, "true", string(b["replayed"]))
				case "comment":
					var original, replayed db.Comment
					require.NoError(t, json.Unmarshal(a["comment"], &original))
					require.NoError(t, json.Unmarshal(b["comment"], &replayed))
					require.NotEmpty(t, original.UID)
					require.Equal(t, original.UID, replayed.UID)
					require.Equal(t, original, replayed)
					require.Equal(t, "false", string(b["changed"]))
				default:
					require.Equal(t, string(a["issue"]), string(b["issue"]))
					require.Equal(t, "false", string(b["changed"]))
				}
				unchanged()
				if kind == "create" {
					delete(body, "metadata")
				} else {
					delete(body, "teammate")
				}
				conflict := send("teammate-1")
				require.Equal(t, 409, conflict.Code, conflict.Body.String())
				unchanged()
				if kind != "create" {
					if kind == "cron" {
						id, err := uid.New()
						require.NoError(t, err)
						path = path[:strings.LastIndex(path, "/")+1] + id
					}
					retryKey = kind + "-suppressed-key"
					body["teammate"] = ""
					suppressed := send("codex")
					require.Equal(t, 200, suppressed.Code, suppressed.Body.String())
					if kind == "cron" {
						body["teammate"] = nil
					}
					again := send("teammate-1")
					require.Equal(t, 200, again.Code, again.Body.String())
					delete(body, "teammate")
					changed := send("teammate-1")
					require.Equal(t, 409, changed.Code, changed.Body.String())
				}
			})
		}
	})
}

// Header attribution must not narrow cross-issue reply target lookup.
func TestHeaderOnlyTeammateTypedReply(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		source, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Reply task", Author: "coordinator"})
		require.NoError(t, err)
		targetIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Finding task", Author: "finder"})
		require.NoError(t, err)
		target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: targetIssue.ID, Author: "finder", Teammate: "reviewer", Body: "Finding"})
		require.NoError(t, err)
		requestIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Request task", Author: "lead"})
		require.NoError(t, err)
		pending, err := json.Marshal(notification.Value{From: "lead", Message: "Please answer this finding", Re: target.UID})
		require.NoError(t, err)
		_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: requestIssue.ID, Actor: "lead", Patch: map[string]jsontext.Value{notification.MetadataKey("coordinator"): pending}})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		raw, err := json.Marshal(map[string]any{"actor": "coordinator", "body": "Answer", "reply_to": targetIssue.ShortID + ":" + strings.ToLower(target.UID[20:]), "kind": "reply"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, issueURLRef(project.ID, source.ShortID, "comments"), strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Kata-Teammate", "codex")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, req)
		require.Equal(t, 200, out.Code, out.Body.String())
		comments, err := store.CommentsByIssue(t.Context(), source.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, target.UID, comments[0].ReplyToUID)
		require.Equal(t, "codex", comments[0].Teammate)

		current, err := store.IssueByID(t.Context(), requestIssue.ID)
		require.NoError(t, err)
		var slots map[string]notification.Value
		require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
		require.NotContains(t, slots, notification.MetadataKey("coordinator"), "header attribution must preserve actor-only request settlement")
		answer, exists := slots[notification.MetadataKey("lead")]
		require.True(t, exists, "requester learns the answer where they asked")
		require.Equal(t, "coordinator", answer.From)
		require.Empty(t, answer.Teammate)
		require.Equal(t, "reply", answer.Kind)
		require.Equal(t, comments[0].UID, answer.Re)

		_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: requestIssue.ID, Actor: "lead", Patch: map[string]jsontext.Value{notification.MetadataKey("coordinator/builder"): pending}})
		require.NoError(t, err)
		raw, err = json.Marshal(map[string]any{"actor": "coordinator", "teammate": "builder", "body": "Explicit answer", "reply_to": targetIssue.ShortID + ":" + strings.ToLower(target.UID[20:]), "kind": "reply"})
		require.NoError(t, err)
		req = httptest.NewRequest(http.MethodPost, issueURLRef(project.ID, source.ShortID, "comments"), strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Kata-Teammate", "codex")
		out = httptest.NewRecorder()
		server.Handler().ServeHTTP(out, req)
		require.Equal(t, 200, out.Code, out.Body.String())
		current, err = store.IssueByID(t.Context(), requestIssue.ID)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
		require.NotContains(t, slots, notification.MetadataKey("coordinator/builder"))
		require.Equal(t, "builder", slots[notification.MetadataKey("lead")].Teammate)
		comments, err = store.CommentsByIssue(t.Context(), source.ID)
		require.NoError(t, err)
		require.Len(t, comments, 2)
		require.Equal(t, target.UID, comments[1].ReplyToUID)
		require.Equal(t, "builder", comments[1].Teammate)
	})
}
