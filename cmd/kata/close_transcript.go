package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/transcript"
)

func closeTranscript(cmd *cobra.Command) *transcript.Transcript {
	cfg, err := config.ReadCloseTranscriptConfig()
	warn := func(message string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message) }
	if err != nil {
		warn(fmt.Sprintf("configuration unavailable (%v); skipping attachment", err))
		return nil
	}
	if !cfg.Enabled {
		return nil
	}
	agent, id := os.Getenv("KATA_TRANSCRIPT_AGENT"), os.Getenv("KATA_TRANSCRIPT_SESSION_ID")
	if agent == "" && id == "" {
		var ok bool
		if agent, id, ok = currentSession(); !ok {
			warn("ambiguous current session; skipping attachment")
			return nil
		}
	}
	ref := &transcript.Transcript{Agent: agent, SessionID: id}
	if err := ref.Validate(); err != nil {
		warn("current session unavailable or invalid; skipping attachment")
		return nil
	}
	// Preserve the exact harness UUID: no filesystem path, transcript body,
	// hook ownership hash, or another session is used as a fallback.
	if cfg.AgentsViewURL != "" {
		link, err := ref.Link(cfg.AgentsViewURL)
		if err != nil {
			warn("AgentsView URL invalid; attaching the session identifier only")
		} else {
			ref.URL = link
		}
	}
	return ref
}

// currentSession reads the invoking harness's session. Child agents inherit
// their parent's environment, so context from both harnesses is ambiguous.
func currentSession() (agent, id string, ok bool) {
	thread, session := os.Getenv("CODEX_THREAD_ID"), os.Getenv("CODEX_SESSION_ID")
	if thread != "" && session != "" && !strings.EqualFold(thread, session) {
		return "", "", false
	}
	codex, claude := cmp.Or(thread, session), os.Getenv("CLAUDE_CODE_SESSION_ID")
	switch {
	case codex != "" && claude != "":
		return "", "", false
	case claude != "":
		return "claude", claude, true
	default:
		return "codex", codex, true
	}
}

// dropUnsupportedTranscript keeps provenance optional: older daemons reject
// the unknown field, so the close proceeds without it instead of failing.
func dropUnsupportedTranscript(
	ctx context.Context, cmd *cobra.Command, client *http.Client, baseURL string, body map[string]any,
) error {
	err := requireDaemonAPIVersion(ctx, client, baseURL, apiVersionCloseTranscript, "close transcript")
	if err == nil {
		return nil
	}
	tooOld, ok := errors.AsType[*cliError](err)
	if !ok || tooOld.Code != "daemon_api_too_old" {
		return err
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+tooOld.Message+"; skipping attachment")
	delete(body, "transcript")
	return nil
}
