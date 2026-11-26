package mcpserver

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// R3/R9: every MCP issue and comment surface keeps accountable identity separate
// from original attribution. Pending/legacy labels must never become verified.
func TestMCPAttributionSummaries(t *testing.T) {
	h := toolHandlers{}
	for _, state := range []string{"verified", "pending", "legacy"} {
		t.Run(state, func(t *testing.T) {
			const id = "11111111-1111-4111-8111-111111111111"
			raw := "Ana [member:" + id + "; enrollment:" + id + "]"
			accountable, source, authority, teammate := "Root [service-account:"+id+"; token:"+id+"]", "Bo [member:"+id+"; enrollment:"+id+"]", "01J00000000000000000000005", "helper-agent"
			var verifiedAccountable *string
			if state == "verified" {
				verifiedAccountable = &accountable
			}
			project := ProjectIdentity{Name: "shared-project"}
			summaries := map[string]any{
				"mutation":     h.summaryFromIssue(project, generated.Issue{Author: raw, AccountableActor: verifiedAccountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.IssueVerification(state)), UpdatedAt: time.Now()}),
				"project_list": h.summaryFromIssueOut(project, generated.IssueOut{Author: raw, AccountableActor: verifiedAccountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.IssueOutVerification(state)), UpdatedAt: time.Now()}),
				"global_list":  summaryFromGlobalIssue(generated.ListGlobalIssueOut{Author: raw, AccountableActor: verifiedAccountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.ListGlobalIssueOutVerification(state)), UpdatedAt: time.Now()}),
				"global_ready": summaryFromReadyGlobalIssue(generated.ReadyGlobalIssueOut{Author: raw, AccountableActor: verifiedAccountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.ReadyGlobalIssueOutVerification(state)), UpdatedAt: time.Now()}),
				"comment":      commentSummary(generated.Comment{Author: raw, AccountableActor: verifiedAccountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.CommentVerification(state)), CreatedAt: time.Now()}),
			}
			for name, summary := range summaries {
				t.Run(name, func(t *testing.T) {
					encoded, err := json.Marshal(summary)
					require.NoError(t, err)
					var record map[string]any
					require.NoError(t, json.Unmarshal(encoded, &record))
					if state == "verified" {
						require.Equal(t, "Root", record["accountable_actor"])
					} else {
						require.NotContains(t, record, "accountable_actor")
					}
					require.Equal(t, "Bo", record["source_actor"])
					require.Equal(t, "Ana", record["author"])
					require.Equal(t, "helper-agent", record["teammate"])
					require.Equal(t, "01J00000000000000000000005", record["authority_uid"])
					require.Equal(t, state, record["verification"])
				})
			}
		})
	}
}

// Comment links and events show federated names while stream payloads stay raw.
func TestMCPFederatedAuthorDisplay(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	raw := "Ana [member:" + id + "; enrollment:" + id + "]"
	accountable := "Root [service-account:" + id + "; token:" + id + "]"
	link := generated.CommentLink{Kind: "reply", Author: &raw}
	summaries := map[string]any{
		"comment_read": commentReadSummary(generated.CommentOut{Author: raw, SourceActor: &raw, AccountableActor: &accountable, Verification: new(generated.CommentOutVerification("verified")), Reply: &link}),
		"event":        eventSummary(&generated.Event{UID: "01J00000000000000000000006", Type: "issue.created", Actor: raw}),
	}
	for name, summary := range summaries {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(summary)
			require.NoError(t, err)
			var record map[string]any
			require.NoError(t, json.Unmarshal(encoded, &record))
			if name == "event" {
				require.Equal(t, "Ana", record["actor"])
				return
			}
			require.Equal(t, "Ana", record["author"])
			require.Equal(t, "Ana", record["source_actor"])
			require.Equal(t, "Root", record["accountable_actor"])
			if reply, ok := record["reply"].(map[string]any); ok {
				require.Equal(t, "Ana", reply["author"])
			}
		})
	}
	t.Run("stream_event", func(t *testing.T) {
		payload := []byte(`{"author":"` + raw + `"}`)
		envelope := generated.EventEnvelope{Actor: raw, Payload: payload}
		event := streamEvent(envelope)
		require.Equal(t, "Ana", event.Actor)
		require.Equal(t, map[string]any{"author": raw}, event.Payload)
		require.JSONEq(t, `{"author":"`+raw+`"}`, string(envelope.Payload))
	})
}
