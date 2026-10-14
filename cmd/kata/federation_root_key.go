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

func federationRootKeyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "root-key", Short: "recover this daemon's root signing key for hub projects"}
	cmd.AddCommand(federationRootKeyRepinCmd())
	return cmd
}

func federationRootKeyRepinCmd() *cobra.Command {
	var preflight, strandRelays bool
	var previousKeyFile string
	cmd := &cobra.Command{
		Use:   "repin",
		Short: "pin a hub project to the daemon's loaded root signing key",
		Long: "Pin a hub project rooted on this daemon to the loaded federation-root.key, creating the key " +
			"file when it is missing. Without --previous-key-file, downstream relays that pinned the old key " +
			"can no longer sync the project. With it, the replacement is a signed rotation that downstream relays accept.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project := strings.TrimSpace(flags.Project)
			if project == "" {
				return federationRebindSelectorError("root-key repin requires --project")
			}
			if err := config.ValidateProjectName(project); err != nil {
				return federationRebindSelectorError(err.Error())
			}
			body := &generated.RepinFederationRootKeyBody{Preflight: &preflight}
			if strandRelays {
				body.StrandRelays = &strandRelays
			}
			if previousKeyFile != "" {
				// The path names a file on the daemon host; the daemon validates it.
				body.PreviousKeyFile = &previousKeyFile
			}
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			apiClient, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			response, callErr := apiClient.RepinFederationRootKeyWithResponse(a.ctx, &generated.RepinFederationRootKeyRequestOptions{PathParams: &generated.RepinFederationRootKeyPath{ProjectName: project}, Body: body})
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
			var result api.FederationRootKeyRepinResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			return printRootKeyRepin(cmd, result)
		},
	}
	cmd.Flags().BoolVar(&preflight, "preflight", false, "show the planned repin and its consequences without changing state")
	cmd.Flags().StringVar(&previousKeyFile, "previous-key-file", "", "absolute path of the previous federation-root.key on the daemon host; signs a rotation downstream relays accept")
	cmd.Flags().BoolVar(&strandRelays, "strand-relays", false, "accept that relays pinned to the previous key can no longer sync; required for an unsigned repin while the project has active relay enrollments")
	return cmd
}

func printRootKeyRepin(cmd *cobra.Command, result api.FederationRootKeyRepinResult) error {
	out := cmd.OutOrStdout()
	if currentOutputMode() == outputAgent {
		return writeAgentOKLine(out, "federation-root-key-repin", agentRowField("project", result.ProjectName), agentRowField("status", result.Status), agentRowField("method", result.Method), agentRowField("key_id", result.KeyID), agentRowField("previous_key_id", result.PreviousKeyID), agentRowField("creates_key_file", fmt.Sprint(result.CreatesKeyFile)), agentRowField("active_relay_enrollments", fmt.Sprint(result.ActiveRelayEnrollments)), agentOptionalRowField("key_file", &result.KeyFile))
	}
	if _, err := fmt.Fprintf(out, "root key %s: %s (%s)\n", textsafe.Line(result.ProjectName), textsafe.Line(result.Status), textsafe.Line(result.Method)); err != nil {
		return err
	}
	if result.KeyFile != "" {
		if _, err := fmt.Fprintf(out, "key file: %s\n", textsafe.Line(result.KeyFile)); err != nil {
			return err
		}
	}
	if result.Consequence != "" {
		if _, err := fmt.Fprintf(out, "%s\n", textsafe.Line(result.Consequence)); err != nil {
			return err
		}
	}
	return nil
}
