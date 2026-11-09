package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
)

func newTrackerSyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tracker", Short: "choose named tracker accounts and inspect durable operations"}
	recovery := &cobra.Command{Use: "recovery", Short: "inspect local tracker uncertainty as the daemon owner", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		a, project, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		if refresh, _ := cmd.Flags().GetBool("refresh"); refresh {
			if _, err := client.RefreshTrackerRecovery(a.ctx, project); err != nil {
				return err
			}
		}
		result, err := client.TrackerRecovery(a.ctx, project)
		if err != nil {
			return err
		}
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), result)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "tracker recovery role=%s phase=%s write_uid=%s provider_acknowledged=%t writes_blocked=%t activation_required=%t restored_history_gap=%t\n", result.Role, result.Phase, result.WriteUID, result.ProviderAcknowledged, result.WritesBlocked, result.ActivationRequired, result.RestoredHistoryGap)
		return err
	}}
	recovery.Flags().Bool("refresh", false, "recover retained receipts without provider writes (actual root daemon owner only)")
	cmd.AddCommand(recovery)
	cmd.AddCommand(&cobra.Command{Use: "settle <uid>", Short: "record an unresolved stopped control as the local daemon owner", Long: "Record an unresolved outcome for a stopped caller control and release its pending-operation capacity. This never retries its effects, clears provider uncertainty, or permits new writes across an unknown outcome. Run against the daemon that owns the control receipt.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !uid.Valid(args[0]) {
			return trackerCLIValidation("operation UID must be a normalized ULID")
		}
		a, project, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		result, err := client.SettleTrackerControl(a.ctx, project, args[0])
		if err != nil {
			return err
		}
		return printTrackerReceipt(cmd, result)
	}})
	cmd.AddCommand(&cobra.Command{Use: "connections", Short: "discover accounts granted to your authenticated identity", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		a, project, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, err := client.TrackerConnections(a.ctx, project)
		if err != nil {
			return err
		}
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), response)
		}
		for _, c := range response.Connections {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s owner=%s connection=%s generation=%s label=%s\n", c.Provider, c.Selection.OwnerInstanceUID, c.Selection.ConnectionUID, c.Selection.Generation, textsafe.Line(c.Label)); err != nil {
				return err
			}
		}
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "receipt <uid>", Short: "read a retained operation outcome after a timeout", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !uid.Valid(args[0]) {
			return trackerCLIValidation("operation UID must be a normalized ULID")
		}
		a, project, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, err := client.TrackerOperationReceipt(a.ctx, project, args[0])
		if err != nil {
			return err
		}
		return printTrackerReceipt(cmd, response)
	}})
	var operation, provider, resource, owner, connection, generation, settings, status, interval, expectedUpdated string
	effect := &cobra.Command{Use: "operation <bind|rebind|disable|run|activate|reconcile>", Short: "submit an explicit account operation with a reusable UID", Long: "Submit one tracker operation. Retain --uid and the exact request after a timeout. Omitted account selection preserves the existing named binding; it never chooses a default account. Bind requires nonsecret provider settings in --config-json. Activation first reconciles through provider reads and excludes older pending intents.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		kind := tracker.ControlKind(args[0])
		if kind.Action() == "" || kind == tracker.ControlAcceptRemote || !uid.Valid(operation) || !tracker.ValidIdentityText(resource, 2048) {
			return trackerCLIValidation("valid operation kind, --uid and --resource are required")
		}
		switch provider {
		case "github", "notion", "linear", "plane", "ticktick", "todoist", "twenty":
		default:
			return trackerCLIValidation("choose --provider explicitly")
		}
		body := kataclient.TrackerOperation{UID: operation, Kind: kind, Provider: provider, Resource: resource}
		if owner != "" || connection != "" || generation != "" {
			selection := kataclient.TrackerSelection{OwnerInstanceUID: owner, ConnectionUID: connection, Generation: generation}
			if selection.Validate() != nil {
				return trackerCLIValidation("provide --owner, --connection and --generation together")
			}
			body.Selection = &selection
		}
		if kind == tracker.ControlRebind {
			expected, err := time.Parse(time.RFC3339Nano, expectedUpdated)
			if err != nil || body.Selection == nil {
				return trackerCLIValidation("rebind requires explicit account selection and --expected-updated-at from current sync status")
			}
			body.ExpectedUpdatedAt = &expected
		} else if cmd.Flags().Changed("expected-updated-at") {
			return trackerCLIValidation("--expected-updated-at applies only to rebind")
		}
		if kind == tracker.ControlBind || kind == tracker.ControlRebind {
			options := map[string]any{}
			if json.Unmarshal([]byte(settings), &options) != nil || options == nil {
				return trackerCLIValidation("--config-json must be an object of nonsecret provider settings")
			}
			body.Binding = &kataclient.TrackerBindingOptions{Config: options, Interval: interval}
			if cmd.Flags().Changed("status-sync") {
				if status != "one-way" && status != "two-way" {
					return trackerCLIValidation("--status-sync must be one-way or two-way")
				}
				body.Binding.StatusSync = &status
			}
		} else if cmd.Flags().Changed("config-json") || cmd.Flags().Changed("status-sync") || cmd.Flags().Changed("interval") {
			return trackerCLIValidation("provider settings apply only to bind")
		}
		a, project, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		a.client, err = longRunningClientForResolved(cmd.Context(), a.resolved)
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, err := client.TrackerOperation(a.ctx, project, body)
		if err != nil {
			return err
		}
		if err := trackerOperationOutcomeError(response); err != nil {
			return err
		}
		return printTrackerReceipt(cmd, response)
	}}
	effect.Flags().StringVar(&expectedUpdated, "expected-updated-at", "", "current binding updated_at timestamp from sync status; rebind only")
	effect.Flags().StringVar(&operation, "uid", "", "operation ULID; reuse with the exact request after a timeout")
	effect.Flags().StringVar(&provider, "provider", "", "GitHub, Notion, Linear, Plane, TickTick, Todoist or Twenty (lowercase)")
	effect.Flags().StringVar(&resource, "resource", "", "exact authorized provider resource locator")
	effect.Flags().StringVar(&owner, "owner", "", "selected credential owner's instance UID")
	effect.Flags().StringVar(&connection, "connection", "", "selected connection UID")
	effect.Flags().StringVar(&generation, "generation", "", "selected connection generation")
	effect.Flags().StringVar(&settings, "config-json", "{}", "nonsecret provider settings as a JSON object; bind/rebind only")
	effect.Flags().StringVar(&status, "status-sync", "", "one-way or two-way; bind/rebind only")
	effect.Flags().StringVar(&interval, "interval", "", "polling duration or seconds; bind/rebind only")
	cmd.AddCommand(effect)
	// Normalize typed SDK failures at every tracker command boundary, including
	// errors returned before a retained operation receipt can be decoded.
	for _, action := range cmd.Commands() {
		run := action.RunE
		if run != nil {
			action.RunE = func(command *cobra.Command, args []string) error { return trackerCLIRequestError(run(command, args)) }
		}
	}
	return cmd
}
func trackerCLIValidation(message string) error {
	return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
}
func printTrackerReceipt(cmd *cobra.Command, receipt *kataclient.TrackerReceipt) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), receipt)
	}
	code := ""
	if receipt.Result != nil {
		code = receipt.Result.Code
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "tracker operation uid=%s kind=%s state=%s result=%s\n", receipt.UID, receipt.Kind, receipt.State, strings.TrimSpace(code))
	if err != nil {
		return err
	}
	return nil
}

func trackerOperationOutcomeError(receipt *kataclient.TrackerReceipt) error {
	if receipt.State != tracker.ControlFailed && receipt.State != tracker.ControlRestored {
		return nil
	}
	code := "tracker_unavailable"
	if receipt.Result != nil {
		code = receipt.Result.Code
	}
	return trackerCLIClassifiedError(code, "tracker operation "+receipt.UID+" did not complete")
}

func trackerCLIClassifiedError(code, prefix string) *cliError {
	if !tracker.IsPublicErrorCode(code) {
		code = "tracker_unavailable"
	}
	failure := &cliError{Message: prefix + ": " + code, Code: code, Kind: kindInternal, ExitCode: ExitInternal}
	switch code {
	case "validation", "tracker_selection_required", "tracker_operation_unsupported":
		failure.Kind, failure.ExitCode = kindValidation, ExitValidation
	case "not_found":
		failure.Kind, failure.ExitCode = kindNotFound, ExitNotFound
	case "tracker_timeout", "tracker_unavailable", "tracker_capability_unavailable":
		failure.Kind, failure.ExitCode = kindDaemonUnavail, ExitDaemonUnavail
	default:
		failure.Kind, failure.ExitCode = kindConflict, ExitConflict
	}
	return failure
}

func trackerCLIRequestError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*cliError](err); ok {
		return err
	}
	// Preserve selected-daemon transport outcome-unknown facts and hints.
	if _, ok := errors.AsType[*daemonTransportError](err); ok {
		return err
	}
	code, status := kataclient.ErrorCode(err), kataclient.StatusCode(err)
	if code == "" && errors.Is(err, context.DeadlineExceeded) {
		code = "tracker_timeout"
	}
	if code == "" && status == 0 {
		return err
	}
	failure := trackerCLIClassifiedError(code, "tracker request failed")
	failure.HTTPStatus = status
	return failure
}
