package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

type issueStatusProjection struct {
	Issue             string     `json:"issue"`
	Project           string     `json:"project"`
	IssueStatus       string     `json:"issue_status"`
	Revision          int64      `json:"revision"`
	Actor             string     `json:"actor"`
	ActorSource       string     `json:"actor_source"`
	Auth              string     `json:"auth"`
	Instance          string     `json:"instance"`
	Owner             *string    `json:"owner,omitempty"`
	Claim             string     `json:"claim"`
	Holder            string     `json:"holder,omitempty"`
	HolderInstance    string     `json:"holder_instance,omitempty"`
	LeaseKind         string     `json:"lease_kind,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	PendingLeaseCount int        `json:"pending_lease_count,omitempty"`
}

type instanceStatusForCLI struct {
	InstanceUID string `json:"instance_uid"`
	Auth        struct {
		Kind  string `json:"kind"`
		Actor string `json:"actor"`
	} `json:"auth"`
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <issue-ref>",
		Short: "show compact issue identity and claim status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIssueStatus(cmd, args[0])
		},
	}
}

func runIssueStatus(cmd *cobra.Command, issueRef string) error {
	ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, issueRef)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}

	var show showResponseForCLI
	if err := getStatusPayload(ctx, client,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(ref.RefForAPI)),
		&show); err != nil {
		return err
	}
	var instance instanceStatusForCLI
	if err := getStatusPayload(ctx, client, baseURL+"/api/v1/instance", &instance); err != nil {
		return err
	}

	actor, source := resolveActor(ctx, flags.As, nil)
	if instance.Auth.Actor != "" {
		actor = instance.Auth.Actor
		source = instance.Auth.Kind
	}
	authKind := instance.Auth.Kind
	if authKind == "" {
		authKind = "unknown"
	}
	now := time.Now().UTC()
	if show.LeaseHubNow != nil && !show.LeaseHubNow.IsZero() {
		now = show.LeaseHubNow.UTC()
	}
	projection := issueStatusProjection{
		Issue:             show.Issue.ShortID,
		Project:           ref.ProjectName,
		IssueStatus:       show.Issue.Status,
		Revision:          show.Issue.Revision,
		Actor:             actor,
		ActorSource:       source,
		Auth:              authKind,
		Instance:          instance.InstanceUID,
		Owner:             show.Issue.Owner,
		Claim:             projectedClaimState(show.Issue.Status, show.Issue.Owner, show.Lease, show.PendingLeases, now),
		PendingLeaseCount: len(show.PendingLeases),
	}
	if show.Lease != nil {
		projection.Holder = show.Lease.Holder
		projection.HolderInstance = show.Lease.HolderInstanceUID
		projection.LeaseKind = show.Lease.ClaimKind
		projection.ExpiresAt = show.Lease.ExpiresAt
	}
	return printIssueStatus(cmd, projection)
}

func getStatusPayload(ctx context.Context, client *http.Client, target string, out any) error {
	status, body, err := httpDoJSON(ctx, client, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if status >= http.StatusBadRequest {
		return apiErrFromBody(status, body)
	}
	return json.Unmarshal(body, out)
}

func projectedClaimState(
	issueStatus string,
	owner *string,
	lease *claimForShowCLI,
	pending []pendingClaimForCLI,
	now time.Time,
) string {
	if issueStatus == "closed" {
		return "closed"
	}
	if lease != nil {
		if lease.ClaimKind == "timed" && lease.ExpiresAt != nil && !lease.ExpiresAt.After(now) {
			return "expired"
		}
		return "active"
	}
	if len(pending) > 0 {
		return "pending"
	}
	if owner != nil && *owner != "" {
		return "assigned"
	}
	return "unassigned"
}

func printIssueStatus(cmd *cobra.Command, status issueStatusProjection) error {
	switch currentOutputMode() {
	case outputJSON:
		var out bytes.Buffer
		if err := emitJSON(&out, status); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), out.String())
		return err
	case outputAgent:
		return printIssueStatusAgent(cmd.OutOrStdout(), status)
	default:
		return printIssueStatusHuman(cmd.OutOrStdout(), status)
	}
}

func printIssueStatusAgent(out io.Writer, status issueStatusProjection) error {
	fields := []agentField{
		agentRowField("issue", status.Issue),
		agentRowField("project", status.Project),
		agentRowField("issue_status", status.IssueStatus),
		agentRowField("revision", fmt.Sprint(status.Revision)),
		agentRowField("actor", status.Actor),
		agentRowField("actor_source", status.ActorSource),
		agentRowField("auth", status.Auth),
		agentRowField("instance", status.Instance),
		agentOptionalRowField("owner", status.Owner),
		agentRowField("claim", status.Claim),
		agentOptionalRowField("holder", optionalStatusString(status.Holder)),
		agentOptionalRowField("holder_instance", optionalStatusString(status.HolderInstance)),
		agentOptionalRowField("lease_kind", optionalStatusString(status.LeaseKind)),
	}
	if status.ExpiresAt != nil {
		expires := status.ExpiresAt.UTC().Format(time.RFC3339Nano)
		fields = append(fields, agentRowField("expires_at", expires))
	}
	if status.PendingLeaseCount > 0 {
		fields = append(fields, agentRowField("pending_leases", fmt.Sprint(status.PendingLeaseCount)))
	}
	if _, err := fmt.Fprint(out, "OK status"); err != nil {
		return err
	}
	for _, field := range fields {
		if field.value == nil {
			continue
		}
		if _, err := fmt.Fprintf(out, " %s=%s", field.name, agentValue(*field.value)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out)
	return err
}

func optionalStatusString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func printIssueStatusHuman(out io.Writer, status issueStatusProjection) error {
	if _, err := fmt.Fprintf(out, "%s  [%s]  claim=%s\n",
		textsafe.Line(status.Issue), textsafe.Line(status.IssueStatus), status.Claim); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "actor: %s (%s; auth=%s)\ninstance: %s\n",
		textsafe.Line(status.Actor), textsafe.Line(status.ActorSource),
		textsafe.Line(status.Auth), textsafe.Line(status.Instance)); err != nil {
		return err
	}
	if status.Owner != nil && *status.Owner != "" {
		if _, err := fmt.Fprintln(out, "owner:", textsafe.Line(*status.Owner)); err != nil {
			return err
		}
	}
	if status.Holder != "" {
		if _, err := fmt.Fprintf(out, "lease: %s from instance %s (%s)\n",
			textsafe.Line(status.Holder), textsafe.Line(status.HolderInstance),
			textsafe.Line(status.LeaseKind)); err != nil {
			return err
		}
	}
	return nil
}
