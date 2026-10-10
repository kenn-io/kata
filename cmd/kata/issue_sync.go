package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func issueSyncLabel(provider string) string {
	switch provider {
	case "github":
		return "GitHub"
	case "notion":
		return "Notion"
	case "twenty":
		return "Twenty"
	case "plane":
		return "Plane"
	case "linear":
		return "Linear"
	default:
		return provider
	}
}

// issueSyncIntervalFlag accepts a duration or whole seconds, at least one second.
func issueSyncIntervalFlag(provider, value string) (string, error) {
	value = strings.TrimSpace(value)
	seconds, err := strconv.Atoi(value)
	if err == nil && seconds >= 1 {
		return value, nil
	}
	if err != nil {
		duration, parseErr := time.ParseDuration(value)
		if parseErr == nil && duration >= time.Second {
			return value, nil
		}
	}
	return "", &cliError{Message: issueSyncLabel(provider) + " sync interval must be at least one second", Kind: kindValidation, ExitCode: ExitValidation}
}

func newIssueSyncDisableCmd(provider string) *cobra.Command {
	return &cobra.Command{
		Use:     "disable",
		Short:   "disable " + issueSyncLabel(provider) + " polling and retain imported issues",
		Long:    "Stop " + issueSyncLabel(provider) + " polling for this project. Imported issues stay.",
		Example: "  kata sync " + provider + " disable --agent",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, projectID, err := githubSyncProjectAPI(cmd.Context())
			if err != nil {
				return err
			}
			client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			response, callErr := client.DisableIssueSyncWithResponse(a.ctx, &generated.DisableIssueSyncRequestOptions{PathParams: &generated.DisableIssueSyncPath{ProjectID: projectID, Provider: provider}, Body: &generated.DisableIssueSyncBody{}})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, provider, "disabled")
		},
	}
}

func newIssueSyncStatusCmd(provider string) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "show " + issueSyncLabel(provider) + " source and sync progress",
		Long:    "Show the bound " + issueSyncLabel(provider) + " source and sync progress for this project.",
		Example: "  kata sync " + provider + " status --agent",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, projectID, err := githubSyncProjectAPI(cmd.Context())
			if err != nil {
				return err
			}
			client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			response, callErr := client.GetIssueSyncStatusWithResponse(a.ctx, &generated.GetIssueSyncStatusRequestOptions{PathParams: &generated.GetIssueSyncStatusPath{ProjectID: projectID, Provider: provider}})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, provider, "status")
		},
	}
}

func newIssueSyncOnceCmd(provider string) *cobra.Command {
	return &cobra.Command{
		Use:     "once",
		Short:   "run one daemon-side " + issueSyncLabel(provider) + " sync now",
		Long:    "Run one " + issueSyncLabel(provider) + " sync pass now instead of waiting for the poll interval.",
		Example: "  kata sync " + provider + " once --agent",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, projectID, err := githubSyncProjectAPI(cmd.Context())
			if err != nil {
				return err
			}
			a.client, err = longRunningClientForResolved(cmd.Context(), a.resolved)
			if err != nil {
				return err
			}
			client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			response, callErr := client.RunIssueSyncOnceWithResponse(a.ctx, &generated.RunIssueSyncOnceRequestOptions{PathParams: &generated.RunIssueSyncOncePath{ProjectID: projectID, Provider: provider}, Body: &generated.RunIssueSyncOnceBody{}})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			return issueSyncPrintOnce(cmd.OutOrStdout(), response.Body, provider)
		},
	}
}
