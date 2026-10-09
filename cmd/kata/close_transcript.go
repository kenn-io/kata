package main

import (
	"fmt"
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
		warn("configuration unavailable; skipping attachment")
		return nil
	}
	if !cfg.Enabled {
		return nil
	}
	agent, id := os.Getenv("KATA_TRANSCRIPT_AGENT"), os.Getenv("KATA_TRANSCRIPT_SESSION_ID")
	if agent == "" && id == "" {
		agent = "codex"
		thread, session := os.Getenv("CODEX_THREAD_ID"), os.Getenv("CODEX_SESSION_ID")
		if thread != "" && session != "" && !strings.EqualFold(thread, session) {
			warn("ambiguous current session; skipping attachment")
			return nil
		}
		id = thread
		if id == "" {
			id = session
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
