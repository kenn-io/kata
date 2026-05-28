package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	minClaimTTL = time.Minute
	maxClaimTTL = 24 * time.Hour
)

func newFederationLeaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lease",
		Short: "manage federation write leases",
	}
	cmd.AddCommand(newFederationLeaseAcquireCmd(), newFederationLeaseReleaseCmd(),
		newFederationLeaseForceReleaseCmd(), newFederationLeaseStealCmd())
	return cmd
}

func newFederationLeaseAcquireCmd() *cobra.Command {
	var ttlRaw string
	cmd := &cobra.Command{
		Use:   "acquire <issue-ref>",
		Short: "acquire the federation write lease for an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ttl, timed, err := parseClaimTTL(ttlRaw)
			if err != nil {
				return err
			}
			return runClaimAction(cmd, args[0], "acquire", ttl, timed)
		},
	}
	cmd.Flags().StringVar(&ttlRaw, "ttl", "", "timed lease duration (for example 30m or 2h)")
	return cmd
}

func newFederationLeaseReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "release <issue-ref>",
		Short: "release your federation write lease on an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaimAction(cmd, args[0], "release", 0, false)
		},
	}
}

func newFederationLeaseForceReleaseCmd() *cobra.Command {
	cmd := newClaimForceReleaseCmd()
	cmd.Short = "force-release any federation write lease on an issue as an administrator"
	return cmd
}

func newFederationLeaseStealCmd() *cobra.Command {
	cmd := newClaimStealCmd()
	cmd.Short = "force-release an issue lease and acquire it as the current actor"
	return cmd
}

func newClaimForceReleaseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "force-release <issue-ref>",
		Short: "force-release any federation write lease on an issue as an administrator",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			actor, err := explicitClaimAdminActor(cmd)
			if err != nil {
				return err
			}
			reason, err := explicitClaimAdminReason(cmd, reason)
			if err != nil {
				return err
			}
			return runClaimForceRelease(cmd, args[0], actor, reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "force-release reason")
	return cmd
}

func newClaimStealCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "steal <issue-ref>",
		Short: "force-release an issue lease and acquire it as the current actor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			actor, err := explicitClaimAdminActor(cmd)
			if err != nil {
				return err
			}
			reason, err := explicitClaimAdminReason(cmd, reason)
			if err != nil {
				return err
			}
			return runClaimSteal(cmd, args[0], actor, reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "force-release reason")
	return cmd
}

func parseClaimTTL(raw string) (time.Duration, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	unit := raw[len(raw)-1]
	if unit >= '0' && unit <= '9' {
		return 0, false, &cliError{
			Message:  "ttl requires a duration unit (for example 30m, 60s, or 2h)",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	multiplier := time.Duration(0)
	switch unit {
	case 's':
		multiplier = time.Second
	case 'm':
		multiplier = time.Minute
	case 'h':
		multiplier = time.Hour
	}
	n, err := parseWholeClaimTTL(raw[:len(raw)-1])
	if multiplier == 0 || err != nil {
		return 0, false, &cliError{
			Message:  "ttl must be a whole number followed by s, m, or h",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	minUnits := int64(minClaimTTL / multiplier)
	if minClaimTTL%multiplier != 0 {
		minUnits++
	}
	maxUnits := int64(maxClaimTTL / multiplier)
	if n < minUnits || n > maxUnits {
		return 0, false, &cliError{
			Message:  "timed lease TTL must be between 60s and 24h",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	ttl := time.Duration(n) * multiplier
	return ttl, true, nil
}

func parseWholeClaimTTL(s string) (int64, error) {
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func runClaimAction(cmd *cobra.Command, rawRef, action string, ttl time.Duration, timed bool) error {
	actor, _ := resolveActor(flags.As, nil)
	body := map[string]any{
		"holder":      actor,
		"client_kind": "cli",
	}
	if action == "claim" || action == "acquire" {
		if timed {
			body["claim_kind"] = "timed"
			body["ttl_seconds"] = int64(ttl / time.Second)
		} else {
			body["claim_kind"] = "hard"
		}
	}
	bs, ref, err := postClaimAction(cmd, rawRef, action, body)
	if err != nil {
		return err
	}
	return printLeaseMutation(cmd, bs, action, ref, actor)
}

func runClaimForceRelease(cmd *cobra.Command, rawRef, actor, reason string) error {
	body := map[string]any{
		"actor":       actor,
		"reason":      reason,
		"client_kind": "cli",
	}
	bs, ref, err := postClaimAction(cmd, rawRef, "force_release", body)
	if err != nil {
		return err
	}
	return printLeaseMutation(cmd, bs, "force_release", ref, actor)
}

func runClaimSteal(cmd *cobra.Command, rawRef, actor, reason string) error {
	ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, rawRef)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}

	releasedBS, err := postClaimActionResolved(ctx, client, baseURL, pid, issue.RefForAPI, "force_release", map[string]any{
		"actor":       actor,
		"reason":      reason,
		"client_kind": "cli",
	})
	if err != nil {
		return err
	}
	released, err := decodeClaimMutation(releasedBS)
	if err != nil {
		return err
	}

	claimedBS, err := postClaimActionResolved(ctx, client, baseURL, pid, issue.RefForAPI, "acquire", map[string]any{
		"holder":      actor,
		"client_kind": "cli",
		"claim_kind":  "hard",
	})
	if err != nil {
		return printClaimStealPartial(cmd, releasedBS, nil, released, err)
	}
	claimed, err := decodeClaimMutation(claimedBS)
	if err != nil {
		return err
	}
	if err := claimStealSecondClaimErr(issue.RefForAPI, claimed); err != nil {
		return printClaimStealPartial(cmd, releasedBS, claimedBS, released, err)
	}
	return printClaimSteal(cmd, issue.RefForAPI, releasedBS, claimedBS, released, claimed)
}

func postClaimAction(cmd *cobra.Command, rawRef, action string, body map[string]any) ([]byte, string, error) {
	ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, rawRef)
	if err != nil {
		return nil, "", err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return nil, "", err
	}
	bs, err := postClaimActionResolved(ctx, client, baseURL, pid, issue.RefForAPI, action, body)
	if err != nil {
		return nil, "", err
	}
	return bs, issue.RefForAPI, nil
}

func postClaimActionResolved(ctx context.Context, client *http.Client, baseURL string, pid int64, ref, action string, body map[string]any) ([]byte, error) {
	postURL := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/lease/actions/%s",
		baseURL, pid, url.PathEscape(ref), action)
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost, postURL, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, apiErrFromBody(status, bs)
	}
	return bs, nil
}

func explicitClaimAdminActor(cmd *cobra.Command) (string, error) {
	flag := cmd.Flag("as")
	if flag == nil || !flag.Changed || strings.TrimSpace(flags.As) == "" {
		return "", &cliError{
			Message:  "--as is required for administrative claim actions",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	return strings.TrimSpace(flags.As), nil
}

func explicitClaimAdminReason(cmd *cobra.Command, reason string) (string, error) {
	if !cmd.Flags().Changed("reason") || strings.TrimSpace(reason) == "" {
		return "", &cliError{
			Message:  "--reason is required for administrative claim actions",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	return strings.TrimSpace(reason), nil
}

type claimMutationBody struct {
	Granted bool `json:"granted"`
	Pending bool `json:"pending"`
	Holder  struct {
		Holder string `json:"holder"`
	} `json:"holder"`
	Claim *struct {
		Holder string `json:"holder"`
	} `json:"claim"`
}

func decodeClaimMutation(bs []byte) (claimMutationBody, error) {
	var b claimMutationBody
	if err := json.Unmarshal(bs, &b); err != nil {
		return claimMutationBody{}, err
	}
	return b, nil
}

func printLeaseMutation(cmd *cobra.Command, bs []byte, action, ref, actor string) error {
	b, err := decodeClaimMutation(bs)
	if err != nil {
		return err
	}
	deniedErr := claimDeniedErr(action, ref, b.Granted, b.Pending, b.Claim)
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), buf.String()); err != nil {
			return err
		}
		return deniedErr
	}
	if deniedErr != nil {
		return deniedErr
	}
	if flags.Quiet {
		return nil
	}
	if mode == outputAgent {
		return printLeaseMutationAgent(cmd, action, ref, actor, b)
	}
	if action == "release" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "released lease on %s\n", ref)
		return err
	}
	if action == "force_release" {
		holder := holderFromClaimMutation(b)
		if holder != "" {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "force-released lease on %s from %s\n", ref, holder)
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "force-released lease on %s\n", ref)
		return err
	}
	holder := b.Holder.Holder
	if holder == "" {
		holder = actor
	}
	if b.Pending {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "lease pending for %s as %s\n", ref, holder)
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "acquired lease on %s as %s\n", ref, holder)
	return err
}

func printLeaseMutationAgent(cmd *cobra.Command, action, ref, actor string, b claimMutationBody) error {
	verb := leaseAgentVerb(action)
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK %s %s\n", verb, ref); err != nil {
		return err
	}
	switch action {
	case "release":
		return nil
	case "force_release":
		holder := holderFromClaimMutation(b)
		if holder == "" {
			return nil
		}
		return writeAgentField(cmd.OutOrStdout(), "ReleasedHolder", agentValue(holder))
	}
	holder := b.Holder.Holder
	if holder == "" {
		holder = actor
	}
	if err := writeAgentField(cmd.OutOrStdout(), "Holder", agentValue(holder)); err != nil {
		return err
	}
	state := "acquired"
	if b.Pending {
		state = "pending"
	}
	return writeAgentField(cmd.OutOrStdout(), "State", state)
}

func leaseAgentVerb(action string) string {
	switch action {
	case "acquire", "claim":
		return "federation-lease-acquire"
	case "release":
		return "federation-lease-release"
	case "force_release":
		return "federation-lease-force-release"
	}
	return "federation-lease-" + action
}

func printClaimSteal(cmd *cobra.Command, ref string, releasedBS, claimedBS []byte, released, claimed claimMutationBody) error {
	releasedHolder := holderFromClaimMutation(released)
	newHolder := holderFromClaimMutation(claimed)
	if newHolder == "" {
		newHolder = strings.TrimSpace(flags.As)
	}
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		payload := claimStealJSON{
			ReleasedHolder: releasedHolder,
			NewHolder:      newHolder,
			Released:       json.RawMessage(releasedBS),
			Claimed:        json.RawMessage(claimedBS),
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if flags.Quiet {
		return nil
	}
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK federation-lease-steal %s\n", ref); err != nil {
			return err
		}
		if releasedHolder != "" {
			if err := writeAgentField(cmd.OutOrStdout(), "ReleasedHolder", agentValue(releasedHolder)); err != nil {
				return err
			}
		}
		return writeAgentField(cmd.OutOrStdout(), "Holder", agentValue(newHolder))
	}
	if releasedHolder != "" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "stole lease on %s from %s as %s\n", ref, releasedHolder, newHolder)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "stole lease on %s as %s\n", ref, newHolder)
	return err
}

func printClaimStealPartial(cmd *cobra.Command, releasedBS, claimedBS []byte, released claimMutationBody, cause error) error {
	releasedHolder := holderFromClaimMutation(released)
	if currentOutputMode() == outputJSON {
		var cli *cliError
		errorPayload := struct {
			Code    string `json:"code,omitempty"`
			Message string `json:"message"`
		}{Message: cause.Error()}
		if errors.As(cause, &cli) {
			errorPayload.Code = cli.Code
			errorPayload.Message = cli.Message
		}
		var buf bytes.Buffer
		payload := claimStealJSON{
			PartialSuccess: true,
			ReleasedHolder: releasedHolder,
			NewHolder:      strings.TrimSpace(flags.As),
			Released:       json.RawMessage(releasedBS),
			ClaimError:     errorPayload,
		}
		if len(claimedBS) > 0 {
			payload.Claimed = json.RawMessage(claimedBS)
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), buf.String()); err != nil {
			return err
		}
	}
	return claimStealPartialErr(cause)
}

type claimStealJSON struct {
	PartialSuccess bool            `json:"partial_success,omitempty"`
	ReleasedHolder string          `json:"released_holder,omitempty"`
	NewHolder      string          `json:"new_holder,omitempty"`
	Released       json.RawMessage `json:"released,omitempty"`
	Claimed        json.RawMessage `json:"claimed,omitempty"`
	ClaimError     any             `json:"claim_error,omitempty"`
}

func claimStealPartialErr(cause error) error {
	exit := ExitConflict
	kind := kindConflict
	code := "claim_steal_partial"
	var cli *cliError
	if errors.As(cause, &cli) {
		exit = cli.ExitCode
		kind = cli.Kind
	}
	return &cliError{
		Message:  fmt.Sprintf("force-release succeeded but lease failed: %s", cause.Error()),
		Code:     code,
		Kind:     kind,
		ExitCode: exit,
	}
}

func claimStealSecondClaimErr(ref string, claimed claimMutationBody) error {
	if claimed.Pending {
		holder := holderFromClaimMutation(claimed)
		msg := "lease pending after force-release"
		if holder != "" {
			msg = fmt.Sprintf("lease pending after force-release: %s pending for %s", holder, ref)
		}
		return &cliError{
			Message:  msg,
			Code:     "claim_pending",
			Kind:     kindConflict,
			ExitCode: ExitConflict,
		}
	}
	return claimDeniedErr("claim", ref, claimed.Granted, claimed.Pending, claimed.Claim)
}

func holderFromClaimMutation(b claimMutationBody) string {
	if b.Claim != nil && strings.TrimSpace(b.Claim.Holder) != "" {
		return strings.TrimSpace(b.Claim.Holder)
	}
	return strings.TrimSpace(b.Holder.Holder)
}

func claimDeniedErr(action, ref string, granted, pending bool, claim *struct {
	Holder string `json:"holder"`
}) error {
	if (action != "claim" && action != "acquire") || pending || granted {
		return nil
	}
	deniedBy := ""
	if claim != nil {
		deniedBy = strings.TrimSpace(claim.Holder)
	}
	msg := "lease denied"
	if deniedBy != "" {
		msg = fmt.Sprintf("lease denied: %s already holds %s", deniedBy, ref)
	}
	return &cliError{
		Message:  msg,
		Code:     "claim_denied",
		Kind:     kindConflict,
		ExitCode: ExitConflict,
	}
}
