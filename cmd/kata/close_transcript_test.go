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

	"go.kenn.io/kata/internal/harness"
	"go.kenn.io/kata/internal/transcript"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const transcriptSessionOne = "00000000-0000-4000-8000-000000000001"
const transcriptSessionTwo = "00000000-0000-4000-8000-000000000002"

func TestCloseTranscript_OptIn(t *testing.T) {
	clearTranscriptEnv(t)
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
	clearTranscriptEnv(t)
	for _, tc := range []struct {
		name, agent, id, thread, session, claude, base, wantAgent, wantID, warning string
	}{
		{name: "no context", warning: "current session unavailable"},
		{name: "conflicting Codex context", thread: transcriptSessionOne, session: transcriptSessionTwo, warning: "ambiguous"},
		{name: "partial explicit session", id: transcriptSessionOne, thread: transcriptSessionTwo, warning: "current session unavailable"},
		{name: "partial explicit context", agent: "claude", thread: transcriptSessionOne, warning: "current session unavailable"},
		{name: "explicit Pi overrides inherited Codex", agent: "pi", id: transcriptSessionTwo, thread: transcriptSessionOne, wantAgent: "pi", wantID: transcriptSessionTwo},
		{name: "explicit Claude overrides inherited Codex", agent: "claude", id: transcriptSessionTwo, thread: transcriptSessionOne, wantAgent: "claude", wantID: transcriptSessionTwo},
		{name: "Claude Code context", claude: transcriptSessionOne, wantAgent: "claude", wantID: transcriptSessionOne},
		{name: "nested agent context", claude: transcriptSessionTwo, thread: transcriptSessionOne, warning: "ambiguous"},
		{name: "explicit pair resolves nested context", agent: "codex", id: transcriptSessionOne, claude: transcriptSessionTwo, thread: transcriptSessionTwo, wantAgent: "codex", wantID: transcriptSessionOne},
		{name: "session alias", session: transcriptSessionOne, wantAgent: "codex", wantID: transcriptSessionOne},
		{name: "no AgentsView integration", thread: transcriptSessionOne, wantAgent: "codex", wantID: transcriptSessionOne},
		{name: "bad link retains ID", thread: transcriptSessionOne, base: "https://user:secret@agentsview.example?token=secret", wantAgent: "codex", wantID: transcriptSessionOne, warning: "URL invalid"},
		{name: "path never persisted", agent: "codex", id: "/private/session/chat.jsonl", warning: "current session unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled=true\nagentsview_url="+fmt.Sprintf("%q", tc.base)+"\n"), 0600))
			for name, value := range map[string]string{"KATA_TRANSCRIPT_AGENT": tc.agent, "KATA_TRANSCRIPT_SESSION_ID": tc.id, "CODEX_THREAD_ID": tc.thread, "CODEX_SESSION_ID": tc.session, "CLAUDE_CODE_SESSION_ID": tc.claude} {
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

func TestCloseTranscript_OldDaemonClosesWithoutProvenance(t *testing.T) {
	clearTranscriptEnv(t)
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
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.25.0"}`))
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
	assert.Contains(t, stderr, "reports 0.25.0")
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

func clearTranscriptEnv(t *testing.T) {
	t.Helper()
	for _, key := range append(harness.EnvKeys(), "KATA_TRANSCRIPT_AGENT", "KATA_TRANSCRIPT_SESSION_ID") {
		t.Setenv(key, "")
	}
}

func TestCloseTranscriptPiAndOMP(t *testing.T) {
	for _, tc := range []struct{ agent, key, marker string }{
		{"pi", "PI_CODING_AGENT", "true"}, {"omp", "OMPCODE", "1"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			clearTranscriptEnv(t)
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Completed example work")
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled=true\n"), 0600))
			t.Setenv(tc.key, tc.marker)
			t.Setenv("PI_SESSION_ID", transcriptSessionOne)
			out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref, "--done",
				"--message", "Implemented the example behavior and ran the focused tests.", "--test", "go test ./internal/example")
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
			assert.Equal(t, map[string]any{"agent": tc.agent, "session_id": transcriptSessionOne}, payload["transcript"])
		})
	}
}

func TestCloseTranscriptOldDaemonDropsNewAgents(t *testing.T) {
	for _, tc := range []struct {
		agent, version string
		wantTranscript bool
	}{
		{"pi", "0.32.0", false}, {"omp", "0.32.0", false},
		{"pi", "0.33.0", true}, {"omp", "0.33.0", true},
		{"pi", "0.34.0", true}, {"omp", "0.34.0", true},
		{"claude", "0.25.0", false},
		{"codex", "0.32.0", true}, {"claude", "0.32.0", true},
		{"codex", "0.26.0", true}, {"claude", "0.26.0", true},
	} {
		t.Run(tc.agent+"/"+tc.version, func(t *testing.T) {
			clearTranscriptEnv(t)
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled=true\n"), 0600))
			t.Setenv("KATA_TRANSCRIPT_AGENT", tc.agent)
			t.Setenv("KATA_TRANSCRIPT_SESSION_ID", transcriptSessionOne)
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
			_, stderr, err := executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL),
				"--project", "example-project", "close", "abc1", "--done",
				"--message", "Implemented the example behavior and ran the focused tests.", "--test", "go test ./internal/example")
			require.NoError(t, err)
			require.NotNil(t, sent)
			assert.Equal(t, "done", sent["reason"])
			assert.Equal(t, "Implemented the example behavior and ran the focused tests.", sent["message"])
			if tc.wantTranscript {
				assert.Equal(t, map[string]any{"agent": tc.agent, "session_id": transcriptSessionOne}, sent["transcript"])
				assert.Empty(t, stderr)
			} else {
				assert.NotContains(t, sent, "transcript")
				if tc.agent == "pi" || tc.agent == "omp" {
					assert.Contains(t, stderr, "close: transcript: daemon too old for "+tc.agent+" transcripts; skipping attachment")
				} else {
					assert.Contains(t, stderr, "reports "+tc.version)
					assert.Contains(t, stderr, "skipping attachment")
				}
			}
		})
	}
}

func TestCloseTranscriptHealthFailurePreservesAttachment(t *testing.T) {
	for _, agent := range []string{"codex", "pi", "omp"} {
		for _, tc := range []struct {
			name, response string
			status         int
		}{
			{"unavailable", `{"error":"unavailable"}`, http.StatusServiceUnavailable},
			{"malformed", `{`, http.StatusOK},
		} {
			t.Run(agent+"/"+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.response))
				}))
				t.Cleanup(server.Close)
				ref := &transcript.Transcript{Agent: agent, SessionID: transcriptSessionOne}
				body := map[string]any{"transcript": ref}
				cmd := &cobra.Command{}
				var stderr bytes.Buffer
				cmd.SetErr(&stderr)
				err := dropUnsupportedTranscript(context.Background(), cmd, server.Client(), server.URL, body)
				require.Error(t, err)
				assert.Same(t, ref, body["transcript"])
				assert.Empty(t, stderr.String())
			})
		}
	}
}
