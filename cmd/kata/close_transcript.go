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
	agent, id := os.Getenv("KATA_TRANSCRIPT_AGENT"), os.Getenv("KATA_TRANSCRIPT_SESSION_ID")
	if agent == "" && id == "" {
		variables := []struct{ agent, name string }{
			{"claude", "CLAUDE_CODE_SESSION_ID"},
			{"codex", "CODEX_THREAD_ID"},
			{"codex", "CODEX_SESSION_ID"},
		}
		for configuredAgent, name := range cfg.SessionIDEnv {
			variables = append(variables, struct{ agent, name string }{configuredAgent, name})
		}
		for _, variable := range variables {
			value := os.Getenv(variable.name)
			if value == "" {
				continue
			}
			if err := (transcript.Transcript{Agent: variable.agent, SessionID: value}).Validate(); err != nil {
				warn(fmt.Sprintf("current session from %s invalid; skipping attachment", variable.name))
				return nil
			}
			if id != "" && (agent != variable.agent || !strings.EqualFold(id, value)) {
				warn("ambiguous current session; skipping attachment")
				return nil
			}
			if id == "" {
				agent, id = variable.agent, value
			}
		}
	}
	ref := &transcript.Transcript{Agent: agent, SessionID: id}
	if err := ref.Validate(); err != nil {
		warn("current session unavailable or invalid; skipping attachment")
		return nil
	}
	// Preserve the exact harness session ID: no filesystem path, transcript body,
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

// dropUnsupportedTranscript keeps provenance optional when a daemon predates
// the supplied transcript shape.
func dropUnsupportedTranscript(
	ctx context.Context, cmd *cobra.Command, client *http.Client, baseURL string, ref *transcript.Transcript, body map[string]any,
) error {
	required := apiVersionCloseTranscript
	if ref.Agent != "claude" && ref.Agent != "codex" {
		required = apiVersionOtherAgentCloseTranscript
	}
	err := requireDaemonAPIVersion(ctx, client, baseURL, required, "close transcript")
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
