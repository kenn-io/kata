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
			accountable, source, authority, teammate := "member", "origin-agent", "01J00000000000000000000005", "helper-agent"
			project := ProjectIdentity{Name: "shared-project"}
			summaries := map[string]any{
				"mutation":     h.summaryFromIssue(project, generated.Issue{Author: "source-author", AccountableActor: &accountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.IssueVerification(state)), UpdatedAt: time.Now()}),
				"project_list": h.summaryFromIssueOut(project, generated.IssueOut{Author: "source-author", AccountableActor: &accountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.IssueOutVerification(state)), UpdatedAt: time.Now()}),
				"global_list":  summaryFromGlobalIssue(generated.ListGlobalIssueOut{Author: "source-author", AccountableActor: &accountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.ListGlobalIssueOutVerification(state)), UpdatedAt: time.Now()}),
				"global_ready": summaryFromReadyGlobalIssue(generated.ReadyGlobalIssueOut{Author: "source-author", AccountableActor: &accountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.ReadyGlobalIssueOutVerification(state)), UpdatedAt: time.Now()}),
				"comment":      commentSummary(generated.Comment{Author: "source-author", AccountableActor: &accountable, SourceActor: &source, AuthorityUID: &authority, Teammate: &teammate, Verification: new(generated.CommentVerification(state)), CreatedAt: time.Now()}),
			}
			for name, summary := range summaries {
				t.Run(name, func(t *testing.T) {
					encoded, err := json.Marshal(summary)
					require.NoError(t, err)
					var record map[string]any
					require.NoError(t, json.Unmarshal(encoded, &record))
					require.Equal(t, "member", record["accountable_actor"])
					require.Equal(t, "origin-agent", record["source_actor"])
					require.Equal(t, "source-author", record["author"])
					require.Equal(t, "helper-agent", record["teammate"])
					require.Equal(t, "01J00000000000000000000005", record["authority_uid"])
					require.Equal(t, state, record["verification"])
				})
			}
		})
	}
}
