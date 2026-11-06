package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/transcript"
	"hegel.dev/go/hegel"
)

// This fixture exits before TestMain, without opening any application source.
func init() {
	if len(os.Args) < 3 || os.Args[1] != "kata-transcript-push-fixture" {
		return
	}
	capture := struct{ Args, Env []string }{os.Args[3:], os.Environ()}
	data, err := json.Marshal(capture)
	if err != nil {
		os.Exit(9)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "capture.json"), data, 0600); err != nil { // #nosec G703 -- the parent supplies a synthetic t.TempDir archive to this test-only executable.
		os.Exit(9)
	}
	_, _ = fmt.Fprintln(os.Stdout, "fixture-secret-session-content")
	_, _ = fmt.Fprintln(os.Stderr, "fixture-only-token")
	switch os.Args[2] {
	case "fail":
		os.Exit(4)
	case "hang":
		waitForTranscriptPushFixtureSignal()
	case "descendant-holder":
		path := filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "descendant.pid")
		if err := os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0600); err != nil { // #nosec G703 -- path is inside the parent's synthetic fixture archive.
			os.Exit(9)
		}
		waitForTranscriptPushFixtureSignal()
	case "descendant-hang", "descendant-exit":
		parent := filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "parent.pid")
		if err := os.WriteFile(parent, []byte(fmt.Sprint(os.Getpid())), 0600); err != nil { // #nosec G703 -- parent is a PID marker inside the parent's synthetic fixture archive.
			os.Exit(9)
		}
		// #nosec G204,G702 -- execute this same test binary with a fixed fixture mode, never an archive-supplied command.
		child := exec.Command(os.Args[0], "kata-transcript-push-fixture", "descendant-holder") //nolint:noctx // The fixture intentionally holds its parent's output handles until tree cleanup.
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		// Do not exit before the descendant has started and exposed its PID.
		ready := filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "descendant.pid")
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil { // #nosec G703 -- readiness is a fixed marker within the parent's synthetic fixture directory.
				break
			}
			if time.Now().After(deadline) {
				_ = child.Process.Kill()
				_ = child.Wait()
				os.Exit(9)
			}
			time.Sleep(10 * time.Millisecond) //nolint:kennlint // sleeptest: a separate OS process publishes readiness through this file; synctest cannot advance it.
		}
		if os.Args[2] == "descendant-hang" {
			waitForTranscriptPushFixtureSignal()
		}
	}
	os.Exit(0)
}

func waitForTranscriptPushFixtureSignal() {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	<-interrupt
}

func transcriptPushFixtureTarget() config.CloseTranscriptTarget {
	return config.CloseTranscriptTarget{AgentsViewURL: "https://team.example/archive", TokenEnv: "TEAM_UPLOAD_TOKEN", Push: true} // #nosec G101 -- TokenEnv is the name of a synthetic fixture variable, not a credential.
}

func transcriptPushFixture(t *testing.T, mode string) (*int, **exec.Cmd) {
	t.Helper()
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("TEAM_UPLOAD_TOKEN", "fixture-only-token")
	original := closeTranscriptPushCommand
	calls := 0
	var command *exec.Cmd
	closeTranscriptPushCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		require.Equal(t, "agentsview", name)
		calls++
		command = exec.CommandContext(ctx, os.Args[0], append([]string{"kata-transcript-push-fixture", mode}, args...)...) // #nosec G204,G702 -- current test binary, selected fixture mode and captured argv only; no real AgentsView process.
		return command
	}
	t.Cleanup(func() { closeTranscriptPushCommand = original })
	return &calls, &command
}

func nativePushTranscript(t *testing.T, agent string) transcript.Transcript {
	t.Helper()
	ref := transcript.Transcript{Agent: agent, SessionID: transcriptSessionOne}
	var err error
	ref.URL, err = ref.Link("https://team.example/archive")
	require.NoError(t, err)
	return ref
}

func TestCloseTranscriptPushExactSession(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi"} {
		t.Run(agent, func(t *testing.T) {
			calls, _ := transcriptPushFixture(t, "success")
			t.Setenv("KATA_AUTH_TOKEN", "fixture-kata-token")
			t.Setenv("OPENAI_API_KEY", "fixture-provider-token")
			target := transcriptPushFixtureTarget()
			ref := nativePushTranscript(t, agent)
			require.NoError(t, runCloseTranscriptPush(t.Context(), target, ref))
			require.Equal(t, 1, *calls)
			data, err := os.ReadFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "capture.json")) // #nosec G304,G703 -- read only the capture written inside transcriptPushFixture's t.TempDir archive.
			require.NoError(t, err)
			var capture struct{ Args, Env []string }
			require.NoError(t, json.Unmarshal(data, &capture))
			canonical := ref.SessionID
			if agent != "claude" {
				canonical = agent + ":" + canonical
			}
			assert.Equal(t, []string{"push", canonical, "--to", target.AgentsViewURL, "--agent", agent, "--token-env", "TEAM_UPLOAD_TOKEN", "--json"}, capture.Args)
			assert.NotContains(t, strings.Join(capture.Args, " "), "fixture-only-token")
			assert.Contains(t, capture.Env, "TEAM_UPLOAD_TOKEN=fixture-only-token")
			for _, entry := range capture.Env {
				assert.False(t, strings.HasPrefix(entry, "KATA_"))
				assert.False(t, strings.HasPrefix(entry, "OPENAI_API_KEY="))
				assert.False(t, strings.HasPrefix(entry, "CODEX_HOME="))
			}
		})
	}
}

func TestCloseTranscriptPushRejectsUnsafeOrMissingSettings(t *testing.T) {
	for _, tc := range []struct{ name, url, token string }{
		{"no selected token", "https://team.example/archive", ""},
		{"missing selected token", "https://team.example/archive", "MISSING_UPLOAD_TOKEN"},
		{"Kata token", "https://team.example/archive", "KATA_AUTH_TOKEN"},
		{"HTTP locator", "http://team.example/archive", "TEAM_UPLOAD_TOKEN"},
		{"URL credentials", "https://user:secret@team.example/archive", "TEAM_UPLOAD_TOKEN"},
		{"changed destination", "https://other.example/archive", "TEAM_UPLOAD_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, _ := transcriptPushFixture(t, "success")
			t.Setenv("AGENTSVIEW_PUSH_TOKEN", "ambient-token-must-not-be-used")
			t.Setenv("MISSING_UPLOAD_TOKEN", "")
			t.Setenv("KATA_AUTH_TOKEN", "fixture-kata-token")
			err := runCloseTranscriptPush(t.Context(), config.CloseTranscriptTarget{AgentsViewURL: tc.url, TokenEnv: tc.token, Push: true}, nativePushTranscript(t, "codex"))
			require.Error(t, err)
			assert.Equal(t, 0, *calls)
			assert.NotContains(t, err.Error(), "secret")
			assert.NotContains(t, err.Error(), "fixture-kata-token")
		})
	}
}

func TestCloseTranscriptPushCancellationReapsChild(t *testing.T) {
	_, command := transcriptPushFixture(t, "hang")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := runCloseTranscriptPush(ctx, transcriptPushFixtureTarget(), nativePushTranscript(t, "codex"))
	require.Error(t, err)
	require.NotNil(t, *command)
	require.NotNil(t, (*command).ProcessState)
	assert.ErrorIs(t, (*command).Process.Kill(), os.ErrProcessDone)
	_, err = os.Stat(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "capture.json")) // #nosec G703 -- fixed capture marker in transcriptPushFixture's synthetic archive directory.
	require.NoError(t, err, "the fixture started before cancellation")
}

func TestCloseTranscriptPushCanonicalIDPreservesNativeID(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		agent := hegel.Draw(ht, hegel.SampledFrom([]string{"claude", "codex", "pi"}))
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", hegel.Draw(ht, hegel.Integers[int64](0, 1<<32-1)), hegel.Draw(ht, hegel.Integers[int64](0, 1<<48-1)))
		ref := transcript.Transcript{Agent: agent, SessionID: id}
		want := id
		if agent != "claude" {
			want = agent + ":" + id
		}
		if got := agentsViewPushSessionID(ref); got != want || ref.SessionID != id {
			ht.Fatalf("canonical conversion changed native session identity")
		}
	}, hegel.WithTestCases(2000))
}

const pushProjectConfig = `[close.transcript]
enabled=true
agentsview_url="https://default.example"
[close.transcript.project.kata]
agentsview_url="https://team.example/archive"
push=true
token_env="TEAM_UPLOAD_TOKEN"
`

func TestCloseTranscriptPushCloseLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "fail"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete scoped work")
			transcriptProjectConfig(t, pushProjectConfig)
			calls, _ := transcriptPushFixture(t, mode)
			args := []string{"--json", "close", ref, "--done", "--message", "Implemented behavior and ran the focused example tests.", "--test", "go test ./internal/example"}
			_, _, err := runCLIWithErr(t, env, dir, append(args, "--dry")...)
			require.NoError(t, err)
			assert.Zero(t, *calls)
			out, stderr, err := runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			assert.Equal(t, 1, *calls)
			require.NotNil(t, closeReceiptTranscript(t, out))
			if mode == "success" {
				assert.Contains(t, stderr, "upload accepted")
			} else {
				assert.Contains(t, stderr, "URL-only")
				assert.Contains(t, stderr, "unconfirmed")
			}
			assert.NotContains(t, out+stderr, "fixture-only-token")
			assert.NotContains(t, out+stderr, "fixture-secret-session-content")
			_, _, err = runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			assert.Equal(t, 1, *calls, "no-op closes never push")
		})
	}
}

func TestCloseTranscriptPushKeyedReplayNeverUploads(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete retryable work")
	transcriptProjectConfig(t, pushProjectConfig)
	calls, _ := transcriptPushFixture(t, "success")
	args := []string{"--json", "close", ref, "--done", "--message", "Implemented retryable behavior and checked the focused tests.", "--test", "go test ./internal/example", "--idempotency-key", "close-fixture-key"}
	_, _, err := runCLIWithErr(t, env, dir, args...)
	require.NoError(t, err)
	require.Equal(t, 1, *calls)
	// URL changes are deliberately outside close retry identity.
	path := filepath.Join(os.Getenv("KATA_HOME"), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(pushProjectConfig, "https://team.example/archive", "https://other.example/archive")), 0600)) // #nosec G703 -- path belongs to transcriptProjectConfig's t.TempDir home.
	_, _, err = runCLIWithErr(t, env, dir, args...)
	require.NoError(t, err)
	assert.Equal(t, 1, *calls)
}

func TestCloseTranscriptPushMissingExecutable(t *testing.T) {
	original := closeTranscriptPushCommand
	closeTranscriptPushCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-agentsview"), args...) // #nosec G204 -- deliberately nonexistent executable beneath a synthetic test directory.
	}
	t.Cleanup(func() { closeTranscriptPushCommand = original })
	t.Setenv("TEAM_UPLOAD_TOKEN", "fixture-only-token")
	err := runCloseTranscriptPush(t.Context(), transcriptPushFixtureTarget(), nativePushTranscript(t, "codex"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.Canceled))
}

func TestCloseTranscriptPushBeforeFollowupCommentFailure(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Complete commented work")
	transcriptProjectConfig(t, pushProjectConfig)
	calls, _ := transcriptPushFixture(t, "success")
	upstream, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			http.Error(w, "fixture comment unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	original := env.URL
	env.URL = server.URL
	defer func() { env.URL = original }()
	_, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example", "--comment", "Example follow-up comment")
	require.Error(t, err)
	assert.Equal(t, 1, *calls, "upload follows the close commit before a separate comment can fail")
	assert.Contains(t, stderr, "upload accepted")
	data := env.RequireOK(t, fmt.Sprintf("/api/v1/projects/%d/issues/%s", pid, ref))
	var stored struct {
		Issue struct {
			Status string `json:"status"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(data, &stored))
	assert.Equal(t, "closed", stored.Issue.Status)
}

func TestCloseTranscriptPushNoDisclosure(t *testing.T) {
	for _, scenario := range []string{"refused", "no evidence", "missing session", "project disabled", "global disabled", "push off"} {
		t.Run(scenario, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
			cfg := pushProjectConfig
			if scenario == "project disabled" {
				cfg += "enabled=false\n"
			}
			if scenario == "global disabled" {
				cfg = strings.Replace(cfg, "enabled=true", "enabled=false", 1)
			}
			if scenario == "push off" {
				cfg = strings.ReplaceAll(cfg, "push=true", "push=false")
			}
			transcriptProjectConfig(t, cfg)
			calls, _ := transcriptPushFixture(t, "success")
			args := []string{"--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example"}
			if scenario == "refused" {
				args = []string{"--json", "close", ref, "--done", "--message", "too short", "--test", "go test ./internal/example"}
			}
			if scenario == "no evidence" {
				args = []string{"--json", "close", ref, "--wontfix", "--message", "This example behavior is intentionally left unchanged because it is outside the supported feature scope."}
			}
			if scenario == "missing session" {
				t.Setenv("KATA_TRANSCRIPT_SESSION_ID", "")
			}
			out, _, err := runCLIWithErr(t, env, dir, args...)
			if scenario == "refused" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if scenario == "project disabled" || scenario == "global disabled" {
				assert.Nil(t, closeReceiptTranscript(t, out))
			}
			assert.Zero(t, *calls)
		})
	}
}

func TestCloseTranscriptPushOldDaemonNeverUploads(t *testing.T) {
	calls, _ := transcriptPushFixture(t, "success")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/health", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.25.0"}`)
	}))
	defer server.Close()
	ref := nativePushTranscript(t, "codex")
	body := map[string]any{"transcript": &ref}
	var stderr strings.Builder
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	require.NoError(t, dropUnsupportedTranscript(t.Context(), cmd, server.Client(), server.URL, body))
	attachment := closeTranscriptAttachment{ref: &ref, target: transcriptPushFixtureTarget(), projectID: 7}
	attachment.finish(t.Context(), cmd, body, transcriptConfirmation(t, nil))
	assert.Zero(t, *calls)
}
