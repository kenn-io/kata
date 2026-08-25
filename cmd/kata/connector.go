package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newConnectorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connector",
		Short: "inspect and configure external connectors",
	}
	cmd.AddCommand(
		newConnectorListCmd(),
		newConnectorStatusCmd(),
		newConnectorFieldCmd(),
	)
	return cmd
}

func newConnectorFieldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "field",
		Short: "inspect and configure bidirectional field mappings",
	}
	cmd.AddCommand(
		newConnectorFieldListCmd(),
		newConnectorFieldMapCmd(),
		newConnectorFieldUnmapCmd(),
	)
	return cmd
}

func newConnectorListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list configured connector instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			apiClient, err := connectorAPIClient(ctx, true)
			if err != nil {
				return err
			}
			resp, callErr := apiClient.ListConnectorsWithResponse(ctx)
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return fmt.Errorf("connector list: empty response")
			}
			return printConnectorList(cmd, resp.Body, resp.JSON200.Connectors)
		},
	}
}

func newConnectorStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <instance>",
		Short: "show safe connector status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			apiClient, err := connectorAPIClient(ctx, true)
			if err != nil {
				return err
			}
			resp, callErr := apiClient.GetConnectorStatusWithResponse(ctx, &generated.GetConnectorStatusRequestOptions{
				PathParams: &generated.GetConnectorStatusPath{Instance: args[0]},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return fmt.Errorf("connector status: empty response")
			}
			return printConnector(cmd, resp.Body, *resp.JSON200, false)
		},
	}
}

func newConnectorFieldListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <instance>",
		Short: "list public connector field descriptors",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			apiClient, err := connectorAPIClient(ctx, true)
			if err != nil {
				return err
			}
			resp, callErr := apiClient.ListConnectorFieldsWithResponse(ctx, &generated.ListConnectorFieldsRequestOptions{
				PathParams: &generated.ListConnectorFieldsPath{Instance: args[0]},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return fmt.Errorf("connector fields: empty response")
			}
			return printConnectorFields(cmd, resp.Body, args[0], resp.JSON200.Fields)
		},
	}
}

func newConnectorFieldMapCmd() *cobra.Command {
	var external string
	cmd := &cobra.Command{
		Use:   "map <instance> <kata-field>",
		Short: "map a connector field bidirectionally",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("external") || strings.TrimSpace(external) == "" {
				return externalCLIValidation("--external is required")
			}
			ctx := cmd.Context()
			apiClient, err := connectorAPIClient(ctx, true)
			if err != nil {
				return err
			}
			resp, callErr := apiClient.MapConnectorFieldWithResponse(ctx, &generated.MapConnectorFieldRequestOptions{
				PathParams: &generated.MapConnectorFieldPath{Instance: args[0], KataField: args[1]},
				Body:       &generated.MapConnectorFieldBody{ExternalField: external},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return fmt.Errorf("map connector field: empty response")
			}
			return printConnectorMapping(cmd, resp.Body, args[0], *resp.JSON200)
		},
	}
	cmd.Flags().StringVar(&external, "external", "", "external field selector")
	return cmd
}

func newConnectorFieldUnmapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unmap <instance> <kata-field>",
		Short: "disable a connector field mapping",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			apiClient, err := connectorAPIClient(ctx, true)
			if err != nil {
				return err
			}
			resp, callErr := apiClient.UnmapConnectorFieldWithResponse(ctx, &generated.UnmapConnectorFieldRequestOptions{
				PathParams: &generated.UnmapConnectorFieldPath{Instance: args[0], KataField: args[1]},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return fmt.Errorf("unmap connector field: empty response")
			}
			return printConnectorMapping(cmd, resp.Body, args[0], *resp.JSON200)
		},
	}
}

func connectorAPIClient(ctx context.Context, longRunning bool) (*kataclient.Client, error) {
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return nil, err
	}
	httpClientForRequest := httpClientFor
	if longRunning {
		httpClientForRequest = longRunningClientFor
	}
	httpClient, err := httpClientForRequest(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	return kataclient.NewWithHTTPClient(baseURL, httpClient)
}

func externalCLITransportError[T any](resp *T, callErr error) error {
	if resp != nil {
		return nil
	}
	if callErr != nil {
		return callErr
	}
	return &cliError{
		Message: "external root request returned no response", Kind: kindInternal, ExitCode: ExitInternal,
	}
}

func externalCLIResponseError(status int, body []byte, callErr error) error {
	if status >= http.StatusBadRequest {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" && envelope.Error.Message != "" {
			return apiErrFromBody(status, body)
		}
		message := fmt.Sprintf("external root request failed (HTTP %d)", status)
		if statusText := http.StatusText(status); statusText != "" {
			message = fmt.Sprintf("external root request failed (HTTP %d %s)", status, statusText)
		}
		return &cliError{
			Message: message, Kind: kindForStatus(status), ExitCode: mapStatusToExit(status, ""),
		}
	}
	return callErr
}

func externalCLIValidation(message string) error {
	return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
}

func printConnectorList(cmd *cobra.Command, raw []byte, connectors []generated.ConnectorOut) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), json.RawMessage(raw))
	}
	if flags.Quiet {
		return nil
	}
	items := append([]generated.ConnectorOut(nil), connectors...)
	sort.Slice(items, func(i, j int) bool { return items[i].InstanceID < items[j].InstanceID })
	for _, item := range items {
		if err := printConnector(cmd, nil, item, true); err != nil {
			return err
		}
	}
	return nil
}

func printConnector(cmd *cobra.Command, raw []byte, connector generated.ConnectorOut, compact bool) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), json.RawMessage(raw))
	}
	if flags.Quiet {
		return nil
	}
	capabilities := append([]string(nil), connector.Capabilities...)
	sort.Strings(capabilities)
	state := "unhealthy"
	if connector.Healthy {
		state = "healthy"
	}
	w := cmd.OutOrStdout()
	if currentOutputMode() == outputAgent {
		_, err := fmt.Fprintf(w, "instance=%s state=%s connector_id=%s display_name=%s protocol=%s capabilities=%s health_error=%s\n",
			agentValue(connector.InstanceID), state, agentValue(optionalExternalString(connector.ConnectorID)),
			agentValue(optionalExternalString(connector.DisplayName)), agentValue(optionalExternalString(connector.Protocol)),
			agentValue(optionalExternalList(capabilities)), agentValue(optionalExternalString(connector.HealthError)))
		return err
	}
	if compact {
		_, err := fmt.Fprintf(w, "%s  %s  connector=%s  capabilities=%s\n",
			textsafe.Line(connector.InstanceID), state, textsafe.Line(optionalExternalString(connector.ConnectorID)),
			textsafe.Line(optionalExternalList(capabilities)))
		return err
	}
	_, err := fmt.Fprintf(w, "Connector: %s\nInstance: %s\nState: %s\nConnector ID: %s\nProtocol: %s\nCapabilities: %s\nHealth error: %s\n",
		textsafe.Line(optionalExternalString(connector.DisplayName)), textsafe.Line(connector.InstanceID), state,
		textsafe.Line(optionalExternalString(connector.ConnectorID)), textsafe.Line(optionalExternalString(connector.Protocol)),
		textsafe.Line(optionalExternalList(capabilities)), textsafe.Line(optionalExternalString(connector.HealthError)))
	return err
}

func printConnectorFields(cmd *cobra.Command, raw []byte, instance string, fields []generated.FieldDescriptor) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), json.RawMessage(raw))
	}
	if flags.Quiet {
		return nil
	}
	items := append([]generated.FieldDescriptor(nil), fields...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	for _, field := range items {
		kinds := append([]string(nil), field.AcceptedKinds...)
		sort.Strings(kinds)
		kindList := optionalExternalList(kinds)
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "instance=%s field=%s name=%s kinds=%s nullable=%t writable=%t schema=%s\n",
				agentValue(instance), agentValue(field.ID), agentValue(field.DisplayName), agentValue(kindList),
				field.Nullable, field.Writable, agentValue(field.SchemaRevision)); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  kinds=%s nullable=%t writable=%t schema=%s\n",
			textsafe.Line(field.ID), textsafe.Line(field.DisplayName), textsafe.Line(kindList),
			field.Nullable, field.Writable, textsafe.Line(field.SchemaRevision)); err != nil {
			return err
		}
	}
	return nil
}

func printConnectorMapping(cmd *cobra.Command, raw []byte, instance string, mapping generated.ExternalFieldMappingOut) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), json.RawMessage(raw))
	}
	if flags.Quiet {
		return nil
	}
	state := "inactive"
	if mapping.Active {
		state = "active"
	}
	if currentOutputMode() == outputAgent {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "instance=%s field=%s external_field=%s external_name=%s active=%t\n",
			agentValue(instance), agentValue(mapping.KataField), agentValue(mapping.ExternalFieldID),
			agentValue(mapping.ExternalFieldName), mapping.Active)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s mapped to %s (%s): %s\n",
		textsafe.Line(mapping.KataField), textsafe.Line(mapping.ExternalFieldName),
		textsafe.Line(mapping.ExternalFieldID), state)
	return err
}

func optionalExternalString(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "-"
	}
	return *value
}

func optionalExternalList(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}
