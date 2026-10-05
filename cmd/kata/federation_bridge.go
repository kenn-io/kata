package main

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func federationBridgeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "bridge", Short: "relay selected projects through a configured hub"}
	cmd.AddCommand(federationBridgeConnectCmd(), federationBridgeStatusCmd(), federationBridgeDisconnectCmd())
	return cmd
}

func federationBridgeConnectCmd() *cobra.Command {
	var catalog, hubProject string
	var preflight bool
	serveDownstream := true
	cmd := &cobra.Command{
		Use: "connect", Short: "connect one project bidirectionally using the hub's saved account", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectName := strings.TrimSpace(flags.Project)
			if projectName == "" {
				return federationRebindSelectorError("bridge connect requires --project")
			}
			if err := config.ValidateProjectName(projectName); err != nil {
				return federationRebindSelectorError(err.Error())
			}
			actor, _ := resolveActor(cmd.Context(), flags.As, nil)
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			apiClient, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			response, callErr := apiClient.ConnectFederationBridgeWithResponse(a.ctx, &generated.ConnectFederationBridgeRequestOptions{Body: &generated.ConnectFederationBridgeBody{
				HubCatalog: catalog, HubProject: hubProject, ProjectName: projectName, Actor: &actor, Preflight: &preflight, ServeDownstream: &serveDownstream,
			}})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			raw, emitted, err := emitPassthrough(cmd, response.Body)
			if err != nil || emitted {
				return err
			}
			var body api.FederationBridgeBody
			if err := json.Unmarshal(raw, &body); err != nil {
				return err
			}
			if currentOutputMode() == outputAgent {
				return writeAgentKVRow(cmd.OutOrStdout(), agentRowField("project", body.ProjectName), agentRowField("status", body.Status), agentRowField("direction", body.Direction), agentRowField("hub", body.HubCatalog), agentRowField("upstream_account", body.UpstreamAccount), agentRowField("local_account", body.LocalAccount))
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "bridge %s: %s (%s)\nhub: %s — account: %s; local account: %s\n", textsafe.Line(body.ProjectName), textsafe.Line(body.Status), textsafe.Line(body.Direction), textsafe.Line(body.HubCatalog), textsafe.Line(body.UpstreamAccount), textsafe.Line(body.LocalAccount))
			return err
		},
	}
	cmd.Flags().StringVar(&catalog, "hub-daemon", "", "configured hub daemon holding the upstream user credential")
	cmd.Flags().StringVar(&hubProject, "hub-project", "", "existing project on the selected hub")
	cmd.Flags().BoolVar(&preflight, "preflight", false, "preview the selected account and project without enrollment")
	cmd.Flags().BoolVar(&serveDownstream, "serve-downstream", true, "allow this daemon to relay the selected project to its spokes")
	for _, flag := range []string{"hub-daemon", "hub-project"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
	return cmd
}

func federationBridgeStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "show one local bridge's recorded connection state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		project := strings.TrimSpace(flags.Project)
		if project == "" {
			return federationRebindSelectorError("bridge status requires --project")
		}
		if err := config.ValidateProjectName(project); err != nil {
			return federationRebindSelectorError(err.Error())
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		apiClient, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, callErr := apiClient.GetFederationBridgeStatusWithResponse(a.ctx, &generated.GetFederationBridgeStatusRequestOptions{PathParams: &generated.GetFederationBridgeStatusPath{ProjectName: project}})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		raw, emitted, err := emitPassthrough(cmd, response.Body)
		if err != nil || emitted {
			return err
		}
		var body api.FederationBridgeStatusBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		if currentOutputMode() == outputAgent {
			return writeAgentKVRow(cmd.OutOrStdout(), agentRowField("project", body.ProjectName), agentRowField("state", body.State), agentRowField("direction", body.Direction), agentRowField("credential_status", body.CredentialStatus), agentRowField("hub", body.HubCatalog), agentRowField("upstream_account", body.UpstreamAccount), agentRowField("local_account", body.LocalAccount))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "bridge %s: %s (%s)\nhub: %s — account: %s; local account: %s\n", textsafe.Line(body.ProjectName), textsafe.Line(body.State), textsafe.Line(body.Direction), textsafe.Line(body.HubCatalog), textsafe.Line(body.UpstreamAccount), textsafe.Line(body.LocalAccount))
		return err
	}}
}

func federationBridgeDisconnectCmd() *cobra.Command {
	var preflight bool
	cmd := &cobra.Command{Use: "disconnect", Short: "revoke one bridge grant and preserve the detached local project", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		project := strings.TrimSpace(flags.Project)
		if project == "" {
			return federationRebindSelectorError("bridge disconnect requires --project")
		}
		if err := config.ValidateProjectName(project); err != nil {
			return federationRebindSelectorError(err.Error())
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		apiClient, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, callErr := apiClient.DisconnectFederationBridgeWithResponse(a.ctx, &generated.DisconnectFederationBridgeRequestOptions{PathParams: &generated.DisconnectFederationBridgePath{ProjectName: project}, Body: &generated.DisconnectFederationBridgeBody{Preflight: &preflight}})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		raw, emitted, err := emitPassthrough(cmd, response.Body)
		if err != nil || emitted {
			return err
		}
		var body api.FederationBridgeDisconnectResult
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		if currentOutputMode() == outputAgent {
			return writeAgentKVRow(cmd.OutOrStdout(), agentRowField("project", body.ProjectName), agentRowField("status", body.Status))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "bridge %s: %s\n", textsafe.Line(body.ProjectName), textsafe.Line(body.Status))
		return err
	}}
	cmd.Flags().BoolVar(&preflight, "preflight", false, "check local detach safety without contacting the hub or changing state")
	return cmd
}
