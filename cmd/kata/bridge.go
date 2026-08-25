package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newBridgeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "bridge", Short: "manage issue bindings to external roots"}
	cmd.AddCommand(
		newBridgeBindCmd(),
		newBridgeShowCmd(),
		newBridgeReconcileCmd(),
		newBridgePauseCmd(),
		newBridgeResumeCmd(),
		newBridgeResolveFieldCmd(),
		newBridgeResolveCommentCmd(),
		newBridgeUnbindCmd(),
	)
	return cmd
}

func newBridgeBindCmd() *cobra.Command {
	var connectorInstance, external string
	var publishComments bool
	cmd := &cobra.Command{
		Use:   "bind <issue>",
		Short: "bind an issue to an existing external root",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(connectorInstance) == "" {
				return externalCLIValidation("--connector is required")
			}
			if strings.TrimSpace(external) == "" {
				return externalCLIValidation("--external is required")
			}
			resolved, err := resolveBridgeTarget(cmd, args[0], true)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.BindExternalRootWithResponse(resolved.ctx, &generated.BindExternalRootRequestOptions{
				PathParams: &generated.BindExternalRootPath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Body: &generated.BindExternalRootBody{
					Actor: &resolved.actor, Connector: connectorInstance, External: external, PublishComments: &publishComments,
				},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "bound", nil)
		},
	}
	cmd.Flags().StringVar(&connectorInstance, "connector", "", "configured connector instance")
	cmd.Flags().StringVar(&external, "external", "", "external root locator")
	cmd.Flags().BoolVar(&publishComments, "publish-comments", false, "publish new Kata root comments externally")
	return cmd
}

func newBridgeShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <issue>",
		Short: "show bridge policy and status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveBridgeTarget(cmd, args[0], false)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.GetExternalRootBridgeWithResponse(resolved.ctx, &generated.GetExternalRootBridgeRequestOptions{
				PathParams: &generated.GetExternalRootBridgePath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "show", nil)
		},
	}
}

func newBridgeReconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <issue>",
		Short: "reconcile a bridge now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveBridgeTarget(cmd, args[0], true)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.ReconcileExternalRootBridgeWithResponse(resolved.ctx, &generated.ReconcileExternalRootBridgeRequestOptions{
				PathParams: &generated.ReconcileExternalRootBridgePath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Body:       &generated.ReconcileExternalRootBridgeBody{Actor: &resolved.actor},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return fmt.Errorf("bridge reconcile: empty response")
			}
			return printBridgeRun(cmd, resp.Body, *resp.JSON200)
		},
	}
}

func newBridgePauseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "pause <issue>",
		Short: "pause bridge reconciliation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveBridgeTarget(cmd, args[0], false)
			if err != nil {
				return err
			}
			body := generated.PauseExternalRootBridgeBody{Actor: &resolved.actor}
			if cmd.Flags().Changed("reason") {
				body.Reason = &reason
			}
			resp, callErr := resolved.client.PauseExternalRootBridgeWithResponse(resolved.ctx, &generated.PauseExternalRootBridgeRequestOptions{
				PathParams: &generated.PauseExternalRootBridgePath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Body:       &body,
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "paused", nil)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "safe operator pause reason")
	return cmd
}

func newBridgeResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <issue>",
		Short: "validate and resume bridge reconciliation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveBridgeTarget(cmd, args[0], true)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.ResumeExternalRootBridgeWithResponse(resolved.ctx, &generated.ResumeExternalRootBridgeRequestOptions{
				PathParams: &generated.ResumeExternalRootBridgePath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Body:       &generated.ResumeExternalRootBridgeBody{Actor: &resolved.actor},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "resumed", nil)
		},
	}
}

func newBridgeResolveFieldCmd() *cobra.Command {
	var use string
	cmd := &cobra.Command{
		Use:   "resolve-field <issue> <kata-field>",
		Short: "resolve a changed field conflict explicitly",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if use != "kata" && use != "external" {
				return externalCLIValidation("--use must be kata or external")
			}
			resolved, err := resolveBridgeTarget(cmd, args[0], true)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.ResolveExternalFieldWithResponse(resolved.ctx, &generated.ResolveExternalFieldRequestOptions{
				PathParams: &generated.ResolveExternalFieldPath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Body:       &generated.ResolveExternalFieldBody{Actor: &resolved.actor, KataField: args[1], Use: use},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "field resolved", map[string]string{"field": args[1], "use": use})
		},
	}
	cmd.Flags().StringVar(&use, "use", "", "winning value: kata or external")
	return cmd
}

func newBridgeResolveCommentCmd() *cobra.Command {
	var adopt string
	var retry, skip bool
	cmd := &cobra.Command{
		Use:   "resolve-comment <issue>",
		Short: "resolve uncertain outbound comment delivery",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			adoptSelected := cmd.Flags().Changed("adopt")
			selected := 0
			if adoptSelected {
				selected++
			}
			if retry {
				selected++
			}
			if skip {
				selected++
			}
			if selected != 1 {
				return externalCLIValidation("exactly one of --adopt, --retry, or --skip is required")
			}
			if adoptSelected && adopt == "" {
				return externalCLIValidation("--adopt requires a non-empty external comment ID")
			}
			action := "adopt"
			var externalCommentID *string
			switch {
			case retry:
				action = "retry"
			case skip:
				action = "skip"
			default:
				externalCommentID = &adopt
			}
			resolved, err := resolveBridgeTargetWithOptions(cmd, args[0], true, true)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.ResolveExternalCommentWithResponse(resolved.ctx, &generated.ResolveExternalCommentRequestOptions{
				PathParams: &generated.ResolveExternalCommentPath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Body: &generated.ResolveExternalCommentBody{
					Action: action, Actor: &resolved.actor, ExternalCommentID: externalCommentID,
				},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "comment resolved", map[string]string{"resolution": action})
		},
	}
	cmd.Flags().StringVar(&adopt, "adopt", "", "adopt the exact external comment ID")
	cmd.Flags().BoolVar(&retry, "retry", false, "retry outbound comment publication")
	cmd.Flags().BoolVar(&skip, "skip", false, "skip the pending comment")
	return cmd
}

func newBridgeUnbindCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unbind <issue>",
		Short: "stop reconciliation while preserving history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveBridgeTargetWithOptions(cmd, args[0], false, true)
			if err != nil {
				return err
			}
			resp, callErr := resolved.client.UnbindExternalRootWithResponse(resolved.ctx, &generated.UnbindExternalRootRequestOptions{
				PathParams: &generated.UnbindExternalRootPath{ProjectID: resolved.projectID, Ref: resolved.issue.RefForAPI},
				Query:      &generated.UnbindExternalRootQuery{Actor: &resolved.actor},
			})
			if err := externalCLITransportError(resp, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
				return err
			}
			return printBridgeResponse(cmd, resp.Body, resp.JSON200, "unbound", nil)
		},
	}
}

type bridgeTarget struct {
	ctx       context.Context
	client    *kataclient.Client
	projectID int64
	issue     resolvedIssueRef
	actor     string
}

func resolveBridgeTarget(cmd *cobra.Command, rawRef string, longRunning bool) (bridgeTarget, error) {
	return resolveBridgeTargetWithOptions(cmd, rawRef, longRunning, false)
}

func resolveBridgeTargetWithOptions(
	cmd *cobra.Command,
	rawRef string,
	longRunning, includeArchivedProject bool,
) (bridgeTarget, error) {
	ctx, baseURL, projectID, issue, err := resolveIssueRefForCommandWithOptions(
		cmd, rawRef, includeArchivedProject,
	)
	if err != nil {
		return bridgeTarget{}, err
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	var httpClient *http.Client
	if longRunning {
		httpClient, err = longRunningClientFor(ctx, baseURL)
	} else {
		httpClient, err = httpClientFor(ctx, baseURL)
	}
	if err != nil {
		return bridgeTarget{}, err
	}
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, httpClient)
	if err != nil {
		return bridgeTarget{}, err
	}
	return bridgeTarget{ctx: ctx, client: apiClient, projectID: projectID, issue: issue, actor: actor}, nil
}

func printBridgeResponse(cmd *cobra.Command, raw []byte, bridge *generated.ExternalRootBridgeOut, action string, extra map[string]string) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), json.RawMessage(raw))
	}
	if bridge == nil {
		return fmt.Errorf("bridge %s: empty response", action)
	}
	if flags.Quiet {
		return nil
	}
	if currentOutputMode() == outputAgent {
		return printBridgeAgent(cmd, *bridge, extra)
	}
	if action != "show" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Bridge %s.\n", textsafe.Line(action)); err != nil {
			return err
		}
	}
	if len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for key := range extra {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", humanExternalLabel(key), textsafe.Line(extra[key])); err != nil {
				return err
			}
		}
	}
	return printBridgeHuman(cmd, *bridge)
}

func printBridgeHuman(cmd *cobra.Command, bridge generated.ExternalRootBridgeOut) error {
	w := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(w,
		"Connector: %s\nState: %s\nReceive comments: %t\nPublish comments: %t\nComplete external: %t\nPaused reason: %s\nLast success: %s\nLast error: %s\nPending comment: %s\n",
		textsafe.Line(bridge.ConnectorInstance), bridgeState(bridge), bridge.ReceiveComments, bridge.PublishComments,
		bridge.CompleteExternal, textsafe.Line(optionalExternalString(bridge.PausedReason)),
		externalTime(bridge.LastSuccessAt), textsafe.Line(optionalExternalString(bridge.LastError)),
		textsafe.Line(optionalExternalString(bridge.PendingCommentUID))); err != nil {
		return err
	}
	conflicts := sortedBridgeConflicts(bridge.FieldConflicts)
	for _, conflict := range conflicts {
		if _, err := fmt.Fprintf(w, "Field conflict: %s\n  Kata: %s\n  External: %s\n",
			textsafe.Line(conflict.KataField), externalCandidateHuman(conflict.KataCandidate),
			externalCandidateHuman(conflict.ExternalCandidate)); err != nil {
			return err
		}
	}
	return nil
}

func printBridgeAgent(cmd *cobra.Command, bridge generated.ExternalRootBridgeOut, extra map[string]string) error {
	w := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(w, "instance=%s state=%s receive_comments=%t publish_comments=%t pending_comment=%s last_error=%s",
		agentValue(bridge.ConnectorInstance), bridgeState(bridge), bridge.ReceiveComments, bridge.PublishComments,
		agentValue(optionalExternalString(bridge.PendingCommentUID)), agentValue(optionalExternalString(bridge.LastError))); err != nil {
		return err
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, " %s=%s", key, agentValue(extra[key])); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, conflict := range sortedBridgeConflicts(bridge.FieldConflicts) {
		if _, err := fmt.Fprintf(w, "field=%s %s %s\n", agentValue(conflict.KataField),
			externalCandidateAgent("kata", conflict.KataCandidate),
			externalCandidateAgent("external", conflict.ExternalCandidate)); err != nil {
			return err
		}
	}
	return nil
}

func printBridgeRun(cmd *cobra.Command, raw []byte, run generated.ExternalRootRunOut) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), json.RawMessage(raw))
	}
	if flags.Quiet {
		return nil
	}
	if currentOutputMode() == outputAgent {
		if err := printBridgeAgent(cmd, run.Bridge, map[string]string{
			"comments_created": fmt.Sprint(run.CommentsCreated), "comments_edited": fmt.Sprint(run.CommentsEdited),
			"completion_requests": fmt.Sprint(run.CompletionRequests), "field_conflicts": fmt.Sprint(run.FieldConflicts),
			"paused": fmt.Sprint(run.Paused), "reopen_requests": fmt.Sprint(run.ReopenRequests),
			"root_updated": fmt.Sprint(run.RootUpdated),
		}); err != nil {
			return err
		}
		return nil
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"Reconciled bridge.\nRoot updated: %t\nComments created: %d\nComments edited: %d\nField conflicts: %d\nCompletion requests: %d\nReopen requests: %d\nPaused: %t\n",
		run.RootUpdated, run.CommentsCreated, run.CommentsEdited, run.FieldConflicts,
		run.CompletionRequests, run.ReopenRequests, run.Paused); err != nil {
		return err
	}
	return printBridgeHuman(cmd, run.Bridge)
}

func bridgeState(bridge generated.ExternalRootBridgeOut) string {
	switch {
	case !bridge.Active:
		return "unbound"
	case bridge.Enabled:
		return "enabled"
	default:
		return "paused"
	}
}

func externalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func sortedBridgeConflicts(conflicts []generated.ExternalFieldConflictOut) []generated.ExternalFieldConflictOut {
	out := append([]generated.ExternalFieldConflictOut(nil), conflicts...)
	sort.Slice(out, func(i, j int) bool { return out[i].KataField < out[j].KataField })
	return out
}

func externalCandidateHuman(candidate *generated.ExternalFieldCandidateOut) string {
	if candidate == nil {
		return "kind=- value=- timezone=-"
	}
	return fmt.Sprintf("kind=%s value=%s timezone=%s", textsafe.Line(candidate.Kind),
		textsafe.Line(optionalExternalString(candidate.Value)), textsafe.Line(optionalExternalString(candidate.Timezone)))
}

func externalCandidateAgent(prefix string, candidate *generated.ExternalFieldCandidateOut) string {
	if candidate == nil {
		return fmt.Sprintf("%s_kind=- %s_value=- %s_timezone=-", prefix, prefix, prefix)
	}
	return fmt.Sprintf("%s_kind=%s %s_value=%s %s_timezone=%s", prefix, agentValue(candidate.Kind),
		prefix, agentValue(optionalExternalString(candidate.Value)), prefix, agentValue(optionalExternalString(candidate.Timezone)))
}

func humanExternalLabel(key string) string {
	parts := strings.Split(key, "_")
	for i := range parts {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, " ")
}
