package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/transcript"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func prepareCloseTranscript(ctx context.Context, cmd *cobra.Command, httpClient *http.Client, client *kataclient.Client, baseURL string, pid int64, issue resolvedIssueRef, includeDeleted bool) *transcript.Transcript {
	cfg, err := config.ReadCloseTranscriptConfig()
	warn := func(message string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message) }
	if err != nil {
		warn(fmt.Sprintf("configuration unavailable (%v); skipping attachment", err))
		return nil
	}
	name := issue.ProjectName
	if len(cfg.Project) > 0 {
		response, callErr := client.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
			PathParams: &generated.ShowIssuePath{ProjectID: pid, Ref: issue.RefForAPI},
			Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
		})
		if response == nil || externalCLIResponseError(response.StatusCode, response.Body, callErr) != nil {
			warn("project unavailable; skipping attachment")
			return nil
		}
		var out api.MutationResponse
		if json.Unmarshal(response.Body, &out.Body) != nil || out.Body.Issue.ProjectID <= 0 {
			warn("project unavailable; skipping attachment")
			return nil
		}
		name, err = moveSourceProjectName(ctx, httpClient, baseURL, pid, out.Body.Issue.ProjectID, name)
		if err != nil {
			warn("project unavailable; skipping attachment")
			return nil
		}
	}
	return closeTranscript(cmd, cfg, cfg.Target(name))
}

func closeTranscript(cmd *cobra.Command, cfg config.CloseTranscriptConfig, target config.CloseTranscriptTarget) *transcript.Transcript {
	warn := func(message string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message) }
	if target.Enabled == nil || !*target.Enabled {
		return nil
	}
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
	// Preserve the exact harness session ID: no filesystem path, transcript body,
	// hook ownership hash, or another session is used as a fallback.
	if target.AgentsViewURL != "" {
		link, err := ref.Link(target.AgentsViewURL)
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
	ctx context.Context, cmd *cobra.Command, client *http.Client, baseURL string, body map[string]any,
) error {
	ref, ok := body["transcript"].(*transcript.Transcript)
	if !ok || ref == nil {
		return nil
	}
	required := apiVersionOtherAgentCloseTranscript
	if ref.Agent == "claude" || ref.Agent == "codex" {
		required = apiVersionCloseTranscript
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
