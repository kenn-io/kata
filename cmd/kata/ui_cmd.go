package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/internal/web"
)

var (
	prepareWebUIForCommand   = client.PrepareWebUI
	openWebUIForCommand      = client.OpenWebUI
	openWebBrowserForCommand = func(ctx context.Context, launch client.WebUILaunch) error {
		return platformOpenTarget(ctx, launch.PublicURL)
	}
	validateWebUIAssetsForCommand = web.ValidateEmbeddedRelease
)

func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui [issue-ref]",
		Short: "open the Kata web UI",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			prepared, err := prepareWebUIForCommand(ctx, client.PrepareWebUIOptions{
				WorkspaceStart: workspaceStartForRemote(),
				DaemonName:     flags.Daemon,
			})
			if err != nil {
				if flags.Daemon != "" || errors.Is(err, client.ErrRemoteUnavailable) ||
					errors.Is(err, client.ErrNamedDaemonNotFound) {
					return cliDaemonTargetError(err)
				}
				return err
			}
			if !prepared.ConfiguredRemote {
				if err := validateWebUIAssetsForCommand(); err != nil {
					return fmt.Errorf("kata ui is unavailable in this module-source build; install a release binary or build from a clone with `make install`: %w", err)
				}
			}
			returnPath := "/kata"
			if len(args) == 1 {
				returnPath, err = resolveUIIssuePath(cmd, prepared, args[0])
				if err != nil {
					return err
				}
			}
			return openWebUIForCommand(ctx, prepared, returnPath, openWebBrowserForCommand)
		},
	}
}

func resolveUIIssuePath(cmd *cobra.Command, prepared client.PreparedWebUI, rawRef string) (string, error) {
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return "", err
	}
	bareProject := strings.TrimSpace(flags.Project)
	if bareProject == "" {
		bareProject = workspaceProjectName(start)
	}
	parsed, err := ResolveRef(rawRef, bareProject)
	if err != nil {
		return "", &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
	}
	projectID, _, err := resolveProjectIDAndNameForRef(
		cmd.Context(), prepared.BaseURL, start, parsed.ProjectName,
	)
	if err != nil {
		return "", err
	}
	httpClient := prepared.Client
	if httpClient == nil {
		httpClient, err = httpClientFor(cmd.Context(), prepared.BaseURL)
		if err != nil {
			return "", err
		}
	}
	status, body, err := httpDoJSON(cmd.Context(), httpClient, http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s",
			strings.TrimRight(prepared.BaseURL, "/"), projectID, url.PathEscape(parsed.RefForAPI)), nil)
	if err != nil {
		return "", err
	}
	if status >= http.StatusBadRequest {
		return "", apiErrFromBody(status, body)
	}
	var response struct {
		Issue struct {
			UID        string `json:"uid"`
			ProjectUID string `json:"project_uid"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.New("daemon returned an invalid issue response")
	}
	if !uid.Valid(response.Issue.UID) {
		return "", errors.New("daemon returned an invalid issue UID")
	}
	if !uid.Valid(response.Issue.ProjectUID) {
		return "", errors.New("daemon returned an invalid project UID")
	}
	return fmt.Sprintf("/kata?scope=%s&issue=%s", response.Issue.ProjectUID, response.Issue.UID), nil
}
