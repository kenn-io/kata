package main

import (
	"encoding/json/v2"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func issueStatusSyncMode(value string) (*generated.EnableIssueSyncRequestBodyStatusSync, error) {
	if value != "one-way" && value != "two-way" {
		return nil, &cliError{Message: "--status-sync must be one-way or two-way", Kind: kindValidation, ExitCode: ExitValidation}
	}
	mode := generated.EnableIssueSyncRequestBodyStatusSync(value)
	return &mode, nil
}

func requireIssueStatusSync(a daemonAPI) error {
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
		Supported bool `json:"issue_status_sync"`
	}
	if err := json.Unmarshal(response.Body, &capability); err != nil {
		return err
	}
	if !capability.Supported {
		return &cliError{Message: "the selected daemon does not support issue status sync; upgrade the daemon", Kind: kindValidation, Code: "issue_status_sync_unsupported", ExitCode: ExitValidation}
	}
	return nil
}

func notionStatusSyncFlagsChanged(cmd *cobra.Command) bool {
	for _, flag := range []string{"status-sync", "complete-group", "todo-group", "closed-status", "open-status"} {
		if cmd.Flags().Changed(flag) {
			return true
		}
	}
	return false
}

func issueStatusSyncDisplayMode(mode string) string {
	if mode == "" {
		return "one-way"
	}
	return mode
}
