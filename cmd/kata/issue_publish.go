package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newIssuePublishCmd(provider string) *cobra.Command {
	var adopt, attempt, message string
	var resolve bool
	cmd := &cobra.Command{Use: "publish <issue-ref>", Short: "publish one native issue to " + issueSyncLabel(provider), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if (adopt != "" && resolve) || (resolve && (strings.TrimSpace(attempt) == "" || strings.TrimSpace(message) == "")) || (!resolve && (cmd.Flags().Changed("attempt") || cmd.Flags().Changed("message"))) {
			return &cliError{Message: "use --adopt or --resolve-not-created; resolution requires --attempt and --message", Kind: kindValidation, ExitCode: ExitValidation}
		}
		ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
		if err != nil {
			return err
		}
		hc, err := longRunningClientFor(ctx, baseURL)
		if err != nil {
			return err
		}
		a := daemonAPI{ctx: ctx, baseURL: baseURL, client: hc}
		client, err := kataclient.NewWithHTTPClient(baseURL, hc)
		if err != nil {
			return err
		}
		if err := requireIssuePublish(a); err != nil {
			return err
		}
		actor, _ := resolveActor(ctx, flags.As, nil)
		response, callErr := client.PublishIssueWithResponse(ctx, &generated.PublishIssueRequestOptions{PathParams: &generated.PublishIssuePath{ProjectID: pid, Provider: provider}, Body: &generated.PublishIssueBody{IssueRef: ref.RefForAPI, Actor: &actor, Adopt: &adopt, ResolveNotCreated: &resolve, Attempt: &attempt, Message: &message}})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), jsontext.Value(response.Body))
		}
		var result struct {
			Provider   string `json:"provider"`
			IssueRef   string `json:"issue_ref"`
			ExternalID string `json:"external_id"`
			Result     string `json:"result"`
			URL        string `json:"url"`
		}
		if err := json.Unmarshal(response.Body, &result); err != nil {
			return err
		}
		if currentOutputMode() == outputAgent {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "provider=%s issue_ref=%s external_id=%s result=%s url=%s\n", agentValue(result.Provider), agentValue(result.IssueRef), agentValue(result.ExternalID), agentValue(result.Result), agentValue(result.URL))
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s) %s\n", textsafe.Line(result.IssueRef), textsafe.Line(result.Result), textsafe.Line(result.ExternalID), textsafe.Line(result.URL))
		return err
	}}
	cmd.Flags().StringVar(&adopt, "adopt", "", "adopt a verified remote identity without creating")
	cmd.Flags().BoolVar(&resolve, "resolve-not-created", false, "record that inspection found no remote object; a wrong attestation can create a duplicate on the next publish")
	cmd.Flags().StringVar(&attempt, "attempt", "", "exact pending publication attempt UID")
	cmd.Flags().StringVar(&message, "message", "", "inspection evidence for resolving an attempt")
	cmd.MarkFlagsMutuallyExclusive("adopt", "resolve-not-created")
	return cmd
}
func requireIssuePublish(a daemonAPI) error {
	client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return err
	}
	response, callErr := client.InstanceWithResponse(a.ctx)
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if err := externalCLITransportError(response, callErr); err != nil {
		return err
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return err
	}
	var capability struct {
		Supported bool `json:"issue_publish"`
	}
	if err := json.Unmarshal(response.Body, &capability); err != nil {
		return err
	}
	if !capability.Supported {
		return &cliError{Message: "the selected daemon does not support issue publishing; upgrade the daemon", Kind: kindValidation, Code: "issue_publish_unsupported", ExitCode: ExitValidation}
	}
	return nil
}
