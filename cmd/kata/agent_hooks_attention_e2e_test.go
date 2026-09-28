package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestAgentHooksAttentionUsage(t *testing.T) {
	t.Setenv("KATA_REF", "")
	for _, mode := range []string{"start", "end"} {
		t.Run(mode, func(t *testing.T) {
			for _, args := range [][]string{
				{"unexpected"}, {"--source", "foreign"}, {"--source="},
				{"--source", "kata-agent-hook-" + map[string]string{"start": "end", "end": "start"}[mode]},
			} {
				t.Run(strings.Join(args, "_"), func(t *testing.T) {
					stdout, stderr, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hooks", "attention", mode}, args...)...)
					require.Error(t, err)
					assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
					assert.Empty(t, stdout)
					assert.NotEmpty(t, stderr)
				})
			}
			for _, marker := range [][]string{nil, {"--source", "kata-agent-hook-" + mode}} {
				stdout, stderr, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hooks", "attention", mode}, marker...)...)
				require.NoError(t, err)
				assert.Empty(t, stdout)
				assert.Empty(t, stderr)
			}
		})
	}
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "attention", "unknown", "claude")
	require.Error(t, err)
	assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
}

type agentHookAttentionSnapshot struct {
	Issue struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
		Revision int64                      `json:"revision"`
	} `json:"issue"`
}

func attentionSnapshot(t *testing.T, env *testenv.Env, pid int64, ref string) agentHookAttentionSnapshot {
	t.Helper()
	return getJSON[agentHookAttentionSnapshot](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref)
}

func executeAttentionAtDaemon(t *testing.T, env *testenv.Env, input io.Reader, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetIn(input)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestE2E_AgentHooksAttentionParity(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	for _, scenario := range []struct {
		name        string
		mode        string
		initial     string
		closed      bool
		want        string
		wantMessage string
	}{
		{"start absent", "start", "", false, `"ok"`, `"existing context"`},
		{"start handoff", "start", `"needs-human"`, false, `"ok"`, `"existing context"`},
		{"end active", "end", `"ok"`, false, `"needs-human"`, `"session ended without hand-off"`},
		{"end stuck", "end", `"stuck"`, false, `"stuck"`, `"existing context"`},
		{"end handoff", "end", `"needs-human"`, false, `"needs-human"`, `"existing context"`},
		{"end absent", "end", "", false, "", `"existing context"`},
		{"end number", "end", `7`, false, `7`, `"existing context"`},
		{"end closed", "end", `"ok"`, true, `"ok"`, `"existing context"`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var snapshots []agentHookAttentionSnapshot
			var deltas []int64
			for _, visible := range []bool{false, true} {
				ref := createIssue(t, env, pid, "example tracked work")
				runCLI(t, env, dir, "meta", "set", ref, attentionMsgKey, "existing context")
				if scenario.initial != "" {
					runCLI(t, env, dir, "meta", "set", ref, attentionKey, scenario.initial, "--json-value")
				}
				if scenario.closed {
					runCLIAs(t, env, dir, "example-actor", "close", ref, "--done", "--message",
						"Completed example work before the session ended; lifecycle must preserve it.", "--commit", "deadbeef")
				}
				before := attentionSnapshot(t, env, pid, ref)
				t.Setenv("KATA_REF", ref)
				args := []string{"attention-hook", scenario.mode, "--source", "kata-agent-hook-" + scenario.mode}
				if visible {
					args = []string{"agent-hooks", "attention", scenario.mode}
				}
				payload := `{"hook_event_name":"SessionStart","session_id":"example-session","source":"startup"}`
				if scenario.mode == "end" {
					payload = `{"hook_event_name":"SessionEnd","session_id":"example-session","reason":"complete"}`
				}
				stdout, stderr, err := executeAttentionAtDaemon(t, env, strings.NewReader(payload), args...)
				require.NoError(t, err)
				assert.Empty(t, stdout)
				assert.Empty(t, stderr)
				after := attentionSnapshot(t, env, pid, ref)
				assert.Equal(t, scenario.want, string(after.Issue.Metadata[attentionKey]))
				assert.Equal(t, scenario.wantMessage, string(after.Issue.Metadata[attentionMsgKey]))
				snapshots = append(snapshots, after)
				deltas = append(deltas, after.Issue.Revision-before.Issue.Revision)
			}
			assert.Equal(t, snapshots[0].Issue.Metadata, snapshots[1].Issue.Metadata)
			assert.Equal(t, deltas[0], deltas[1])
		})
	}
}
