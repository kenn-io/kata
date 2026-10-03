package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/pkg/client/generated"
)

// AssignInput names the issue and its new owner.
type AssignInput struct {
	Ref   string `json:"ref"`
	Owner string `json:"owner" jsonschema:"Actor that will own the issue"`
}

// UnassignInput names the issue whose owner to clear.
type UnassignInput struct {
	Ref           string  `json:"ref"`
	ExpectedOwner *string `json:"expected_owner,omitempty" jsonschema:"Clear only while this actor still owns the issue"`
}

// InboxInput selects whose attention requests to read.
type InboxInput struct {
	For     string `json:"for" jsonschema:"Recipient actor, or actor/teammate"`
	Project string `json:"project,omitempty" jsonschema:"Project name; omit to read every project in scope"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum issues to read; default 20"`
}

// InboxRequest is one open issue asking for the recipient's attention.
type InboxRequest struct {
	Ref          string  `json:"ref"`
	QualifiedRef string  `json:"qualified_ref"`
	WebURL       *string `json:"web_url,omitempty"`
	Title        string  `json:"title"`
	From         string  `json:"from"`
	Teammate     string  `json:"teammate,omitempty"`
	Message      string  `json:"message"`
}

// InboxOutput lists attention requests without clearing them.
type InboxOutput struct {
	Recipient string         `json:"recipient"`
	Requests  []InboxRequest `json:"requests"`
	Truncated bool           `json:"truncated" jsonschema:"More open issues carry requests than the limit allowed"`
}

// StatusInput selects one issue.
type StatusInput struct {
	Ref string `json:"ref"`
}

// StatusOutput reports who holds an issue and who this server writes as.
type StatusOutput struct {
	Project             ProjectIdentity `json:"project"`
	Ref                 string          `json:"ref"`
	QualifiedRef        string          `json:"qualified_ref"`
	Status              string          `json:"status"`
	Revision            int64           `json:"revision"`
	Owner               *string         `json:"owner,omitempty"`
	AssignmentExpiresOn *string         `json:"assignment_expires_on,omitempty"`
	Hold                string          `json:"hold" jsonschema:"closed, active, expired, pending, assigned, or unassigned"`
	Holder              string          `json:"holder,omitempty"`
	HolderInstance      string          `json:"holder_instance,omitempty"`
	LeaseKind           string          `json:"lease_kind,omitempty"`
	LeaseExpiresAt      *string         `json:"lease_expires_at,omitempty"`
	PendingLeaseCount   int             `json:"pending_lease_count"`
	Actor               string          `json:"actor" jsonschema:"Actor this server writes as"`
	ActorSource         string          `json:"actor_source" jsonschema:"startup or daemon"`
	AuthKind            string          `json:"auth_kind"`
	Instance            string          `json:"instance"`
}

func registerCoordinationTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	mutating := toolHints(false, true, false)
	addTool(server, "kata.assign", "Assign issue", "Make another actor the owner of an issue.", mutating, handlers.assign)
	addTool(server, "kata.inbox", "Read inbox", "List open issues that ask a recipient for attention. Reading does not clear them.", read, handlers.inbox)
	addTool(server, "kata.status", "Issue status", "Show who owns or holds an issue and which actor this server writes as.", read, handlers.status)
	addTool(server, "kata.unassign", "Unassign issue", "Clear an issue's owner, optionally only while an expected owner still holds it.", mutating, handlers.unassign)
}

func (h toolHandlers) assign(ctx context.Context, _ *sdkmcp.CallToolRequest, input AssignInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	owner := strings.TrimSpace(input.Owner)
	if owner == "" {
		return nil, MutationOutput{}, errors.New("owner must not be empty")
	}
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.AssignIssue(ctx, &generated.AssignIssueRequestOptions{
		PathParams: &generated.AssignIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.AssignIssueBody{Actor: &h.options.Actor, Owner: owner},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) unassign(ctx context.Context, _ *sdkmcp.CallToolRequest, input UnassignInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	var expected *string
	if input.ExpectedOwner != nil {
		if expected = optionalString(*input.ExpectedOwner); expected == nil {
			return nil, MutationOutput{}, errors.New("expected_owner must not be empty")
		}
	}
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.UnassignIssue(ctx, &generated.UnassignIssueRequestOptions{
		PathParams: &generated.UnassignIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.UnassignIssueBody{Actor: &h.options.Actor, ExpectedOwner: expected},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) inbox(ctx context.Context, request *sdkmcp.CallToolRequest, input InboxInput) (*sdkmcp.CallToolResult, InboxOutput, error) {
	recipient, err := notification.NormalizeRecipient(input.For)
	if err != nil {
		return nil, InboxOutput{}, err
	}
	key := notification.MetadataKey(recipient)
	_, listed, err := h.list(ctx, request, ListInput{Project: input.Project, Status: "open", Metadata: []string{key}, Limit: input.Limit})
	if err != nil {
		return nil, InboxOutput{}, err
	}
	output := InboxOutput{Recipient: recipient, Requests: make([]InboxRequest, 0, len(listed.Issues)), Truncated: listed.Truncated}
	for _, issue := range listed.Issues {
		// Skip malformed requests the same way the CLI inbox does.
		var value struct {
			From     string `json:"from"`
			Message  string `json:"message"`
			Teammate any    `json:"teammate"`
		}
		encoded, err := json.Marshal(issue.metadata[key])
		if err != nil || json.Unmarshal(encoded, &value) != nil ||
			strings.TrimSpace(value.From) == "" || strings.TrimSpace(value.Message) == "" {
			continue
		}
		handle, _ := value.Teammate.(string)
		if teammate.Validate(handle) != nil {
			handle = ""
		}
		output.Requests = append(output.Requests, InboxRequest{
			Ref: issue.Ref, QualifiedRef: issue.QualifiedRef, WebURL: issue.WebURL, Title: issue.Title,
			From: value.From, Teammate: handle, Message: value.Message,
		})
	}
	return successResult(), output, nil
}

func (h toolHandlers) status(ctx context.Context, _ *sdkmcp.CallToolRequest, input StatusInput) (*sdkmcp.CallToolResult, StatusOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, false)
	if err != nil {
		return nil, StatusOutput{}, err
	}
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
	})
	if err != nil {
		return nil, StatusOutput{}, err
	}
	instance, err := h.options.Client.Instance(ctx)
	if err != nil {
		return nil, StatusOutput{}, err
	}
	issue := shown.Issue
	output := StatusOutput{
		Project: project, Ref: issue.ShortID, QualifiedRef: project.Name + "#" + issue.ShortID,
		Status: issue.Status, Revision: issue.Revision, Owner: issue.Owner,
		AssignmentExpiresOn: formatOptionalTime(issue.AssignmentExpiresOn),
		PendingLeaseCount:   len(shown.PendingLeases),
		Actor:               h.options.Actor, ActorSource: "startup",
		AuthKind: instance.Auth.Kind, Instance: instance.InstanceUID,
	}
	if instance.Auth.Actor != nil && *instance.Auth.Actor != "" {
		output.Actor, output.ActorSource = *instance.Auth.Actor, "daemon"
	}
	if output.AuthKind == "" {
		output.AuthKind = "unknown"
	}
	now := time.Now().UTC()
	if shown.LeaseHubNow != nil && !shown.LeaseHubNow.IsZero() {
		now = shown.LeaseHubNow.UTC()
	}
	output.Hold = issueHold(issue.Status, issue.Owner, issue.AssignmentExpiresOn, shown.Lease, len(shown.PendingLeases), now)
	if lease := shown.Lease; lease != nil {
		output.Holder, output.HolderInstance, output.LeaseKind = lease.Holder, lease.HolderInstanceUID, lease.ClaimKind
		output.LeaseExpiresAt = formatOptionalTime(lease.ExpiresAt)
	}
	return successResult(), output, nil
}

// issueHold matches the CLI status hold projection.
func issueHold(status string, owner *string, assignmentExpiresOn *time.Time, lease *generated.IssueClaimOut, pending int, now time.Time) string {
	switch {
	case status == "closed":
		return "closed"
	case lease != nil && lease.ClaimKind == "timed" && lease.ExpiresAt != nil && !lease.ExpiresAt.After(now):
		return "expired"
	case lease != nil:
		return "active"
	case pending > 0:
		return "pending"
	case owner == nil || *owner == "":
		return "unassigned"
	case assignmentExpiresOn != nil && !assignmentExpiresOn.After(now):
		return "expired"
	default:
		return "assigned"
	}
}
