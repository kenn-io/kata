package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/internal/version"
	kataclient "go.kenn.io/kata/pkg/client"
)

func newHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "health",
		Short:   "report daemon health",
		Long:    `Read the selected daemon’s health. Use kata doctor to diagnose configuration without starting the daemon.`,
		Example: `  kata health --agent`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// health is a probe — it must report the daemon's actual
			// state, not auto-start one and report on the spawned
			// child. Hammer-test finding #1.
			a, err := discoverDaemonAPI(cmd.Context())
			if err != nil {
				return err
			}
			apiClient, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			resp, callErr := apiClient.HealthWithResponse(a.ctx)
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			bs := resp.Body
			var b struct {
				OK            bool                  `json:"ok"`
				SchemaVersion int                   `json:"schema_version"`
				Uptime        string                `json:"uptime"`
				DBPath        string                `json:"db_path"`
				Embeddings    *api.EmbeddingsHealth `json:"embeddings"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			selected := daemonDiagnosis{State: "ready", Source: a.resolved.Source.String(), SourcePath: a.resolved.SourcePath, Kind: "local", BinaryVersion: version.Version, Endpoint: a.resolved.Address, SchemaVersion: b.SchemaVersion}
			if !b.OK {
				selected.State = "unhealthy"
			}
			if a.resolved.ConfiguredRemote() {
				selected.Kind = "remote"
				selected.Endpoint = safeDaemonOrigin(a.baseURL)
			}
			if profile := a.resolved.LocalProfile; profile != nil {
				selected.Kind = "local_profile"
				selected.Profile = profile.Name
				selected.Home = profile.Home
				selected.StorageID = profile.StorageID
				selected.ExpectedInstanceUID = profile.InstanceUID
				selected.ObservedInstanceUID = profile.InstanceUID
			} else if selected.Kind == "local" {
				selected.Home, _ = config.KataHome()
			}
			selected = withDiagnosisAction(selected)
			warning := func() error {
				if b.Embeddings == nil || (b.Embeddings.Credential != "missing" && b.Embeddings.Credential != "rejected") {
					return nil
				}
				_, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: embeddings: %s; semantic search disabled, lexical only\n", textsafe.Line(b.Embeddings.CredentialReason))
				return err
			}
			mode := currentOutputMode()
			if mode == outputAgent {
				daemonStatus := "unhealthy"
				if b.OK {
					daemonStatus = "running"
				}
				extra := ""
				if a.resolved.LocalProfile != nil {
					extra = fmt.Sprintf(" selected_state=%s profile=%s home=%s instance_uid=%s", selected.State, agentValue(selected.Profile), agentValue(selected.Home), selected.ObservedInstanceUID)
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK health ok=%t daemon=%s%s\n", b.OK, daemonStatus, extra)
				if err != nil {
					return err
				}
				return warning()
			}
			if mode == outputJSON {
				var payload map[string]jsontext.Value
				if err := json.Unmarshal(bs, &payload); err != nil {
					return err
				}
				data, err := json.Marshal(selected)
				if err != nil {
					return err
				}
				payload["selected"] = data
				var buf bytes.Buffer
				if err := emitJSON(&buf, payload); err != nil {
					return err
				}
				_, err = fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			if a.resolved.LocalProfile != nil {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Selected profile: %s home=%s instance_uid=%s\n", selected.Profile, selected.Home, selected.ObservedInstanceUID); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok=%v schema_version=%d uptime=%s db=%s\n",
				b.OK, b.SchemaVersion, b.Uptime, b.DBPath)
			if err != nil {
				return err
			}
			return warning()
		},
	}
}
