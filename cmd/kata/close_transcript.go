package main

import (
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
	id := os.Getenv("KATA_TRANSCRIPT_SESSION_ID")
	if id == "" {
		for _, name := range cfg.SessionIDEnv {
			value := os.Getenv(name)
			if value == "" {
				continue
			}
			if id != "" && !strings.EqualFold(id, value) {
				warn("ambiguous current session; skipping attachment")
				return nil
			}
			id = value
		}
	}
	ref := &transcript.Transcript{SessionID: id}
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

// dropUnsupportedTranscript keeps provenance optional: older daemons cannot
// accept agentless transcripts, so the close proceeds without one.
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
