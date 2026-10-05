package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func projectsAccessCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "access", Short: "manage project team visibility"}
	cmd.AddCommand(projectsAccessActionCmd(false), projectsAccessActionCmd(true))
	return cmd
}

func projectsAccessActionCmd(write bool) *cobra.Command {
	action := "show"
	if write {
		action = "set"
	}
	var visibility string
	var teamNames []string
	cmd := &cobra.Command{Use: action + " <project>", Short: action + " project visibility", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if write && (visibility != "all" && visibility != "teams") {
			return &cliError{Message: "--visibility must be all or teams", Kind: kindValidation, ExitCode: ExitValidation}
		}
		if write && visibility == "all" && len(teamNames) > 0 {
			return &cliError{Message: "all visibility cannot contain teams", Kind: kindValidation, ExitCode: ExitValidation}
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		project, err := resolveProjectSelector(a, args[0])
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		current, callErr := client.GetProjectAccessWithResponse(a.ctx, &generated.GetProjectAccessRequestOptions{PathParams: &generated.GetProjectAccessPath{ProjectID: project.ID}})
		if err := externalCLITransportError(current, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(current.StatusCode, current.Body, callErr); err != nil {
			return err
		}
		if current.JSON200 == nil {
			return fmt.Errorf("daemon returned no access policy")
		}
		body := current.Body
		policy := current.JSON200.Policy
		if write {
			teams, err := resolveInitialTeamUIDs(a.ctx, client, teamNames)
			if err != nil {
				return err
			}
			wire, callErr := client.SetProjectAccessWithResponse(a.ctx, &generated.SetProjectAccessRequestOptions{PathParams: &generated.SetProjectAccessPath{ProjectID: project.ID}, Body: &generated.SetProjectAccessBody{Visibility: generated.SetProjectAccessRequestBodyVisibility(visibility), TeamUids: teams, Revision: &policy.Revision}})
			if err := externalCLITransportError(wire, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
				return err
			}
			if wire.JSON200 == nil {
				return fmt.Errorf("daemon returned no access policy")
			}
			body = wire.Body
			policy = wire.JSON200.Policy
		}
		_, emitted, err := emitPassthrough(cmd, body)
		if err != nil || emitted {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "project=%s visibility=%s revision=%d teams=%s\n", textsafe.Line(project.Name), policy.Visibility, policy.Revision, strings.Join(policy.TeamUids, ","))
		return err
	}}
	if write {
		cmd.Flags().StringVar(&visibility, "visibility", "", "all or teams")
		cmd.Flags().StringArrayVar(&teamNames, "team", nil, "team name or UID (repeatable)")
	}
	return cmd
}
