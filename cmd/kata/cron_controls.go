package main

import (
	"encoding/json/jsontext"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newCronProjectPauseCmd(paused bool) *cobra.Command {
	action := "resume"
	if paused {
		action = "pause"
	}
	var reason string
	cmd := &cobra.Command{Use: action, Short: action + " cron delivery for the whole project", Args: cobra.NoArgs,
		Long: "Change cron delivery policy at the project's owning hub. Running work may finish; adapters must honor the pause before starting new work. Resuming preserves job flags and catch-up policies.",
	}
	if paused {
		cmd.Flags().StringVar(&reason, "reason", "", "pause reason (up to 256 bytes)")
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		daemon, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		var project projectRef
		if strings.TrimSpace(flags.Project) != "" {
			project, err = resolveProjectSelectorIncludingArchived(daemon, flags.Project)
		} else {
			project, err = resolveProjectArgFlagOrWorkspace(daemon, nil)
		}
		if err != nil {
			return err
		}
		actor, _ := resolveActor(cmd.Context(), flags.As, nil)
		client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
		if err != nil {
			return err
		}
		var raw []byte
		if paused {
			response, callErr := client.PauseCronProjectWithResponse(cmd.Context(), &generated.PauseCronProjectRequestOptions{PathParams: &generated.PauseCronProjectPath{ProjectID: project.ID}, Body: &generated.PauseCronProjectBody{Actor: &actor, Reason: &reason}})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			raw, err = cronCLIResponse(response.StatusCode, response.Body, callErr)
		} else {
			response, callErr := client.ResumeCronProjectWithResponse(cmd.Context(), &generated.ResumeCronProjectRequestOptions{PathParams: &generated.ResumeCronProjectPath{ProjectID: project.ID}, Body: &generated.ResumeCronProjectBody{Actor: &actor}})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			raw, err = cronCLIResponse(response.StatusCode, response.Body, callErr)
		}
		if err != nil {
			return err
		}
		if flags.Quiet && currentOutputMode() != outputJSON {
			return nil
		}
		return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
	}
	return cmd
}
