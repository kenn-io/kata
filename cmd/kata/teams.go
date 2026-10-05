package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newTeamsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "teams", Short: "manage project-access teams"}
	cmd.AddCommand(teamsCreateCmd(), teamsListCmd(), teamsShowCmd())
	members := &cobra.Command{Use: "members", Short: "manage canonical actor membership"}
	members.AddCommand(teamsMembershipCmd(true), teamsMembershipCmd(false))
	cmd.AddCommand(members)
	return cmd
}

func teamsCreateCmd() *cobra.Command {
	return &cobra.Command{Use: "create <name>", Short: "create a project-access team", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		wire, callErr := client.CreateTeamWithResponse(a.ctx, &generated.CreateTeamRequestOptions{Body: &generated.CreateTeamBody{Name: args[0]}})
		if err := externalCLITransportError(wire, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
			return err
		}
		_, emitted, err := emitPassthrough(cmd, wire.Body)
		if err != nil || emitted {
			return err
		}
		if wire.JSON200 == nil {
			return fmt.Errorf("daemon returned no team")
		}
		team := wire.JSON200.Team
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "created team %s uid=%s\n", textsafe.Line(team.Name), team.UID)
		return err
	}}
}

func teamsListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "list project-access teams", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		wire, callErr := client.ListTeamsWithResponse(a.ctx)
		if err := externalCLITransportError(wire, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
			return err
		}
		_, emitted, err := emitPassthrough(cmd, wire.Body)
		if err != nil || emitted {
			return err
		}
		if wire.JSON200 == nil {
			return fmt.Errorf("daemon returned no team list")
		}
		for _, team := range wire.JSON200.Teams {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s uid=%s revision=%d\n", textsafe.Line(team.Name), team.UID, team.Revision); err != nil {
				return err
			}
		}
		return nil
	}}
}

func teamsShowCmd() *cobra.Command {
	return &cobra.Command{Use: "show <team>", Short: "show one team and its members", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		uid, err := resolveTeamUID(a.ctx, client, args[0])
		if err != nil {
			return err
		}
		wire, callErr := client.ShowTeamWithResponse(a.ctx, &generated.ShowTeamRequestOptions{PathParams: &generated.ShowTeamPath{TeamUID: uid}})
		if err := externalCLITransportError(wire, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
			return err
		}
		_, emitted, err := emitPassthrough(cmd, wire.Body)
		if err != nil || emitted {
			return err
		}
		if wire.JSON200 == nil {
			return fmt.Errorf("daemon returned no team")
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "team %s uid=%s revision=%d\n", textsafe.Line(wire.JSON200.Team.Name), uid, wire.JSON200.Team.Revision); err != nil {
			return err
		}
		for _, member := range wire.JSON200.Members {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), textsafe.Line(member)); err != nil {
				return err
			}
		}
		return nil
	}}
}

func teamsMembershipCmd(present bool) *cobra.Command {
	action := "remove"
	if present {
		action = "add"
	}
	var actor string
	cmd := &cobra.Command{Use: action + " <team> --actor <actor>", Short: action + " a canonical actor's team membership", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		actor = strings.TrimSpace(actor)
		if actor == "" {
			return &cliError{Message: "--actor is required", Kind: kindValidation, ExitCode: ExitValidation}
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		uid, err := resolveTeamUID(a.ctx, client, args[0])
		if err != nil {
			return err
		}
		if present {
			wire, callErr := client.AddTeamMemberWithResponse(a.ctx, &generated.AddTeamMemberRequestOptions{PathParams: &generated.AddTeamMemberPath{TeamUID: uid, Actor: actor}})
			if err := externalCLITransportError(wire, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
				return err
			}
			_, emitted, err := emitPassthrough(cmd, wire.Body)
			if err != nil || emitted {
				return err
			}
		} else {
			wire, callErr := client.RemoveTeamMemberWithResponse(a.ctx, &generated.RemoveTeamMemberRequestOptions{PathParams: &generated.RemoveTeamMemberPath{TeamUID: uid, Actor: actor}})
			if err := externalCLITransportError(wire, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
				return err
			}
			_, emitted, err := emitPassthrough(cmd, wire.Body)
			if err != nil || emitted {
				return err
			}
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s team membership uid=%s actor=%s\n", action, uid, textsafe.Line(actor))
		return err
	}}
	cmd.Flags().StringVar(&actor, "actor", "", "canonical account actor")
	return cmd
}

// Names are resolved only through the selected daemon's owner-authorized list.
func resolveTeamUID(ctx context.Context, client *kataclient.Client, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	wire, callErr := client.ListTeamsWithResponse(ctx)
	if wire == nil {
		return "", externalCLITransportError(wire, callErr)
	}
	if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
		return "", err
	}
	if wire.JSON200 == nil {
		return "", fmt.Errorf("daemon returned no team list")
	}
	resolved := ""
	for _, team := range wire.JSON200.Teams {
		if team.Name == selector || team.UID == selector {
			if resolved != "" && resolved != team.UID {
				return "", &cliError{Message: "team selector is ambiguous; use its UID", Kind: kindConflict, ExitCode: ExitConflict}
			}
			resolved = team.UID
		}
	}
	if resolved == "" {
		return "", &cliError{Message: "team not found", Kind: kindNotFound, ExitCode: ExitNotFound}
	}
	return resolved, nil
}

func resolveInitialTeamUIDs(ctx context.Context, client *kataclient.Client, names []string) ([]string, error) {
	teams := make([]string, 0, len(names))
	for _, name := range names {
		uid, err := resolveTeamUID(ctx, client, name)
		if err != nil {
			return nil, err
		}
		teams = append(teams, uid)
	}
	return teams, nil
}
