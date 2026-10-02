package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestE2E_NativeAttentionExplicitIdentity(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "native lifecycle work")
	t.Setenv("CLAUDE_PROJECT_DIR", t.TempDir())
	t.Setenv("KATA_REF", "unrelated")
	t.Setenv("KATA_SESSION_ID", "example-launch")
	invoke := func(mode, session string) {
		t.Helper()
		resetFlags(t)
		out, stderr, err := executeAttentionAtDaemon(t, env, unreadableHookInput{}, "agent-hooks", "attention-native", "pi", mode, "--session", session, "--host-pid", strconv.Itoa(os.Getpid()), "--ref", ref, "--workspace", dir)
		require.NoError(t, err)
		require.Empty(t, out)
		require.Empty(t, stderr)
	}
	invoke("start", "old-session")
	got, _ := attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "ok", got)
	// Same-launch duplicate must preserve an explicit handoff.
	runCLI(t, env, dir, "meta", "set", ref, attentionKey, "stuck")
	invoke("start", "old-session")
	got, _ = attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "stuck", got)
	// Native replacement establishes a new owner; detached old end is fenced.
	invoke("start", "new-session")
	invoke("end", "old-session")
	got, _ = attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "ok", got)
	invoke("end", "new-session")
	got, _ = attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "needs-human", got)
	// Resuming a native ID in a new launch establishes the next baseline.
	t.Setenv("KATA_SESSION_ID", "next-launch")
	invoke("start", "new-session")
	got, _ = attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "ok", got)
}

func TestNativeAttentionInvalidPayloadIsSilent(t *testing.T) {
	t.Setenv("KATA_REF", "")
	for _, payload := range []string{"", `{`, `[]`, `{"session_id":3}`, `{"session_id":"one","sessionId":"two"}`} {
		out, stderr, err := executeAgentHook(t, strings.NewReader(payload), "agent-hooks", "attention-native", "claude", "start")
		require.NoError(t, err)
		require.Empty(t, out)
		require.Empty(t, stderr)
	}
}

func TestNativeAttentionLaunchGeneration(t *testing.T) {
	t.Setenv("KATA_SESSION_ID", "")
	first, err := nativeAttentionLaunchGeneration(os.Getpid())
	require.NoError(t, err)
	require.NotEmpty(t, first)
	second, err := nativeAttentionLaunchGeneration(os.Getpid())
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, err = nativeAttentionLaunchGeneration(-1)
	require.Error(t, err)
}

func TestNativeAttentionPayload(t *testing.T) {
	for _, tc := range []struct{ raw, wantID, wantDir string }{
		{`{"session_id":"native-one","cwd":"/example/workspace"}`, "native-one", "/example/workspace"},
		{`{"sessionId":"copilot-one","cwd":"/example/workspace"}`, "copilot-one", "/example/workspace"},
		{`{"conversation_id":"cursor-one","workspace_roots":["/example/workspace"]}`, "cursor-one", "/example/workspace"},
	} {
		id, dir, err := readNativeAttentionPayload(strings.NewReader(tc.raw))
		require.NoError(t, err)
		require.Equal(t, tc.wantID, id)
		require.Equal(t, tc.wantDir, dir)
	}
}

func TestNativeAttentionEndConflictRechecksSessionOwner(t *testing.T) {
	d := &fakeAttnDaemon{lookupSequence: []attnLookup{
		{kind: lookupOpen, attention: "ok", session: "old-owner", revision: 3},
		{kind: lookupOpen, attention: "ok", session: "new-owner", revision: 4},
	}, conditionalResults: []attnWriteResult{attnWriteConflict}}
	attnEndSession(d, "abc4", "old-owner")
	require.Len(t, d.conditionalWrites, 1)
	require.Equal(t, map[string]string{attentionSessionKey: "ended:old-owner", attentionKey: "needs-human", attentionMsgKey: "session ended without hand-off"}, d.conditionalWrites[0].patch)
}

func TestNativeAttentionExplicitHostResumeWithoutLauncherOverride(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "resumed native work")
	t.Setenv("KATA_SESSION_ID", "")
	t.Setenv("CLAUDE_PROJECT_DIR", t.TempDir())
	invoke := func(mode string, host int) {
		t.Helper()
		resetFlags(t)
		_, _, err := executeAttentionAtDaemon(t, env, unreadableHookInput{}, "agent-hooks", "attention-native", "pi", mode, "--session", "resumed-id", "--host-pid", strconv.Itoa(host), "--ref", ref, "--workspace", dir)
		require.NoError(t, err)
	}
	invoke("start", os.Getpid())
	invoke("end", os.Getpid())
	got, _ := attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "needs-human", got)
	// Use the still-live parent as a distinct launch identity; creation time is
	// resolved from the actual OS, with no optional launcher token.
	invoke("start", os.Getppid())
	got, _ = attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "ok", got)
	// A detached end from the previous host cannot end the resumed host.
	invoke("end", os.Getpid())
	got, _ = attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "ok", got)
}

func TestE2E_NativeAttentionRejectsWrongEventAndSubagent(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "parent session work")
	t.Setenv("KATA_SESSION_ID", "event-launch")
	t.Setenv("KATA_REF", ref)
	resetFlags(t)
	_, _, err := executeAttentionAtDaemon(t, env, unreadableHookInput{}, "agent-hooks", "attention-native", "grok", "start", "--session", "parent-session", "--host-pid", strconv.Itoa(os.Getpid()), "--ref", ref, "--workspace", dir)
	require.NoError(t, err)
	for _, payload := range []string{
		`{"sessionId":"parent-session","hookEventName":"Stop"}`,
		`{"sessionId":"parent-session","hookEventName":"SessionStart"}`,
		`{"sessionId":"parent-session","hookEventName":"SessionEnd","subagentType":"explorer"}`,
		`{"sessionId":"parent-session"}`,
	} {
		resetFlags(t)
		_, _, err = executeAttentionAtDaemon(t, env, strings.NewReader(payload), "agent-hooks", "attention-native", "grok", "end", "--workspace", dir)
		require.NoError(t, err)
		got, _ := attnMetaValue(t, env, pid, ref, attentionKey)
		require.Equal(t, "ok", got, payload)
	}
	resetFlags(t)
	_, _, err = executeAttentionAtDaemon(t, env, strings.NewReader(`{"sessionId":"parent-session","hookEventName":"SessionEnd"}`), "agent-hooks", "attention-native", "grok", "end", "--workspace", dir)
	require.NoError(t, err)
	got, _ := attnMetaValue(t, env, pid, ref, attentionKey)
	require.Equal(t, "needs-human", got)
}

func TestNativeAttentionLifecyclePayload(t *testing.T) {
	for _, tc := range []struct {
		target, mode, raw string
		valid             bool
	}{
		{"hermes", "start", `{"session_id":"one","hook_event_name":"on_session_reset"}`, true},
		{"hermes", "end", `{"session_id":"one","hook_event_name":"on_session_finalize"}`, true},
		{"hermes", "end", `{"session_id":"one","hook_event_name":"on_session_end"}`, false},
		{"cursor", "start", `{"conversation_id":"one","hook_event_name":"sessionStart","workspace_roots":["/example"]}`, true},
		{"copilot", "start", `{"sessionId":"one","source":"resume","cwd":"/example"}`, true},
		{"copilot", "end", `{"sessionId":"one","reason":"user_exit","cwd":"/example"}`, true},
		{"copilot", "end", `{"sessionId":"one","stopReason":"end_turn","cwd":"/example"}`, false},
		{"claude", "start", `{"session_id":"one","hook_event_name":"SessionEnd"}`, false},
		{"grok", "end", `{"sessionId":"one","hookEventName":"SessionEnd","hook_event_name":"Stop"}`, false},
	} {
		t.Run(tc.target+tc.mode+tc.raw, func(t *testing.T) {
			_, _, err := readNativeAttentionPayloadFor(tc.target, tc.mode, strings.NewReader(tc.raw))
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
