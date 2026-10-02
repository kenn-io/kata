package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestNativeAttentionReportsTransientDaemonLookupFailure(t *testing.T) {
	resetFlags(t)
	var issueLookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 42, "name": "example-project"},
			}))
		case "/api/v1/projects/42/issues/abc4":
			require.Equal(t, http.MethodGet, r.Method)
			issueLookups.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	stderr, err := executeNativeAttentionForTest(t, server.URL,
		"agent-hooks", "attention-native", "pi", "start", "--session", "retry-session",
		"--host-pid", strconv.Itoa(os.Getpid()), "--ref", "abc4", "--workspace", workspace)
	require.EqualValuesf(t, 1, issueLookups.Load(), "stderr=%q err=%v", stderr, err)
	require.ErrorContains(t, err, "native attention start: issue lookup unavailable")
	require.Contains(t, stderr, "native attention start: issue lookup unavailable")
}

func TestNativeAttentionReportsTransientMetadataWriteFailure(t *testing.T) {
	for _, mode := range []string{"start", "end"} {
		t.Run(mode, func(t *testing.T) {
			resetFlags(t)
			const session = "retry-session"
			hostPID := os.Getpid()
			generation, err := nativeAttentionLaunchGeneration(hostPID)
			require.NoError(t, err)
			sum := sha256.Sum256([]byte("pi\x00" + generation + "\x00" + session))
			owner := hex.EncodeToString(sum[:])
			var metadataWrites atomic.Int32
			storedOwner := "prior-owner"
			if mode == "end" {
				storedOwner = owner
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/projects/resolve":
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
						"project": map[string]any{"id": 42, "name": "example-project"},
					}))
				case "/api/v1/projects/42/issues/abc4":
					require.Equal(t, http.MethodGet, r.Method)
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{
						"short_id": "abc4", "status": "open", "revision": int64(17),
						"metadata": map[string]string{attentionKey: attnValueOK, attentionSessionKey: storedOwner},
					}}))
				case "/api/v1/projects/42/issues/abc4/metadata":
					require.Equal(t, http.MethodPost, r.Method)
					metadataWrites.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)

			workspace := t.TempDir()
			stderr, err := executeNativeAttentionForTest(t, server.URL,
				"agent-hooks", "attention-native", "pi", mode, "--session", session,
				"--host-pid", strconv.Itoa(hostPID), "--ref", "abc4", "--workspace", workspace)
			require.EqualValuesf(t, 1, metadataWrites.Load(), "stderr=%q err=%v", stderr, err)
			want := "native attention " + mode + ": metadata update failed"
			require.ErrorContains(t, err, want)
			require.Contains(t, stderr, want)
		})
	}
}

func executeNativeAttentionForTest(t *testing.T, baseURL string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	flags.Project = "example-project"
	cmd.SetContext(contextWithBaseURL(t.Context(), baseURL))
	var output bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil {
		emitRootError(&output, cmd, args, err, runEEntered)
	}
	return output.String(), err
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
	require.NoError(t, attnEndSession(d, "abc4", "old-owner"))
	require.Len(t, d.conditionalWrites, 1)
	require.Equal(t, map[string]string{attentionSessionKey: "ended:old-owner", attentionKey: "needs-human", attentionMsgKey: "session ended without hand-off"}, d.conditionalWrites[0].patch)
}

func TestNativeAttentionSessionHelpersReportTransientFailures(t *testing.T) {
	for _, mode := range []string{"start", "end"} {
		for _, failure := range []string{"lookup", "write", "conflicts"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				owner := "previous-owner"
				if mode == "end" {
					owner = "current-owner"
				}
				open := attnLookup{kind: lookupOpen, attention: attnValueOK, session: owner, revision: 13}
				d := &fakeAttnDaemon{lookups: map[string]attnLookup{"abc4": open}}
				want := "native attention " + mode + ": "
				switch failure {
				case "lookup":
					d.lookupSequence = []attnLookup{{kind: lookupTransient}}
					want += "issue lookup unavailable"
				case "write":
					d.conditionalResults = []attnWriteResult{attnWriteFailed}
					want += "metadata update failed"
				case "conflicts":
					d.lookupSequence = make([]attnLookup, attnWriteAttempts)
					d.conditionalResults = make([]attnWriteResult, attnWriteAttempts)
					for i := range d.lookupSequence {
						d.lookupSequence[i] = open
						d.conditionalResults[i] = attnWriteConflict
					}
					want += "metadata changed repeatedly"
				}

				var err error
				if mode == "start" {
					err = attnStartSession(d, "abc4", "current-owner")
				} else {
					err = attnEndSession(d, "abc4", owner)
				}
				require.ErrorContains(t, err, want)
			})
		}
	}
}

func TestNativeAttentionSessionHelpersKeepNonActionableCasesSilent(t *testing.T) {
	missing := &fakeAttnDaemon{lookups: map[string]attnLookup{"abc4": {kind: lookupGone}}}
	require.NoError(t, attnStartSession(missing, "abc4", "current-owner"))
	require.NoError(t, attnEndSession(missing, "abc4", "current-owner"))
	stale := &fakeAttnDaemon{lookups: map[string]attnLookup{"abc4": {
		kind: lookupOpen, attention: attnValueOK, session: "new-owner", revision: 14,
	}}}
	require.NoError(t, attnEndSession(stale, "abc4", "old-owner"))
	require.Empty(t, stale.conditionalWrites)
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
