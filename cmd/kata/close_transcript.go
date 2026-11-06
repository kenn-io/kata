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

type closeTranscriptAttachment struct {
	ref       *transcript.Transcript
	target    config.CloseTranscriptTarget
	projectID int64
}

func warnCloseTranscript(cmd *cobra.Command, message string) {
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message)
}

func prepareCloseTranscript(ctx context.Context, cmd *cobra.Command, client *kataclient.Client, pid int64, issue resolvedIssueRef) closeTranscriptAttachment {
	cfg, err := config.ReadCloseTranscriptConfig()
	if err != nil {
		warnCloseTranscript(cmd, fmt.Sprintf("configuration unavailable (%v); skipping attachment", err))
		return closeTranscriptAttachment{}
	}
	target := cfg.Target(issue.ProjectName)
	selectedPID := pid
	if len(cfg.Project) > 0 {
		name, actualPID, err := closeTranscriptProject(ctx, client, pid, issue)
		if err != nil {
			warnCloseTranscript(cmd, "project unavailable; attaching the session identifier only")
			target = config.CloseTranscriptTarget{Enabled: &cfg.Enabled}
			selectedPID = 0
		} else {
			target = cfg.Target(name)
			selectedPID = actualPID
		}
	}
	return closeTranscriptAttachment{ref: closeTranscript(cmd, cfg, target), target: target, projectID: selectedPID}
}

// Historical references can resolve to an issue that has moved projects.
// Failure is optional provenance failure, never a new close prerequisite.
func closeTranscriptProject(ctx context.Context, client *kataclient.Client, pid int64, issue resolvedIssueRef) (string, int64, error) {
	response, callErr := client.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: pid, Ref: issue.RefForAPI}})
	if response == nil {
		return "", 0, errors.New("issue unavailable")
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return "", 0, err
	}
	var out api.MutationResponse
	if err := json.Unmarshal(response.Body, &out.Body); err != nil {
		return "", 0, err
	}
	actualPID := out.Body.Issue.ProjectID
	if actualPID <= 0 {
		return "", 0, errors.New("issue project unavailable")
	}
	if actualPID == pid {
		return issue.ProjectName, pid, nil
	}
	project, callErr := client.ShowProjectWithResponse(ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: actualPID}})
	if project == nil {
		return "", 0, errors.New("project unavailable")
	}
	if err := externalCLIResponseError(project.StatusCode, project.Body, callErr); err != nil {
		return "", 0, err
	}
	var named struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal(project.Body, &named); err != nil {
		return "", 0, err
	}
	if named.Project.Name == "" {
		return "", 0, errors.New("project name unavailable")
	}
	return named.Project.Name, actualPID, nil
}

func closeTranscript(cmd *cobra.Command, cfg config.CloseTranscriptConfig, target config.CloseTranscriptTarget) *transcript.Transcript {
	warn := func(message string) { warnCloseTranscript(cmd, message) }
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

// confirmCloseTranscript grants upload only for this invocation's new event.
// URL is deliberately checked: it is not part of keyed retry identity.
func confirmCloseTranscript(ref *transcript.Transcript, projectID int64, response []byte) error {
	var receipt api.MutationResponse
	if err := json.Unmarshal(response, &receipt.Body); err != nil {
		return errors.New("close confirmation unavailable")
	}
	event := receipt.Body.Event
	if !receipt.Body.Changed || receipt.Body.Reused || event == nil || event.Type != "issue.closed" {
		return errors.New("no new close event")
	}
	if projectID <= 0 || receipt.Body.Issue.ProjectID != projectID || event.ProjectID != projectID {
		return errors.New("project changed; recorded locator may be stale")
	}
	if event.IssueUID == nil || *event.IssueUID != receipt.Body.Issue.UID || receipt.Body.Issue.UID == "" {
		return errors.New("close issue confirmation unavailable")
	}
	var payload struct {
		Transcript *transcript.Transcript `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil || ref == nil || payload.Transcript == nil || *payload.Transcript != *ref {
		return errors.New("close transcript confirmation unavailable")
	}
	return nil
}
