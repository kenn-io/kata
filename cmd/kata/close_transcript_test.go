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
		name, id, thread, session, claude, pi, variables, base, wantID, agent, wantAgent, warning string
	}{
		{name: "no context", warning: "current session unavailable"},
		{name: "conflicting Codex context", thread: transcriptSessionOne, session: transcriptSessionTwo, warning: "ambiguous"},
		{name: "differently cased IDs for one agent", thread: "00000000-0000-4000-8000-00000000000a", session: "00000000-0000-4000-8000-00000000000A", warning: "ambiguous"},
		{name: "configured session", variables: `pi = "PI_SESSION_ID"`, pi: transcriptSessionTwo, wantID: transcriptSessionTwo, wantAgent: "pi"},
		{name: "invalid configured session names variable", variables: `pi = "PI_SESSION_ID"`, pi: "/private/session/chat.jsonl", warning: "PI_SESSION_ID"},
		{name: "default ignores unconfigured session", pi: transcriptSessionOne, warning: "current session unavailable"},
		{name: "equal aliases", wantAgent: "codex", thread: transcriptSessionOne, session: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "Claude Code context", wantAgent: "claude", claude: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "different agents with equal UUIDs", claude: transcriptSessionOne, thread: transcriptSessionOne, warning: "ambiguous"},
		{name: "ID without agent", id: transcriptSessionOne, warning: "current session unavailable or invalid; skipping attachment"},
		{name: "nested agent context", claude: transcriptSessionTwo, thread: transcriptSessionOne, warning: "ambiguous"},
		{name: "override resolves discovered conflict", pi: transcriptSessionOne, id: transcriptSessionOne, agent: "pi", wantAgent: "pi", claude: transcriptSessionTwo, thread: transcriptSessionTwo, wantID: transcriptSessionOne},
		{name: "session alias", wantAgent: "codex", session: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "no AgentsView integration", wantAgent: "codex", thread: transcriptSessionOne, wantID: transcriptSessionOne},
		{name: "bad link retains ID", wantAgent: "codex", thread: transcriptSessionOne, base: "https://user:secret@agentsview.example?token=secret", wantID: transcriptSessionOne, warning: "URL invalid"},
		{name: "agent without session skips discovery", claude: transcriptSessionOne, agent: "codex", warning: "current session unavailable or invalid; skipping attachment"},
		{name: "configured alias retains Claude agent", variables: `claude = "PI_SESSION_ID"`, pi: transcriptSessionOne, claude: transcriptSessionOne, wantID: transcriptSessionOne, wantAgent: "claude"},
		{name: "configured variable cannot hide built-in conflict", variables: `pi = "PI_SESSION_ID"`, pi: transcriptSessionOne, thread: transcriptSessionTwo, warning: "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			variables := ""
			if tc.variables != "" {
				variables = "[close.transcript.session_id_env]\n" + tc.variables + "\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled=true\nagentsview_url="+fmt.Sprintf("%q", tc.base)+"\n"+variables), 0600))
			for name, value := range map[string]string{"KATA_TRANSCRIPT_AGENT": tc.agent, "PI_SESSION_ID": tc.pi, "KATA_TRANSCRIPT_SESSION_ID": tc.id, "CODEX_THREAD_ID": tc.thread, "CODEX_SESSION_ID": tc.session, "CLAUDE_CODE_SESSION_ID": tc.claude} {
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

func TestCloseTranscript_DaemonCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, version, variable, agent, id string
		attach                             bool
	}{
		{"Codex on 0.26", "0.26.0", "CODEX_SESSION_ID", "codex", transcriptSessionOne, true},
		{"Claude on 0.33", "0.33.0", "CLAUDE_CODE_SESSION_ID", "claude", transcriptSessionOne, true},
		{"Pi on 0.33", "0.33.0", "PI_SESSION_ID", "pi", transcriptSessionOne, false},
		{"Pi on 0.34", "0.34.0", "PI_SESSION_ID", "pi", transcriptSessionOne, true},
		{"transcript before 0.26", "0.25.0", "CODEX_THREAD_ID", "codex", transcriptSessionOne, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			config := "[close.transcript]\nenabled = true\n"
			if tc.agent == "pi" {
				config += "[close.transcript.session_id_env]\npi = \"PI_SESSION_ID\"\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600))
			for _, name := range []string{"CODEX_THREAD_ID", "PI_SESSION_ID", "CODEX_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "KATA_TRANSCRIPT_AGENT", "KATA_TRANSCRIPT_SESSION_ID"} {
				t.Setenv(name, "")
			}
			t.Setenv(tc.variable, tc.id)
			var sent map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects/resolve":
					_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
				case "/api/v1/health":
					_, _ = fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, tc.version)
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
			if tc.attach {
				expected := map[string]any{"agent": tc.agent, "session_id": tc.id}
				assert.Equal(t, expected, sent["transcript"])
				assert.Empty(t, stderr)
			} else {
				assert.NotContains(t, sent, "transcript")
				assert.Contains(t, stderr, "reports "+tc.version)
				assert.Contains(t, stderr, "skipping attachment")
			}
		})
	}
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
