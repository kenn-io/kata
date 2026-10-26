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
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/notification"
	"hegel.dev/go/hegel"
)

func TestCommentAttributionKeepsRecipientAddresses(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		check := func(tb testing.TB, scope string, c dbtest.RecordTeammateCase, pending bool) {
			issue, _, err := store.CreateIssue(tb.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review task", Author: "alice"})
			require.NoError(tb, err)
			send := func(actor, header string, body map[string]any) db.Comment {
				body["actor"] = actor
				raw, err := json.Marshal(body)
				require.NoError(tb, err)
				req := httptest.NewRequest(http.MethodPost, issueURLRef(project.ID, issue.ShortID, "comments"), strings.NewReader(string(raw)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-token")
				req.Header.Set("X-Kata-Teammate", header)
				out := httptest.NewRecorder()
				server.Handler().ServeHTTP(out, req)
				require.Equal(tb, 200, out.Code, out.Body.String())
				var result struct {
					Comment db.Comment `json:"comment"`
				}
				require.NoError(tb, json.Unmarshal(out.Body.Bytes(), &result))
				return result.Comment
			}
			header := ""
			body := map[string]any{"body": "Finding"}
			if scope == "target" {
				header = c.Header
				if c.Present {
					body["teammate"] = c.Body
				}
			}
			target := send("alice", header, body)
			actor := "alice"
			if scope == "target" {
				require.Equal(tb, c.Want(), target.Teammate)
			} else {
				actor = "charlie"
				body = map[string]any{"body": "Earlier answer", "reply_to": target.UID, "kind": "reply"}
				if c.Present {
					body["teammate"] = c.Body
				}
				prior := send("charlie", c.Header, body)
				require.Equal(tb, target.UID, prior.ReplyToUID)
				require.Equal(tb, c.Want(), prior.Teammate)
			}
			// Addressing is an independent oracle: only an explicit nonempty body
			// selects a named inbox, regardless of the header-derived record value.
			address := actor
			if c.Present && c.Body != "" {
				address += "/" + c.Body
			}
			if pending {
				raw, err := json.Marshal(notification.Value{From: "lead", Message: "Existing request"})
				require.NoError(tb, err)
				_, err = store.PatchIssueMetadata(tb.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "lead", Patch: map[string]jsontext.Value{notification.MetadataKey(address): raw}})
				require.NoError(tb, err)
			}
			kind := "reply"
			if scope == "prior linker" {
				kind = "confirm"
			}
			answer := send("bob", "", map[string]any{"body": "I verified the complete finding and recorded the reproduction evidence.", "reply_to": target.UID, "kind": kind})
			require.Equal(tb, target.UID, answer.ReplyToUID)
			current, err := store.IssueByID(tb.Context(), issue.ID)
			require.NoError(tb, err)
			var slots map[string]notification.Value
			require.NoError(tb, json.Unmarshal([]byte(current.Metadata), &slots))
			value, exists := slots[notification.MetadataKey(address)]
			require.True(tb, exists, "record attribution must not move recipient inbox: %s", current.Metadata)
			if pending {
				require.Equal(tb, "Existing request", value.Message)
			} else {
				require.Equal(tb, answer.UID, value.Re)
			}
			if c.Header != "" && address != actor+"/"+c.Header {
				require.NotContains(tb, slots, notification.MetadataKey(actor+"/"+c.Header))
			}
			if c.Present && c.Body != "" {
				require.NotContains(tb, slots, notification.MetadataKey(actor))
			}
		}
		for _, scope := range []string{"target", "prior linker"} {
			for _, mode := range []string{"untagged", "header", "explicit"} {
				for _, pending := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/pending_%v", scope, mode, pending), func(t *testing.T) {
						c := dbtest.RecordTeammateCase{}
						if mode != "untagged" {
							c.Header = "codex"
						}
						if mode == "explicit" {
							c.Present = true
							c.Body = "named-worker"
						}
						check(t, scope, c, pending)
					})
				}
			}
		}
		hegel.Test(t, func(ht *hegel.T) {
			c := dbtest.DrawRecordTeammate(ht)
			scope := "target"
			if hegel.Draw(ht, hegel.Booleans()) {
				scope = "prior linker"
			}
			check(ht, scope, c, hegel.Draw(ht, hegel.Booleans()))
		}, hegel.WithTestCases(32), hegel.WithDatabase(t.TempDir()))
	})
}

func TestCommentAddressingSnapshotPreservesOrigin(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "alice"})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		req := httptest.NewRequest(http.MethodPost, issueURLRef(project.ID, issue.ShortID, "comments"), strings.NewReader(`{"actor":"alice","body":"Finding"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Kata-Teammate", "codex")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, req)
		require.Equal(t, 200, out.Code, out.Body.String())
		_, err = store.EnableProjectFederation(t.Context(), project.ID, "alice")
		require.NoError(t, err)
		events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
		require.NoError(t, err)
		found := false
		for _, event := range events {
			if event.Type != "issue.snapshot" {
				continue
			}
			var payload struct {
				Comments []map[string]jsontext.Value `json:"comments"`
			}
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
			for _, comment := range payload.Comments {
				found = true
				require.Equal(t, `"codex"`, string(comment["teammate"]))
				require.Equal(t, `""`, string(comment["addressing_teammate"]), "snapshot must retain actor-only original addressing")
			}
		}
		require.True(t, found)
	})
}
