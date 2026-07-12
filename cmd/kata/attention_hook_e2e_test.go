package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// runAttnHook runs the real hidden command against env's live daemon. The
// launcher project directory, rather than hook stdin or process cwd, anchors
// normal workspace/project resolution.
func runAttnHook(t *testing.T, env *testenv.Env, dir string, args ...string) error {
	t.Helper()
	resetFlags(t)
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	return cmd.Execute()
}

func attnMetaValue(t *testing.T, env *testenv.Env, pid int64, ref, key string) (string, bool) {
	t.Helper()
	type resp struct {
		Issue struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"issue"`
	}
	got := getJSON[resp](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref)
	raw, ok := got.Issue.Metadata[key]
	if !ok {
		return "", false
	}
	var value string
	require.NoError(t, json.Unmarshal(raw, &value))
	return value, true
}

func TestE2E_AttentionHook_StartSetsOnlyAttentionFromKataRef(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "launcher-tracked work")
	runCLI(t, env, dir, "meta", "set", ref, attentionMsgKey, "existing context")
	t.Setenv("KATA_REF", ref)

	require.NoError(t, runAttnHook(t, env, dir, "attention-hook", "start"))

	value, ok := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, ok)
	assert.Equal(t, attnValueOK, value)
	message, ok := attnMetaValue(t, env, pid, ref, attentionMsgKey)
	require.True(t, ok)
	assert.Equal(t, "existing context", message)
}

func TestE2E_AttentionHook_EndEscalatesDirectlyFromKataRefWithoutState(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "unhanded-off work")
	runCLI(t, env, dir, "meta", "set", ref, attentionKey, attnValueOK)
	t.Setenv("KATA_REF", ref)

	// SessionEnd needs only the launcher-provided ref: stdin is empty and no
	// session state is created or consulted.
	require.NoError(t, runAttnHook(t, env, dir, "attention-hook", "end"))

	value, ok := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, ok)
	assert.Equal(t, attnValueNeedsHuman, value)
	message, ok := attnMetaValue(t, env, pid, ref, attentionMsgKey)
	require.True(t, ok)
	assert.Equal(t, attnHandoffMsg, message)
}

func TestE2E_AttentionHook_EndSkipsClosedIssue(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "closed before session end")
	runCLI(t, env, dir, "meta", "set", ref, attentionKey, attnValueOK)
	runCLIAs(t, env, dir, "agent-a", "close", ref, "--done", "--message",
		"closing this issue before the session-end attention hook runs",
		"--commit", "deadbeef")
	t.Setenv("KATA_REF", ref)

	require.NoError(t, runAttnHook(t, env, dir, "attention-hook", "end"))

	value, ok := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, ok)
	assert.Equal(t, attnValueOK, value)
	_, ok = attnMetaValue(t, env, pid, ref, attentionMsgKey)
	assert.False(t, ok)
}

func TestE2E_AttentionHook_ConditionalEndRejectsConcurrentChange(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "concurrent hand-off")
	runCLI(t, env, dir, "meta", "set", ref, attentionKey, attnValueOK)

	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	flags.Workspace = dir
	d := &liveAttnDaemon{cmd: cmd}
	lookup := d.lookup(ref)
	require.Equal(t, lookupOpen, lookup.kind)

	// A deliberate hand-off after lookup advances the revision. The stale
	// If-Match must reject the hook patch and preserve the newer value.
	runCLI(t, env, dir, "meta", "set", ref, attentionKey, "stuck")
	result := d.setMetaIfRevision(ref, map[string]string{
		attentionKey:    attnValueNeedsHuman,
		attentionMsgKey: attnHandoffMsg,
	}, lookup.revision)
	assert.Equal(t, attnWriteConflict, result)

	value, exists := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, exists)
	assert.Equal(t, "stuck", value)
}
