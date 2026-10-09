package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
				assert.Equal(t, map[string]any{"agent": "codex", "session_id": transcriptSessionOne, "url": "https://agentsview.example/archive/sessions/codex/" + transcriptSessionOne}, payload["transcript"])
				// Readers retrieve the same provenance without keeping the close receipt.
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
				assert.Equal(t, payload["transcript"], provenance)
			} else {
				assert.NotContains(t, payload, "transcript")
			}
		})
	}
}

func TestCloseTranscript_ContextAndOptionalLink(t *testing.T) {
	for _, tc := range []struct {
		name, agent, id, thread, session, base, wantAgent, wantID, warning string
	}{
		{name: "no context", warning: "current session unavailable"},
		{name: "conflicting Codex context", thread: transcriptSessionOne, session: transcriptSessionTwo, warning: "ambiguous"},
		{name: "partial explicit context", agent: "claude", thread: transcriptSessionOne, warning: "current session unavailable"},
		{name: "explicit Claude overrides inherited Codex", agent: "claude", id: transcriptSessionTwo, thread: transcriptSessionOne, wantAgent: "claude", wantID: transcriptSessionTwo},
		{name: "session alias", session: transcriptSessionOne, wantAgent: "codex", wantID: transcriptSessionOne},
		{name: "no AgentsView integration", thread: transcriptSessionOne, wantAgent: "codex", wantID: transcriptSessionOne},
		{name: "bad link retains ID", thread: transcriptSessionOne, base: "https://user:secret@agentsview.example?token=secret", wantAgent: "codex", wantID: transcriptSessionOne, warning: "URL invalid"},
		{name: "path never persisted", agent: "codex", id: "/private/session/chat.jsonl", warning: "current session unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled=true\nagentsview_url="+fmt.Sprintf("%q", tc.base)+"\n"), 0600))
			for name, value := range map[string]string{"KATA_TRANSCRIPT_AGENT": tc.agent, "KATA_TRANSCRIPT_SESSION_ID": tc.id, "CODEX_THREAD_ID": tc.thread, "CODEX_SESSION_ID": tc.session} {
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
				assert.Equal(t, tc.wantAgent, ref.Agent)
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
