package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func cronAccessCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "access", Short: "manage who may define execution jobs and observe runs", Long: "Control independent local cron grants through teams. Needs the daemon owner's authority."}
	cmd.AddCommand(cronAccessActionCmd(false), cronAccessActionCmd(true))
	return cmd
}

func cronAccessActionCmd(write bool) *cobra.Command {
	action := "show"
	if write {
		action = "set"
	}
	var define, observe string
	var defineTeams, observeTeams []string
	cmd := &cobra.Command{Use: action + " <project>", Short: action + " local cron grants", Args: cobra.ExactArgs(1)}
	cmd.Long = "Show the project's independent define and observe grants and policy revision."
	cmd.Example = "  kata cron access show example-project --agent"
	if write {
		cmd.Long = "Set define or observe to any or teams. Repeat --define-team or --observe-team\nwith a team name or UID. A team flag selects teams mode when its mode is\nomitted. A grant with no flags keeps its current value. A teams mode without\nteam flags clears that grant's teams.\nConcurrent edits fail with conflict (exit 5); reload with access show and retry."
		cmd.Example = "  kata cron access set example-project --define teams --define-team authors --agent\n  kata cron access set example-project --observe teams --observe-team runtimes --agent"
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		mentioned := func(grant string) bool { return cmd.Flags().Changed(grant) || cmd.Flags().Changed(grant+"-team") }
		if write {
			if !mentioned("define") && !mentioned("observe") {
				return cronCLIValidation("set requires define or observe flags")
			}
			for _, value := range []struct {
				mode    string
				teams   []string
				changed bool
			}{{define, defineTeams, cmd.Flags().Changed("define")}, {observe, observeTeams, cmd.Flags().Changed("observe")}} {
				if value.changed && value.mode != "any" && value.mode != "teams" {
					return cronCLIValidation("grant mode must be any or teams")
				}
				if value.mode == "any" && len(value.teams) > 0 {
					return cronCLIValidation("any grant cannot contain teams")
				}
			}
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
		current, callErr := client.GetCronAccessWithResponse(a.ctx, &generated.GetCronAccessRequestOptions{PathParams: &generated.GetCronAccessPath{ProjectID: project.ID}})
		if err := externalCLITransportError(current, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(current.StatusCode, current.Body, callErr); err != nil {
			return err
		}
		if current.JSON200 == nil {
			return fmt.Errorf("daemon returned no cron access policy")
		}
		body := current.Body
		policy := current.JSON200.Policy
		if write {
			update := func(current generated.CronGrantResponse, grant, mode string, names []string) (generated.CronGrant, error) {
				if !mentioned(grant) {
					return generated.CronGrant{Mode: current.Mode, TeamUids: current.TeamUids}, nil
				}
				if mode == "" {
					mode = "teams"
				}
				teams, err := resolveInitialTeamUIDs(a.ctx, client, names)
				if err != nil {
					return generated.CronGrant{}, err
				}
				return generated.CronGrant{Mode: mode, TeamUids: teams}, nil
			}
			nextDefine, err := update(policy.Define, "define", define, defineTeams)
			if err != nil {
				return err
			}
			nextObserve, err := update(policy.Observe, "observe", observe, observeTeams)
			if err != nil {
				return err
			}
			result, callErr := client.SetCronAccessWithResponse(a.ctx, &generated.SetCronAccessRequestOptions{PathParams: &generated.SetCronAccessPath{ProjectID: project.ID}, Body: &generated.SetCronAccessBody{Define: nextDefine, Observe: nextObserve, Revision: policy.Revision}})
			if err := externalCLITransportError(result, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(result.StatusCode, result.Body, callErr); err != nil {
				return err
			}
			if result.JSON200 == nil {
				return fmt.Errorf("daemon returned no cron access policy")
			}
			body = result.Body
			policy = result.JSON200.Policy
		}
		_, emitted, err := emitPassthrough(cmd, body)
		if err != nil || emitted {
			return err
		}
		if currentOutputMode() == outputAgent {
			return writeAgentOKLine(cmd.OutOrStdout(), "cron-access", agentRowField("project", project.Name), agentRowField("revision", fmt.Sprint(policy.Revision)), agentRowField("define", policy.Define.Mode), agentRowField("observe", policy.Observe.Mode), agentRowListField("define_teams", policy.Define.TeamUids), agentRowListField("observe_teams", policy.Observe.TeamUids))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "project=%s revision=%d define=%s define_teams=%s observe=%s observe_teams=%s\n", textsafe.Line(project.Name), policy.Revision, policy.Define.Mode, strings.Join(policy.Define.TeamUids, ","), policy.Observe.Mode, strings.Join(policy.Observe.TeamUids, ","))
		return err
	}
	if write {
		cmd.Flags().StringVar(&define, "define", "", "any or teams")
		cmd.Flags().StringArrayVar(&defineTeams, "define-team", nil, "team name or UID for define (repeatable)")
		cmd.Flags().StringVar(&observe, "observe", "", "any or teams")
		cmd.Flags().StringArrayVar(&observeTeams, "observe-team", nil, "team name or UID for observe (repeatable)")
	}
	return cmd
}
