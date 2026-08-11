package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// FederationEnrollInput enables a hub project or creates an enrollment.
type FederationEnrollInput struct {
	Action                       string `json:"action,omitempty"`
	Project                      string `json:"project,omitempty"`
	SpokeInstanceUID             string `json:"spoke_instance_uid,omitempty"`
	Capabilities                 string `json:"capabilities,omitempty"`
	Token                        string `json:"token,omitempty"`
	AllowAdoptionSnapshotAuthors bool   `json:"allow_adoption_snapshot_authors,omitempty"`
}

// FederationEnrollOutput reports federation metadata or a new enrollment.
type FederationEnrollOutput struct {
	Project    ProjectIdentity                  `json:"project"`
	Metadata   *generated.ProjectFederationBody `json:"metadata,omitempty"`
	Enrollment *FederationEnrollmentSummary     `json:"enrollment,omitempty"`
	Token      string                           `json:"token,omitempty"`
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

// FederationJoinInput carries one hub enrollment into a spoke join.
type FederationJoinInput struct {
	ProjectName            string `json:"project_name"`
	HubURL                 string `json:"hub_url"`
	HubProjectID           int64  `json:"hub_project_id"`
	HubProjectUID          string `json:"hub_project_uid"`
	ReplayHorizonEventID   int64  `json:"replay_horizon_event_id,omitempty"`
	BaselineThroughEventID int64  `json:"baseline_through_event_id,omitempty"`
	Token                  string `json:"token,omitempty"`
	Capabilities           string `json:"capabilities,omitempty"`
	AllowInsecure          bool   `json:"allow_insecure,omitempty"`
	PushEnabled            bool   `json:"push_enabled,omitempty"`
	AdoptExisting          bool   `json:"adopt_existing,omitempty"`
}

// FederationJoinOutput reports the created or adopted spoke binding.
type FederationJoinOutput struct {
	Project ProjectSummary                 `json:"project"`
	Binding generated.FederationBindingOut `json:"binding"`
	Adopted bool                           `json:"adopted,omitempty"`
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
	additive := toolHints(false, false, false)
	destructive := toolHints(false, true, false)
	addTool(server, "kata.lease", "Manage lease", "Acquire, renew, or release the startup actor's federation write lease.", additive, handlers.lease)
	addTool(server, "kata.lease_force_release", "Force-release lease", "Administratively release another lease with a reason.", destructive, handlers.leaseForceRelease)
	addTool(server, "kata.lease_status", "Lease status", "Read the current federation write lease for an issue.", read, handlers.leaseStatus)
	addTool(server, "kata.lease_steal", "Steal lease", "Resumably force-release and acquire a lease as the startup actor.", destructive, handlers.leaseSteal)
}

func registerFederationTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, true)
	mutating := toolHints(false, true, true)
	additive := toolHints(false, false, true)
	addTool(server, "kata.federation_status", "Federation status", "Read in-scope federation, enrollment, replica, and quarantine status without secrets.", read, handlers.federationStatus)
	addTool(server, "kata.federation_enroll", "Federation enrollment", "Enable federation for a project or create a spoke enrollment.", additive, handlers.federationEnroll)
	addTool(server, "kata.federation_enrollment_revoke", "Revoke enrollment", "Revoke a federation enrollment by ID.", mutating, handlers.federationEnrollmentRevoke)
	addTool(server, "kata.federation_join", "Join federation", "Join or adopt an enrolled hub project as a spoke.", additive, handlers.federationJoin)
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

func (h toolHandlers) leaseSteal(ctx context.Context, _ *sdkmcp.CallToolRequest, input LeaseStealInput) (*sdkmcp.CallToolResult, LeaseOutput, error) {
	statusResult, status, err := h.leaseStatus(ctx, nil, LeaseStatusInput{Ref: input.Ref})
	_ = statusResult
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	previous := status.Holder
	if status.Held && status.Holder != h.options.Actor {
		if _, _, err := h.leaseForceRelease(ctx, nil, LeaseForceReleaseInput{Ref: input.Ref, Reason: input.Reason}); err != nil {
			return nil, LeaseOutput{}, err
		}
	}
	_, acquired, err := h.lease(ctx, nil, LeaseInput{
		Ref: input.Ref, Action: "acquire", TTLSeconds: input.TTLSeconds, Purpose: input.Purpose,
	})
	if err != nil {
		return nil, LeaseOutput{}, err
	}
	acquired.PreviousHolder = previous
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
		Project: project, Ref: project.Name + "#" + ref, Held: response.Lease != nil,
		Granted: response.Granted, Holder: response.Holder.Holder, Event: eventSummary(response.Event),
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
	return successResult(), FederationStatusOutput{Statuses: statuses, Enrollments: enrollments}, nil
}

func (h toolHandlers) federationEnroll(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationEnrollInput) (*sdkmcp.CallToolResult, FederationEnrollOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, FederationEnrollOutput{}, err
	}
	action := strings.TrimSpace(input.Action)
	if action == "" {
		action = "enroll"
	}
	if action == "enable" {
		response, enableErr := h.options.Client.EnableProjectFederation(ctx, &generated.EnableProjectFederationRequestOptions{PathParams: &generated.EnableProjectFederationPath{ProjectID: project.ID}, Body: &generated.EnableProjectFederationBody{Actor: &h.options.Actor}})
		if enableErr != nil {
			return nil, FederationEnrollOutput{}, enableErr
		}
		return successResult(), FederationEnrollOutput{Project: project, Metadata: response}, nil
	}
	if action != "enroll" {
		return nil, FederationEnrollOutput{}, errors.New("action must be enable or enroll")
	}
	if strings.TrimSpace(input.SpokeInstanceUID) == "" || strings.TrimSpace(input.Capabilities) == "" {
		return nil, FederationEnrollOutput{}, errors.New("enroll requires spoke_instance_uid and capabilities")
	}
	response, err := h.options.Client.CreateFederationEnrollment(ctx, &generated.CreateFederationEnrollmentRequestOptions{Body: &generated.CreateFederationEnrollmentBody{
		Actor: &h.options.Actor, ProjectID: project.ID, SpokeInstanceUID: input.SpokeInstanceUID,
		Capabilities: input.Capabilities, Token: optionalString(input.Token), AllowAdoptionSnapshotAuthors: optionalTrue(input.AllowAdoptionSnapshotAuthors),
	}})
	if err != nil {
		return nil, FederationEnrollOutput{}, err
	}
	summary := federationEnrollmentSummary(*response)
	output := FederationEnrollOutput{Project: project, Enrollment: &summary}
	if response.Token != nil {
		output.Token = *response.Token
	}
	return successResult(), output, nil
}

func (h toolHandlers) federationEnrollmentRevoke(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationEnrollmentRevokeInput) (*sdkmcp.CallToolResult, FederationEnrollmentRevokeOutput, error) {
	if input.ID <= 0 {
		return nil, FederationEnrollmentRevokeOutput{}, errors.New("id must be positive")
	}
	// Resolve and scope the enrollment before revocation.
	_, status, err := h.federationStatus(ctx, nil, FederationStatusInput{})
	if err != nil {
		return nil, FederationEnrollmentRevokeOutput{}, err
	}
	allowed := false
	for _, enrollment := range status.Enrollments {
		if enrollment.ID == input.ID {
			allowed = true
			break
		}
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

func (h toolHandlers) federationJoin(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationJoinInput) (*sdkmcp.CallToolResult, FederationJoinOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" || strings.TrimSpace(input.HubURL) == "" || input.HubProjectID <= 0 || strings.TrimSpace(input.HubProjectUID) == "" {
		return nil, FederationJoinOutput{}, errors.New("project_name, hub_url, hub_project_id, and hub_project_uid are required")
	}
	if h.options.Scope.Mode() != ScopeAll {
		if _, err := h.options.Scope.Project(ctx, h.options.Client, projectName, false); err != nil {
			return nil, FederationJoinOutput{}, err
		}
	}
	body := generated.CreateFederationReplicaRequestBody{
		Actor: &h.options.Actor, ProjectName: projectName, HubURL: input.HubURL, HubProjectID: input.HubProjectID,
		HubProjectUID: input.HubProjectUID, ReplayHorizonEventID: input.ReplayHorizonEventID,
		BaselineThroughEventID: optionalInt64(input.BaselineThroughEventID), Token: optionalString(input.Token),
		Capabilities: optionalString(input.Capabilities), AllowInsecure: optionalTrue(input.AllowInsecure), PushEnabled: optionalTrue(input.PushEnabled), AdoptExisting: optionalTrue(input.AdoptExisting),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, FederationJoinOutput{}, err
	}
	response, err := h.options.Client.CreateFederationReplica(ctx, &generated.CreateFederationReplicaRequestOptions{}, func(_ context.Context, request *http.Request) error {
		request.Body = io.NopCloser(bytes.NewReader(payload))
		request.ContentLength = int64(len(payload))
		request.Header.Set("Content-Type", "application/json")
		return nil
	})
	if err != nil {
		return nil, FederationJoinOutput{}, err
	}
	adopted := response.Adopted != nil && *response.Adopted
	return successResult(), FederationJoinOutput{Project: projectSummaryOut(response.Project), Binding: response.Binding, Adopted: adopted}, nil
}

func (h toolHandlers) federationRebind(ctx context.Context, _ *sdkmcp.CallToolRequest, input FederationRebindInput) (*sdkmcp.CallToolResult, FederationRebindOutput, error) {
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
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, true)
	if err != nil {
		return nil, FederationLeaveOutput{}, err
	}
	disposition := strings.TrimSpace(input.Disposition)
	if disposition == "" {
		disposition = "detach"
	}
	if disposition != "detach" && disposition != "archive" {
		return nil, FederationLeaveOutput{}, errors.New("disposition must be detach or archive")
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
	return successResult(), FederationQuarantineOutput{Project: project, Quarantine: *response}, nil
}

func federationEnrollmentSummary(enrollment generated.FederationEnrollmentOut) FederationEnrollmentSummary {
	return FederationEnrollmentSummary{ID: enrollment.ID, ProjectID: enrollment.ProjectID, SpokeInstanceUID: enrollment.SpokeInstanceUID, Capabilities: enrollment.Capabilities, Actor: enrollment.Actor, Revoked: enrollment.RevokedAt != nil}
}

func optionalInt64(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}
