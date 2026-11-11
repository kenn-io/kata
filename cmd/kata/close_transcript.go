package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/transcript"
)

func prepareCloseTranscript(ctx context.Context, cmd *cobra.Command, baseURL string, pid int64, issue resolvedIssueRef, includeDeleted bool) (*transcript.Transcript, *api.TranscriptSelection, bool) {
	cfg, err := config.ReadCloseTranscriptConfig()
	warn := func(message string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message) }
	if err != nil {
		warn(fmt.Sprintf("configuration unavailable (%v); skipping attachment", err))
		return nil, nil, false
	}
	if !cfg.AnyEnabled() {
		return nil, nil, false
	}
	ref := closeTranscript(cmd, cfg)
	if ref == nil {
		return nil, nil, false
	}
	target := cfg.Target(issue.ProjectName)
	var selection *api.TranscriptSelection
	unconfigured := false
	if len(cfg.Project) > 0 {
		hydrated, err := hydrateRefWithQualified(ctx, baseURL, pid, issue, includeDeleted)
		if err != nil {
			warn("project unavailable; using process override for attachment")
			return ref, &api.TranscriptSelection{Record: cfg.ForceEnabled()}, false
		}
		_, configured := cfg.Project[hydrated.ProjectName]
		unconfigured = !configured
		target = cfg.Target(hydrated.ProjectName)
		selection = &api.TranscriptSelection{ProjectID: hydrated.CurrentProjectID, Record: *target.Enabled}
	}
	if target.AgentsViewURL != "" && (selection == nil || selection.Record) {
		link, err := ref.Link(target.AgentsViewURL)
		if err != nil {
			warn("AgentsView URL invalid; attaching the session identifier only")
		} else {
			ref.URL = link
		}
	}
	return ref, selection, unconfigured
}

func closeTranscript(cmd *cobra.Command, cfg config.CloseTranscriptConfig) *transcript.Transcript {
	warn := func(message string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message) }
	agent, id := os.Getenv("KATA_TRANSCRIPT_AGENT"), os.Getenv("KATA_TRANSCRIPT_SESSION_ID")
	var source string
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
			if id != "" && (agent != variable.agent || id != value) {
				warn("ambiguous current session; skipping attachment")
				return nil
			}
			if id == "" {
				agent, id = variable.agent, value
				source = variable.name
			}
		}
	}
	ref := &transcript.Transcript{Agent: agent, SessionID: id}
	if err := ref.Validate(); err != nil {
		if source != "" {
			warn(fmt.Sprintf("current session from %s invalid; skipping attachment", source))
		} else {
			warn("current session unavailable or invalid; skipping attachment")
		}
		return nil
	}
	return ref
}

// dropUnsupportedTranscript keeps provenance optional when a daemon predates
// the supplied transcript shape.
func dropUnsupportedTranscript(
	ctx context.Context, cmd *cobra.Command, client *http.Client, baseURL string, body map[string]any, unconfigured bool,
) error {
	ref, ok := body["transcript"].(*transcript.Transcript)
	if !ok || ref == nil {
		return nil
	}
	required := apiVersionOtherAgentCloseTranscript
	if ref.Agent == "claude" || ref.Agent == "codex" {
		required = apiVersionCloseTranscript
	}
	if body["transcript_selection"] != nil {
		err := requireDaemonAPIVersion(ctx, client, baseURL, apiVersionTranscriptSelection, "close transcript")
		if err == nil {
			return nil
		}
		tooOld, ok := errors.AsType[*cliError](err)
		if !ok || tooOld.Code != "daemon_api_too_old" {
			return err
		}
		if !unconfigured {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+tooOld.Message+"; skipping attachment")
			delete(body, "transcript")
			delete(body, "transcript_selection")
			return nil
		}
		delete(body, "transcript_selection")
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
	delete(body, "transcript_selection")
	return nil
}
