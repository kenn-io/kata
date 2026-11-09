package main

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// runCronProjectCommand resolves the selected project, sends one request, and
// prints the daemon's raw response. honorQuiet lets --quiet suppress it.
func runCronProjectCommand(cmd *cobra.Command, honorQuiet bool, call func(context.Context, *kataclient.Client, int64, string) ([]byte, error)) error {
	daemon, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	project, err := resolveCronProject(daemon)
	if err != nil {
		return err
	}
	actor, _ := resolveActor(cmd.Context(), flags.As, nil)
	client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
	if err != nil {
		return err
	}
	raw, err := call(cmd.Context(), client, project.ID, actor)
	if err != nil {
		return err
	}
	if honorQuiet && flags.Quiet && currentOutputMode() != outputJSON {
		return nil
	}
	return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
}

func resolveCronProject(daemon daemonAPI) (projectRef, error) {
	if strings.TrimSpace(flags.Project) != "" {
		return resolveProjectSelectorIncludingArchived(daemon, flags.Project)
	}
	return resolveProjectArgFlagOrWorkspace(daemon, nil)
}

func newCronProjectPauseCmd(paused bool) *cobra.Command {
	action := "resume"
	if paused {
		action = "pause"
	}
	var reason string
	var discard bool
	cmd := &cobra.Command{Use: action, Short: action + " cron delivery for the whole project", Args: cobra.NoArgs,
		Long: "Change cron delivery policy at the project's owning hub. Running work may finish; adapters must honor the pause before starting new work. Resuming preserves job flags and catch-up policies.",
	}
	if paused {
		cmd.Flags().StringVar(&reason, "reason", "", "pause reason (up to 256 bytes)")
	} else {
		cmd.Flags().BoolVar(&discard, "discard-missed", false, "discard all undelivered scheduled occurrences through the resume cutoff (root only)")
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return runCronProjectCommand(cmd, true, func(ctx context.Context, c *kataclient.Client, projectID int64, actor string) ([]byte, error) {
			if paused {
				wire, err := c.PauseCronProjectWithResponse(ctx, &generated.PauseCronProjectRequestOptions{PathParams: &generated.PauseCronProjectPath{ProjectID: projectID}, Body: &generated.PauseCronProjectBody{Actor: &actor, Reason: &reason}})
				if wire == nil {
					return nil, externalCLITransportError(wire, err)
				}
				return cronCLIResponse(wire.StatusCode, wire.Body, err)
			}
			options := &generated.ResumeCronProjectRequestOptions{PathParams: &generated.ResumeCronProjectPath{ProjectID: projectID}, Body: &generated.ResumeCronProjectBody{Actor: &actor}}
			if discard {
				revision, err := cronDiscardCapabilities(ctx, c, projectID, true)
				if err != nil {
					return nil, err
				}
				options.Query = &generated.ResumeCronProjectQuery{DiscardMissed: &discard}
				options.Header = &generated.ResumeCronProjectHeaders{IfMatch: new(fmt.Sprintf("rev-%d", revision))}
			}
			wire, err := c.ResumeCronProjectWithResponse(ctx, options)
			if wire == nil {
				return nil, externalCLITransportError(wire, err)
			}
			return cronCLIResponse(wire.StatusCode, wire.Body, err)
		})
	}
	return cmd
}

// Read once per invocation. A conflict or uncertain response must never refresh
// this guard and retry a destructive resume against a later pause.
func cronDiscardCapabilities(ctx context.Context, c *kataclient.Client, pid int64, project bool) (int64, error) {
	caps, err := c.GetCronCapabilities(ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: pid}})
	if err != nil {
		return 0, err
	}
	if caps == nil || !slices.Contains(caps.Controls, "resume_discard_missed") || (project && caps.ProjectRevision <= 0) {
		return 0, cronCLIValidation("daemon cannot safely discard missed runs")
	}
	return caps.ProjectRevision, nil
}
