package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestTeammateFlowSharedIssueAndChildHandoff(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "shared goal")
	initial, err := env.DB.IssueByShortID(t.Context(), pid, parent, db.IncludeDeletedNo)
	require.NoError(t, err)
	for _, handle := range []string{"teammate-1", "teammate-2"} {
		t.Setenv("KATA_TEAMMATE", handle)
		runCLIAs(t, env, dir, "coordinator", "comment", parent, "--body", "Progress from "+handle)
	}
	comments, err := env.DB.CommentsByIssue(t.Context(), initial.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	// Marshal storage results to check the public fields rather than a CLI-only label.
	encoded, err := json.Marshal(comments)
	require.NoError(t, err)
	var attributed []map[string]any
	require.NoError(t, json.Unmarshal(encoded, &attributed))
	for i, handle := range []string{"teammate-1", "teammate-2"} {
		assert.Equal(t, "coordinator", attributed[i]["author"])
		assert.Equal(t, handle, attributed[i]["teammate"])
		assert.NotEmpty(t, attributed[i]["uid"])
		assert.NotEmpty(t, attributed[i]["created_at"])
	}
	after, err := env.DB.IssueByID(t.Context(), initial.ID)
	require.NoError(t, err)
	assert.Equal(t, initial.Author, after.Author)
	assert.Equal(t, initial.Owner, after.Owner)
	assert.JSONEq(t, string(initial.Metadata), string(after.Metadata))

	createArgs := []string{"create", "delegated investigation", "--parent", parent, "--idempotency-key", "teammate-flow-child", "--json"}
	var child struct {
		Issue db.Issue `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCLIAs(t, env, dir, "coordinator", createArgs...)), &child))
	var retry struct {
		Issue db.Issue `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCLIAs(t, env, dir, "coordinator", createArgs...)), &retry))
	assert.Equal(t, child.Issue.UID, retry.Issue.UID)
	assert.Equal(t, "coordinator", child.Issue.Author)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(child.Issue.Metadata), &metadata))
	assert.Equal(t, "teammate-2", metadata["teammate"])
	links, err := env.DB.LinksByIssue(t.Context(), child.Issue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "parent", links[0].Type)
	assert.Equal(t, child.Issue.ID, links[0].FromIssueID)
	assert.Equal(t, initial.ID, links[0].ToIssueID)

	runCLIAs(t, env, dir, "coordinator", "notify", parent, "--to", "coordinator/teammate-1", "--message", "Please check my investigation")
	t.Setenv("KATA_TEAMMATE", "teammate-1")
	runCLIAs(t, env, dir, "coordinator", "notify", parent, "--to", "coordinator/teammate-2", "--message", "Please check my reproduction")
	read := func(recipient string) inboxOutput {
		t.Helper()
		var result inboxOutput
		require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "inbox", "--for", recipient, "--json")), &result))
		return result
	}
	first, second := read("coordinator/teammate-1"), read("coordinator/teammate-2")
	require.Len(t, first.Requests, 1)
	require.Len(t, second.Requests, 1)
	assert.Equal(t, "teammate-2", first.Requests[0].Teammate)
	assert.Equal(t, "teammate-1", second.Requests[0].Teammate)
	assert.Empty(t, read("coordinator").Requests)
	runCLI(t, env, dir, "notify", parent, "--to", "coordinator/teammate-1", "--clear")
	assert.Empty(t, read("coordinator/teammate-1").Requests)
	assert.Len(t, read("coordinator/teammate-2").Requests, 1)
}
