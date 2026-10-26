package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

const transcriptSessionOne = "00000000-0000-4000-8000-000000000001"
const transcriptSessionTwo = "00000000-0000-4000-8000-000000000002"

func TestCloseTranscript_OptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Completed example work")
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("CODEX_THREAD_ID", transcriptSessionOne)
			t.Setenv("CODEX_SESSION_ID", transcriptSessionOne)
			t.Setenv("CLAUDE_CODE_SESSION_ID", "")
			t.Setenv("KATA_TRANSCRIPT_AGENT", "")
			t.Setenv("KATA_TRANSCRIPT_SESSION_ID", "")
			if enabled {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\nagentsview_url = \"https://agentsview.example/archive/\"\n"), 0600))
			}
			out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref,
				"--done", "--message", "Implemented the example behavior and ran the focused tests.", "--test", "go test ./internal/example")
			require.NoError(t, err)
			require.Empty(t, stderr)
			var response struct {
				Event struct {
					Payload string `json:"payload"`
				} `json:"event"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(response.Event.Payload), &payload))
			if enabled {
				assert.Equal(t, map[string]any{"session_id": transcriptSessionOne, "url": "https://agentsview.example/archive/sessions/" + transcriptSessionOne}, payload["transcript"])
				// Readers retrieve the same provenance without keeping the close receipt.
				provenance := closeTranscriptFromHistory(t, env, dir)
				assert.Equal(t, payload["transcript"], provenance)
			} else {
				assert.NotContains(t, payload, "transcript")
			}
		})
	}
}

func TestCloseTranscript_ConfiguredSessionEnv(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "Completed example work")
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("PI_SESSION_ID", transcriptSessionTwo)
	t.Setenv("CODEX_THREAD_ID", transcriptSessionOne)
	t.Setenv("CODEX_SESSION_ID", transcriptSessionOne)
	t.Setenv("CLAUDE_CODE_SESSION_ID", transcriptSessionOne)
	t.Setenv("KATA_TRANSCRIPT_SESSION_ID", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\nsession_id_env = [\"PI_SESSION_ID\"]\nagentsview_url = \"https://agentsview.example/archive/\"\n"), 0600))

	_, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref,
		"--done", "--message", "Implemented the example behavior and ran the focused tests.", "--test", "go test ./internal/example")
	require.NoError(t, err)
	require.Empty(t, stderr)

	provenance := closeTranscriptFromHistory(t, env, dir)
	assert.Equal(t, map[string]any{"session_id": transcriptSessionTwo, "url": "https://agentsview.example/archive/sessions/" + transcriptSessionTwo}, provenance)
}

func closeTranscriptFromHistory(t *testing.T, env *testenv.Env, dir string) any {
	t.Helper()
	history := runCLI(t, env, dir, "--json", "events", "--after", "0", "--limit", "100")
	var stream struct {
		Events []struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal([]byte(history), &stream))
	var provenance any
	for _, event := range stream.Events {
		if event.Type == "issue.closed" {
			provenance = event.Payload["transcript"]
		}
	}
	return provenance
}

func TestCloseTranscript_ContextAndOptionalLink(t *testing.T) {
	for _, tc := range []struct {
		name, id, thread, session, claude, pi, variables, base, wantID, warning string
	}{
		{name: "no context", warning: "current session unavailable"},
		{name: "conflicting Codex context", thread: transcriptSessionOne, session: transcriptSessionTwo, warning: "ambiguous"},
		{name: "configured session", variables: `["PI_SESSION_ID"]`, pi: transcriptSessionTwo, thread: transcriptSessionOne, wantID: transcriptSessionTwo},
		{name: "default ignores unconfigured session", pi: transcriptSessionOne, warning: "current session unavailable"},
		{name: "configured conflict", variables: `["PI_SESSION_ID", "CODEX_THREAD_ID"]`, pi: transcriptSessionOne, thread: transcriptSessionTwo, warning: "ambiguous"},
		{name: "equal aliases", thread: transcriptSessionOne, session: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "explicit session overrides inherited context", id: transcriptSessionTwo, thread: transcriptSessionOne, wantID: transcriptSessionTwo},
		{name: "Claude Code context", claude: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "nested agent context", claude: transcriptSessionTwo, thread: transcriptSessionOne, warning: "ambiguous"},
		{name: "override resolves configured conflict", variables: `["PI_SESSION_ID", "CODEX_THREAD_ID"]`, pi: transcriptSessionOne, id: transcriptSessionOne, claude: transcriptSessionTwo, thread: transcriptSessionTwo, wantID: transcriptSessionOne},
		{name: "session alias", session: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "no AgentsView integration", thread: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "bad link retains ID", thread: transcriptSessionOne, base: "https://user:secret@agentsview.example?token=secret", wantID: transcriptSessionOne, warning: "URL invalid"},
		{name: "path never persisted", id: "/private/session/chat.jsonl", warning: "current session unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			variables := ""
			if tc.variables != "" {
				variables = "session_id_env=" + tc.variables + "\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\n"+variables+"enabled=true\nagentsview_url="+fmt.Sprintf("%q", tc.base)+"\n"), 0600))
			for name, value := range map[string]string{"KATA_TRANSCRIPT_AGENT": "ignored", "PI_SESSION_ID": tc.pi, "KATA_TRANSCRIPT_SESSION_ID": tc.id, "CODEX_THREAD_ID": tc.thread, "CODEX_SESSION_ID": tc.session, "CLAUDE_CODE_SESSION_ID": tc.claude} {
				t.Setenv(name, value)
			}
			cmd := &cobra.Command{}
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			ref := closeTranscript(cmd)
			if tc.wantID == "" {
				assert.Nil(t, ref)
			} else {
				require.NotNil(t, ref)
				assert.Equal(t, tc.wantID, ref.SessionID)
				assert.Empty(t, ref.Agent)
				assert.Empty(t, ref.URL)
			}
			if tc.warning == "" {
				assert.Empty(t, stderr.String())
			} else {
				assert.Contains(t, stderr.String(), tc.warning)
			}
			assert.NotContains(t, stderr.String(), "secret")
			assert.NotContains(t, stderr.String(), "/private")
		})
	}
}

func TestCloseTranscript_OldDaemonClosesWithoutProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\n"), 0600))
	t.Setenv("CODEX_THREAD_ID", transcriptSessionOne)
	for _, name := range []string{"CODEX_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "KATA_TRANSCRIPT_AGENT", "KATA_TRANSCRIPT_SESSION_ID"} {
		t.Setenv(name, "")
	}
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.32.0"}`))
		case "/api/v1/projects/1/issues/abc1/actions/close":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
			_, _ = w.Write([]byte(`{"changed":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, stderr, err := executeRootCapture(t,
		contextWithBaseURL(context.Background(), server.URL),
		"--project", "example-project", "close", "abc1", "--done",
		"--message", "Implemented the example behavior and ran the focused tests.",
		"--test", "go test ./internal/example")
	require.NoError(t, err)
	require.NotNil(t, sent)
	assert.NotContains(t, sent, "transcript")
	assert.Contains(t, stderr, "reports 0.32.0")
	assert.Contains(t, stderr, "skipping attachment")
}

func TestCloseTranscript_MalformedConfigurationNamesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript\n"), 0600))
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	assert.Nil(t, closeTranscript(cmd))
	assert.Contains(t, stderr.String(), filepath.Join(home, "config.toml"))
	assert.Contains(t, stderr.String(), "skipping attachment")
}
