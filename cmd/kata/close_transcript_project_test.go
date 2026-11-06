package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/transcript"
)

func transcriptProjectConfig(t *testing.T, text string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_TRANSCRIPT_AGENT", "codex")
	t.Setenv("KATA_TRANSCRIPT_SESSION_ID", transcriptSessionOne)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(text), 0600))
}

func closeReceiptTranscript(t *testing.T, output string) *transcript.Transcript {
	t.Helper()
	var receipt api.MutationResponse
	require.NoError(t, json.Unmarshal([]byte(output), &receipt.Body))
	require.NotNil(t, receipt.Body.Event)
	var payload struct {
		Transcript *transcript.Transcript `json:"transcript"`
	}
	require.NoError(t, json.Unmarshal([]byte(receipt.Body.Event.Payload), &payload))
	return payload.Transcript
}

func TestCloseTranscriptProjectSelection(t *testing.T) {
	for _, selector := range []string{"workspace", "flag", "qualified", "moved"} {
		t.Run(selector, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete project work")
			team, err := env.DB.CreateProject(t.Context(), "team-project")
			require.NoError(t, err)
			args := []string{}
			project := "team-project"
			switch selector {
			case "workspace":
				project = "kata"
			case "flag":
				ref = createIssue(t, env, team.ID, "Team work")
				args = []string{"--project", team.Name}
			case "qualified":
				ref = team.Name + "#" + createIssue(t, env, team.ID, "Qualified work")
				args = []string{"--project", "kata"}
			case "moved":
				runCLI(t, env, dir, "move", ref, team.Name)
				ref = "kata#" + ref
			}
			transcriptProjectConfig(t, `[close.transcript]
enabled=true
agentsview_url="https://default.example"
[close.transcript.project.kata]
agentsview_url="https://workspace.example"
[close.transcript.project.team-project]
agentsview_url="https://team.example/archive"
`)
			args = append(args, "--json", "close", ref, "--done", "--message", "Implemented project behavior and checked the focused tests.", "--test", "go test ./internal/example")
			out, stderr, err := runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			require.Empty(t, stderr)
			got := closeReceiptTranscript(t, out)
			require.NotNil(t, got)
			base := "https://team.example/archive"
			if project == "kata" {
				base = "https://workspace.example"
			}
			assert.Equal(t, base+"/sessions/codex/"+transcriptSessionOne, got.URL)
		})
	}
}

func TestCloseTranscriptProjectOverride(t *testing.T) {
	for _, tc := range []struct {
		name, entry    string
		global, attach bool
		url            string
	}{
		{"inherit", `agentsview_url="https://team.example"`, true, true, "https://team.example/sessions/codex/" + transcriptSessionOne},
		{"enable", `enabled=true`, false, true, ""},
		{"disable", `enabled=false`, true, false, ""},
		{"blank URL", `agentsview_url=""`, true, true, ""},
		{"invalid URL", `agentsview_url="https://user:secret@team.example"`, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
			transcriptProjectConfig(t, fmt.Sprintf("[close.transcript]\nenabled=%t\nagentsview_url=\"https://default.example\"\n[close.transcript.project.kata]\n%s\n", tc.global, tc.entry))
			out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref, "--done", "--message", "Implemented behavior and ran the example package tests.", "--test", "go test ./internal/example")
			require.NoError(t, err)
			got := closeReceiptTranscript(t, out)
			if tc.attach {
				require.NotNil(t, got)
				assert.Equal(t, tc.url, got.URL)
			} else {
				assert.Nil(t, got)
			}
			assert.NotContains(t, stderr, "secret")
		})
	}
}

func transcriptConfirmation(t *testing.T, edit func(*api.MutationResponse)) []byte {
	t.Helper()
	var receipt api.MutationResponse
	receipt.Body.Issue = db.Issue{UID: "example-issue", ProjectID: 7}
	payload, err := json.Marshal(map[string]any{"transcript": transcript.Transcript{Agent: "codex", SessionID: transcriptSessionOne, URL: "https://team.example/sessions/codex/" + transcriptSessionOne}})
	require.NoError(t, err)
	receipt.Body.Changed = true
	receipt.Body.Event = &db.Event{Type: "issue.closed", ProjectID: 7, IssueUID: new("example-issue"), Payload: string(payload)}
	if edit != nil {
		edit(&receipt)
	}
	bs, err := json.Marshal(receipt.Body)
	require.NoError(t, err)
	return bs
}

func TestCloseTranscriptConfirmation(t *testing.T) {
	ref := &transcript.Transcript{Agent: "codex", SessionID: transcriptSessionOne, URL: "https://team.example/sessions/codex/" + transcriptSessionOne}
	require.NoError(t, confirmCloseTranscript(ref, 7, transcriptConfirmation(t, nil)))
	for _, tc := range []struct {
		name string
		edit func(*api.MutationResponse)
	}{
		{"no-op or dry", func(r *api.MutationResponse) { r.Body.Changed = false }},
		{"keyed replay", func(r *api.MutationResponse) {
			r.Body.Reused = true
			r.Body.OriginalEvent = r.Body.Event
			r.Body.Event = nil
		}},
		{"missing event", func(r *api.MutationResponse) { r.Body.Event = nil }},
		{"different issue project", func(r *api.MutationResponse) { r.Body.Issue.ProjectID = 8 }},
		{"different event project", func(r *api.MutationResponse) { r.Body.Event.ProjectID = 8 }},
		{"different issue", func(r *api.MutationResponse) { r.Body.Event.IssueUID = new("another-issue") }},
		{"wrong event", func(r *api.MutationResponse) { r.Body.Event.Type = "issue.reopened" }},
		{"missing transcript", func(r *api.MutationResponse) { r.Body.Event.Payload = "{}" }},
		{"different session", func(r *api.MutationResponse) {
			r.Body.Event.Payload = fmt.Sprintf(`{"transcript":{"agent":"codex","session_id":%q,"url":%q}}`, transcriptSessionTwo, ref.URL)
		}},
		{"different locator", func(r *api.MutationResponse) {
			r.Body.Event.Payload = fmt.Sprintf(`{"transcript":{"agent":"codex","session_id":%q,"url":"https://other.example"}}`, ref.SessionID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, confirmCloseTranscript(ref, 7, transcriptConfirmation(t, tc.edit)))
		})
	}
	require.Error(t, confirmCloseTranscript(ref, 7, []byte(`malformed response`)))
}

func TestCloseTranscriptProjectLookupIsOptional(t *testing.T) {
	for _, tc := range []struct {
		name            string
		global, project bool
	}{
		{"global enabled", true, true},
		{"unknown project override cannot disable global identifier fallback", true, false},
		{"global disabled is strict opt-in", false, true},
		{"disabled everywhere", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
			transcriptProjectConfig(t, fmt.Sprintf(`[close.transcript]
enabled=%t
agentsview_url="https://default.example"
[close.transcript.project.kata]
enabled=%t
agentsview_url="https://team.example"
`, tc.global, tc.project))
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/"+ref) {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			original := env.URL
			env.URL = server.URL
			defer func() { env.URL = original }()
			out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example")
			require.NoError(t, err)
			got := closeReceiptTranscript(t, out)
			if tc.global {
				require.NotNil(t, got)
				assert.Empty(t, got.URL)
			} else {
				assert.Nil(t, got)
			}
			assert.Contains(t, stderr, "project unavailable")

		})
	}
}
