package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// LeaseStatusInput selects an issue lease to inspect.
type LeaseStatusInput struct {
	Ref string `json:"ref"`
}

// LeaseInput selects a lease action for the startup actor.
type LeaseInput struct {
	Ref        string `json:"ref"`
	Action     string `json:"action"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
}

// LeaseForceReleaseInput selects a lease for administrative release.
type LeaseForceReleaseInput struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// LeaseStealInput selects a lease for resumable takeover.
type LeaseStealInput struct {
	Ref        string `json:"ref"`
	Reason     string `json:"reason"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
}

// LeaseOutput reports current lease state.
type LeaseOutput struct {
	Project        ProjectIdentity `json:"project"`
	Ref            string          `json:"ref"`
	Held           bool            `json:"held"`
	Granted        bool            `json:"granted,omitempty"`
	Pending        bool            `json:"pending,omitempty"`
	RequestUID     string          `json:"request_uid,omitempty"`
	Holder         string          `json:"holder,omitempty"`
	PreviousHolder string          `json:"previous_holder,omitempty"`
	ClaimKind      string          `json:"claim_kind,omitempty"`
	Purpose        string          `json:"purpose,omitempty"`
	ExpiresAt      string          `json:"expires_at,omitempty"`
	Revision       int64           `json:"revision,omitempty"`
	Event          *EventSummary   `json:"event,omitempty"`
}

// FederationStatusInput requests scoped federation status.
type FederationStatusInput struct{}

// FederationEnrollmentSummary is a secret-free enrollment record.
type FederationEnrollmentSummary struct {
	ID               int64  `json:"id"`
	ProjectID        int64  `json:"project_id,omitempty"`
	SpokeInstanceUID string `json:"spoke_instance_uid"`
	Capabilities     string `json:"capabilities"`
	Actor            string `json:"actor"`
	Revoked          bool   `json:"revoked"`
}

// FederationStatusOutput contains scoped federation status and enrollments.
type FederationStatusOutput struct {
	Statuses    []generated.FederationProjectStatus `json:"statuses"`
	Enrollments []FederationEnrollmentSummary       `json:"enrollments"`
}

// FederationEnrollmentRevokeInput selects an enrollment to revoke.
type FederationEnrollmentRevokeInput struct {
	ID int64 `json:"id"`
}

// FederationEnrollmentRevokeOutput reports enrollment revocation.
type FederationEnrollmentRevokeOutput struct {
	ID      int64 `json:"id"`
	Revoked bool  `json:"revoked"`
}

// FederationRebindInput selects a spoke and configured hub catalog entry.
type FederationRebindInput struct {
	Project    string `json:"project"`
	HubCatalog string `json:"hub_catalog"`
}

// FederationRebindOutput reports a completed spoke rebind.
type FederationRebindOutput struct {
	Result generated.RebindFederationReplicaResponseBody `json:"result"`
}

// FederationLeaveInput selects one resumable leave phase.
type FederationLeaveInput struct {
	Project     string `json:"project"`
	Phase       string `json:"phase"`
	Disposition string `json:"disposition,omitempty"`
	Force       bool   `json:"force,omitempty"`
	Confirm     string `json:"confirm,omitempty"`
}

// FederationLeaveOutput reports a federation leave phase.
type FederationLeaveOutput struct {
	Project           ProjectSummary                                `json:"project"`
	Phase             string                                        `json:"phase"`
	Disposition       string                                        `json:"disposition"`
	Detached          bool                                          `json:"detached"`
	Archived          bool                                          `json:"archived,omitempty"`
	PendingEnrollment *generated.PendingFederationEnrollmentCleanup `json:"pending_enrollment,omitempty"`
}

// FederationQuarantineInput selects a quarantine resolution action.
type FederationQuarantineInput struct {
	Project string `json:"project"`
	ID      int64  `json:"id"`
	Action  string `json:"action"`
	Confirm string `json:"confirm"`
	Reason  string `json:"reason,omitempty"`
}

// FederationQuarantineOutput reports the updated quarantine batch.
type FederationQuarantineOutput struct {
	Project    ProjectIdentity                       `json:"project"`
	Quarantine generated.FederationQuarantineSummary `json:"quarantine"`
}

func registerLeaseTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	destructive := toolHints(false, true, false)
	addTool(server, "kata.lease", "Manage lease", "Acquire, renew, or release the startup actor's federation write lease; renew extends the expiry on every call.", nonIdempotent(toolHints(false, true, false)), handlers.lease)
	addTool(server, "kata.lease_force_release", "Force-release lease", "Administratively release another lease with a reason.", destructive, handlers.leaseForceRelease)
	addTool(server, "kata.lease_status", "Lease status", "Read the current federation write lease for an issue.", read, handlers.leaseStatus)
	addTool(server, "kata.lease_steal", "Steal lease", "Resumably force-release and acquire a lease as the startup actor.", destructive, handlers.leaseSteal)
}

func registerFederationTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, true)
	mutating := toolHints(false, true, true)
	addTool(server, "kata.federation_status", "Federation status", "Read in-scope federation, enrollment, replica, and quarantine status without secrets.", read, handlers.federationStatus)
	addTool(server, "kata.federation_enrollment_revoke", "Revoke enrollment", "Revoke a federation enrollment by ID.", mutating, handlers.federationEnrollmentRevoke)
	addTool(server, "kata.federation_rebind", "Rebind federation", "Rebind a spoke to a configured hub catalog entry.", mutating, handlers.federationRebind)
	addTool(server, "kata.federation_leave", "Leave federation", "Run the preflight, prepare, or commit phase of resumable federation leave.", mutating, handlers.federationLeave)
	addTool(server, "kata.federation_quarantine", "Resolve quarantine", "Retry or skip a federation quarantine batch after exact confirmation.", mutating, handlers.federationQuarantine)
}

func (h toolHandlers) leaseStatus(ctx context.Context, _ *sdkmcp.CallToolRequest, input LeaseStatusInput) (*sdkmcp.CallToolResult, LeaseOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, false)
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	response, err := h.options.Client.GetIssueLeaseStatus(ctx, &generated.GetIssueLeaseStatusRequestOptions{
		PathParams: &generated.GetIssueLeaseStatusPath{ProjectID: project.ID, Ref: ref},
	})
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	output := LeaseOutput{Project: project, Ref: project.Name + "#" + ref, Held: response.Held, Holder: response.Holder.Holder}
	fillLease(&output, response.Lease)
	return successResult(), output, nil
}

func (h toolHandlers) lease(ctx context.Context, _ *sdkmcp.CallToolRequest, input LeaseInput) (*sdkmcp.CallToolResult, LeaseOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	body, err := h.leaseActionBody(input.TTLSeconds, input.Purpose)
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	var response *generated.ClaimActionResponseBody
	switch input.Action {
	case "acquire":
		response, err = h.options.Client.AcquireIssueLease(ctx, &generated.AcquireIssueLeaseRequestOptions{
			PathParams: &generated.AcquireIssueLeasePath{ProjectID: project.ID, Ref: ref}, Body: body,
		})
	case "renew":
		response, err = h.options.Client.RenewIssueLease(ctx, &generated.RenewIssueLeaseRequestOptions{
			PathParams: &generated.RenewIssueLeasePath{ProjectID: project.ID, Ref: ref}, Body: body,
		})
	case "release":
		response, err = h.options.Client.ReleaseIssueLease(ctx, &generated.ReleaseIssueLeaseRequestOptions{
			PathParams: &generated.ReleaseIssueLeasePath{ProjectID: project.ID, Ref: ref}, Body: body,
		})
	default:
		return nil, LeaseOutput{}, errors.New("action must be acquire, renew, or release")
	}
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	return successResult(), leaseMutationOutput(project, ref, response), nil
}

func (h toolHandlers) leaseForceRelease(ctx context.Context, _ *sdkmcp.CallToolRequest, input LeaseForceReleaseInput) (*sdkmcp.CallToolResult, LeaseOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		return nil, LeaseOutput{}, errors.New("reason is required")
	}
	clientKind := "mcp"
	response, err := h.options.Client.ForceReleaseIssueLease(ctx, &generated.ForceReleaseIssueLeaseRequestOptions{
		PathParams: &generated.ForceReleaseIssueLeasePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.ForceReleaseIssueLeaseBody{Actor: &h.options.Actor, Reason: &reason, ClientKind: &clientKind},
	})
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	return successResult(), leaseMutationOutput(project, ref, response), nil
}

// leaseSteal acquires first: the daemon re-grants a lease already held by the
// startup principal, so a retry after a lost response keeps the lease instead
// of force-releasing it and racing other claimants. Only a denied acquire
// force-releases the reported holder before one more acquire attempt.
func (h toolHandlers) leaseSteal(ctx context.Context, _ *sdkmcp.CallToolRequest, input LeaseStealInput) (*sdkmcp.CallToolResult, LeaseOutput, error) {
	acquire := LeaseInput{Ref: input.Ref, Action: "acquire", TTLSeconds: input.TTLSeconds, Purpose: input.Purpose}
	_, acquired, err := h.lease(ctx, nil, acquire)
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	if acquired.Granted || acquired.Pending {
		return successResult(), acquired, nil
	}
	previous := acquired.Holder
	if _, _, err := h.leaseForceRelease(ctx, nil, LeaseForceReleaseInput{Ref: input.Ref, Reason: input.Reason}); err != nil {
		return nil, LeaseOutput{}, err
	}
	_, acquired, err = h.lease(ctx, nil, acquire)
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	acquired.PreviousHolder = previous
	if !acquired.Granted && !acquired.Pending {
		return nil, LeaseOutput{}, fmt.Errorf("lease steal force-release succeeded but lease acquisition was denied for %s", acquired.Ref)
	}
	return successResult(), acquired, nil
}

func (h toolHandlers) leaseActionBody(ttlSeconds int64, purpose string) (*generated.ClaimActionBody, error) {
	if ttlSeconds != 0 && (ttlSeconds < 60 || ttlSeconds > int64((24*time.Hour)/time.Second)) {
		return nil, errors.New("ttl_seconds must be between 60 and 86400 when set")
	}
	holder := h.options.Actor
	clientKind := "mcp"
	claimKind := "hard"
	var ttl *int64
	if ttlSeconds > 0 {
		claimKind = "timed"
		ttl = &ttlSeconds
	}
	return &generated.ClaimActionBody{
		Holder: &holder, ClientKind: &clientKind, ClaimKind: &claimKind,
		TTLSeconds: ttl, Purpose: optionalString(purpose),
	}, nil
}

func leaseMutationOutput(project ProjectIdentity, ref string, response *generated.ClaimActionResponseBody) LeaseOutput {
	output := LeaseOutput{
		Project: project, Ref: project.Name + "#" + ref,
		Held:    response.Lease != nil && response.Lease.ReleasedAt == nil,
		Granted: response.Granted, Holder: response.Holder.Holder, Event: eventSummary(response.Event),
	}
	if response.Pending != nil {
		output.Pending = *response.Pending
	}
	if response.RequestUID != nil {
		output.RequestUID = *response.RequestUID
	}
	fillLease(&output, response.Lease)
	return output
}

func fillLease(output *LeaseOutput, lease *generated.IssueClaimOut) {
	if lease == nil {
		return
	}
	output.Holder = lease.Holder
	output.ClaimKind = lease.ClaimKind
	output.Purpose = lease.Purpose
	output.Revision = lease.Revision
	if lease.ExpiresAt != nil {
		output.ExpiresAt = formatTime(*lease.ExpiresAt)
	}
}

func (h toolHandlers) federationStatus(ctx context.Context, _ *sdkmcp.CallToolRequest, _ FederationStatusInput) (*sdkmcp.CallToolResult, FederationStatusOutput, error) {
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, false)
	if err != nil {
		return nil, FederationStatusOutput{}, err
	}
	allowed := make(map[int64]struct{}, len(projects))
	for _, project := range projects {
		allowed[project.ID] = struct{}{}
	}
	status, err := h.options.Client.GetFederationStatus(ctx, &generated.GetFederationStatusRequestOptions{})
	if err != nil {
		return nil, FederationStatusOutput{}, err
	}
	statuses := make([]generated.FederationProjectStatus, 0, len(status.Statuses))
	for _, item := range status.Statuses {
		if _, ok := allowed[item.ProjectID]; ok {
			statuses = append(statuses, item)
		}
	}
	list, err := h.options.Client.ListFederationEnrollments(ctx)
	if err != nil {
		return nil, FederationStatusOutput{}, err
	}
	enrollments := make([]FederationEnrollmentSummary, 0, len(list.Enrollments))
	for _, item := range list.Enrollments {
		if item.ProjectID == 0 && h.options.Scope.Mode() != ScopeAll {
			continue
		}
		if item.ProjectID != 0 {
			if _, ok := allowed[item.ProjectID]; !ok {
				continue
			}
		}
		enrollments = append(enrollments, federationEnrollmentSummary(item))
	}
	return successResult(), FederationStatusOutput{Statuses: h.redactFederationErrors(statuses), Enrollments: enrollments}, nil
}

// Federation error text can quote refused events verbatim, including issue
// references in other projects, so scoped servers replace it with a stable
// notice instead of the raw daemon prose.
const scopedFederationErrorNotice = "error detail withheld in scoped MCP mode; inspect with the CLI or a daemon-wide server"

func (h toolHandlers) redactFederationErrors(statuses []generated.FederationProjectStatus) []generated.FederationProjectStatus {
	if h.options.Scope.Mode() == ScopeAll {
		return statuses
	}
	for index := range statuses {
		if statuses[index].LastError != nil {
			notice := scopedFederationErrorNotice
			statuses[index].LastError = &notice
		}
		for quarantine := range statuses[index].ActiveQuarantines {
			statuses[index].ActiveQuarantines[quarantine].ErrorData = scopedFederationErrorNotice
		}
	}
	return statuses
}

func (h toolHandlers) redactQuarantineError(summary generated.FederationQuarantineSummary) generated.FederationQuarantineSummary {
	if h.options.Scope.Mode() != ScopeAll {
		summary.ErrorData = scopedFederationErrorNotice
	}
	return summary
}

func (h toolHandlers) federationEnrollmentRevoke(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationEnrollmentRevokeInput) (*sdkmcp.CallToolResult, FederationEnrollmentRevokeOutput, error) {
	if input.ID <= 0 {
		return nil, FederationEnrollmentRevokeOutput{}, errors.New("id must be positive")
	}
	// Archiving a project retains its enrollments and their credentials, so
	// revocation authorizes against the scope including archived members;
	// federationStatus lists only active projects. Global enrollments
	// (project_id 0) stay daemon-wide only.
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, true)
	if err != nil {
		return nil, FederationEnrollmentRevokeOutput{}, err
	}
	scoped := make(map[int64]struct{}, len(projects))
	for _, project := range projects {
		scoped[project.ID] = struct{}{}
	}
	list, err := h.options.Client.ListFederationEnrollments(ctx)
	if err != nil {
		return nil, FederationEnrollmentRevokeOutput{}, err
	}
	allowed := false
	for _, enrollment := range list.Enrollments {
		if enrollment.ID != input.ID {
			continue
		}
		if enrollment.ProjectID == 0 {
			allowed = h.options.Scope.Mode() == ScopeAll
		} else {
			_, allowed = scoped[enrollment.ProjectID]
		}
		break
	}
	if !allowed {
		return nil, FederationEnrollmentRevokeOutput{}, errors.New("enrollment is outside the MCP startup scope")
	}
	response, err := h.options.Client.RevokeFederationEnrollment(ctx, &generated.RevokeFederationEnrollmentRequestOptions{PathParams: &generated.RevokeFederationEnrollmentPath{EnrollmentID: input.ID}})
	if err != nil {
		return nil, FederationEnrollmentRevokeOutput{}, err
	}
	return successResult(), FederationEnrollmentRevokeOutput{ID: response.ID, Revoked: response.Revoked}, nil
}

func (h toolHandlers) federationRebind(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationRebindInput) (*sdkmcp.CallToolResult, FederationRebindOutput, error) {
	// Rebinding routes the replica's enrollment bearer token to whichever
	// configured catalog origin the caller selects, so it stays an
	// operator-tier action.
	if err := h.requireAllProjectsScope("federation rebind"); err != nil {
		return nil, FederationRebindOutput{}, err
	}
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, FederationRebindOutput{}, err
	}
	if strings.TrimSpace(input.HubCatalog) == "" {
		return nil, FederationRebindOutput{}, errors.New("hub_catalog is required")
	}
	response, err := h.options.Client.RebindFederationReplica(ctx, &generated.RebindFederationReplicaRequestOptions{PathParams: &generated.RebindFederationReplicaPath{ProjectID: project.ID}, Body: &generated.RebindFederationReplicaBody{HubCatalog: input.HubCatalog}})
	if err != nil {
		return nil, FederationRebindOutput{}, err
	}
	return successResult(), FederationRebindOutput{Result: *response}, nil
}

func (h toolHandlers) federationLeave(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationLeaveInput) (*sdkmcp.CallToolResult, FederationLeaveOutput, error) {
	phase := strings.TrimSpace(input.Phase)
	if phase == "" {
		return nil, FederationLeaveOutput{}, errors.New("phase is required")
	}
	if phase != "preflight" && phase != "prepare" && phase != "commit" {
		return nil, FederationLeaveOutput{}, errors.New("phase must be preflight, prepare, or commit")
	}
	disposition := strings.TrimSpace(input.Disposition)
	if disposition == "" {
		disposition = "detach"
	}
	if disposition != "detach" && disposition != "archive" {
		return nil, FederationLeaveOutput{}, errors.New("disposition must be detach or archive")
	}
	if disposition == "archive" {
		// Archiving through leave is project catalog administration; a
		// scoped server may only detach.
		if err := h.requireAllProjectsScope("federation leave with the archive disposition"); err != nil {
			return nil, FederationLeaveOutput{}, err
		}
	}
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, true)
	if err != nil {
		return nil, FederationLeaveOutput{}, err
	}
	if phase == "commit" {
		expected := "COMMIT FEDERATION LEAVE " + project.Name
		if input.Confirm != expected {
			return nil, FederationLeaveOutput{}, fmt.Errorf("confirm must equal %q", expected)
		}
	}
	preflight, prepare := phase == "preflight", phase == "prepare"
	response, err := h.options.Client.LeaveFederationReplica(ctx, &generated.LeaveFederationReplicaRequestOptions{PathParams: &generated.LeaveFederationReplicaPath{ProjectID: project.ID}, Body: &generated.LeaveFederationReplicaBody{Actor: &h.options.Actor, Disposition: &disposition, Force: optionalTrue(input.Force), Preflight: optionalTrue(preflight), Prepare: optionalTrue(prepare)}})
	if err != nil {
		return nil, FederationLeaveOutput{}, err
	}
	archived := response.Archived != nil && *response.Archived
	return successResult(), FederationLeaveOutput{Project: projectSummaryOut(response.Project), Phase: phase, Disposition: response.Disposition, Detached: response.Detached, Archived: archived, PendingEnrollment: response.PendingEnrollment}, nil
}

func (h toolHandlers) federationQuarantine(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationQuarantineInput) (*sdkmcp.CallToolResult, FederationQuarantineOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, FederationQuarantineOutput{}, err
	}
	if input.ID <= 0 {
		return nil, FederationQuarantineOutput{}, errors.New("id must be positive")
	}
	expected := strings.ToUpper(input.Action) + fmt.Sprintf(" FEDERATION BATCH %d", input.ID)
	if input.Action != "retry" && input.Action != "skip" {
		return nil, FederationQuarantineOutput{}, errors.New("action must be retry or skip")
	}
	if input.Confirm != expected {
		return nil, FederationQuarantineOutput{}, fmt.Errorf("confirm must equal %q", expected)
	}
	var response *generated.FederationQuarantineSummary
	if input.Action == "retry" {
		response, err = h.options.Client.RetryFederationQuarantine(ctx, &generated.RetryFederationQuarantineRequestOptions{PathParams: &generated.RetryFederationQuarantinePath{ProjectID: project.ID, QuarantineID: input.ID}, Body: &generated.RetryFederationQuarantineBody{Actor: h.options.Actor, Reason: optionalString(input.Reason)}, Header: &generated.RetryFederationQuarantineHeaders{XKataConfirm: &expected}})
	} else {
		response, err = h.options.Client.SkipFederationQuarantine(ctx, &generated.SkipFederationQuarantineRequestOptions{PathParams: &generated.SkipFederationQuarantinePath{ProjectID: project.ID, QuarantineID: input.ID}, Body: &generated.SkipFederationQuarantineBody{Actor: h.options.Actor, Reason: optionalString(input.Reason)}, Header: &generated.SkipFederationQuarantineHeaders{XKataConfirm: &expected}})
	}
	if err != nil {
		return nil, FederationQuarantineOutput{}, err
	}
	return successResult(), FederationQuarantineOutput{Project: project, Quarantine: h.redactQuarantineError(*response)}, nil
}

func federationEnrollmentSummary(enrollment generated.FederationEnrollmentOut) FederationEnrollmentSummary {
	return FederationEnrollmentSummary{ID: enrollment.ID, ProjectID: enrollment.ProjectID, SpokeInstanceUID: enrollment.SpokeInstanceUID, Capabilities: enrollment.Capabilities, Actor: enrollment.Actor, Revoked: enrollment.RevokedAt != nil}
}
