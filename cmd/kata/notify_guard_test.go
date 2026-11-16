package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNotifyNoReplaceKeepsHeldRequest(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "review the callback")
	runCLIAs(t, env, dir, "agent-a", "notify", ref, "--to", "reviewer", "--message", "first pass")

	_, err := runCLICapture(t, env, dir, "--as", "agent-b", "notify", ref, "--to", "reviewer", "--message", "second pass", "--no-replace")
	cli := requireCLIError(t, err, ExitConflict)
	assert.Equal(t, "notification_held", cli.Code)
	assert.Contains(t, cli.Message, "agent-a")
	assert.Contains(t, cli.Message, "first pass")

	metadata := notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.JSONEq(t, `{"from":"agent-a","message":"first pass"}`, metadata["notify.cmV2aWV3ZXI"])

	// An empty slot accepts the request.
	runCLIAs(t, env, dir, "agent-b", "notify", ref, "--to", "ops", "--message", "check rollout", "--no-replace")
	metadata = notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.JSONEq(t, `{"from":"agent-b","message":"check rollout"}`, metadata["notify.b3Bz"])
}

func TestNotifyClearIfFromClearsOnlyThatSender(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "review the callback")
	runCLIAs(t, env, dir, "agent-a", "notify", ref, "--to", "reviewer", "--message", "first pass")

	runCLIAs(t, env, dir, "agent-b", "notify", ref, "--to", "reviewer", "--clear", "--if-from", "agent-b")
	metadata := notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.Contains(t, metadata, "notify.cmV2aWV3ZXI")

	runCLIAs(t, env, dir, "agent-b", "notify", ref, "--to", "reviewer", "--clear", "--if-from", "agent-a")
	metadata = notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.NotContains(t, metadata, "notify.cmV2aWV3ZXI")
}

func TestNotifyGuardFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--to", "reader", "--message", "m", "--if-from", "a"},
		{"--to", "reader", "--clear", "--no-replace"},
		{"--broadcast", "--message", "m", "--no-replace"},
		{"--to", "reader", "--clear", "--if-from", " "},
	} {
		resetFlags(t)
		_, _, err := executeRootCapture(t, t.Context(), append([]string{"notify", "abcd"}, args...)...)
		_ = requireCLIError(t, err, ExitValidation)
	}
}

