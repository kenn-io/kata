package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerOwnershipHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "assignIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/assign",
	}, func(ctx context.Context, in *api.AssignRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		owner := strings.TrimSpace(in.Body.Owner)
		if owner == "" {
			return nil, api.NewError(400, "validation", "owner must be non-empty", "", nil)
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, &owner, actor)
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if changed && evt != nil {
			cfg.Publish().Event(in.ProjectID, *evt)
		}
		evt, err = scopedMutationEvent(ctx, cfg.DB, evt)
		if err != nil {
			return nil, err
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "unassignIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/unassign",
	}, func(ctx context.Context, in *api.UnassignRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		var expectedOwner *string
		if in.Body.ExpectedOwner != nil {
			expected := strings.TrimSpace(*in.Body.ExpectedOwner)
			if expected == "" {
				return nil, api.NewError(400, "validation",
					"expected_owner must be non-empty when provided", "omit expected_owner for an unconditional unassign", nil)
			}
			expectedOwner = &expected
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UnassignOwner(ctx, issue.ID, actor, expectedOwner)
		if err != nil {
			if errors.Is(err, db.ErrOwnerMismatch) {
				var currentOwner any
				if updated.Owner != nil {
					currentOwner = *updated.Owner
				}
				return nil, api.NewError(409, "owner_mismatch",
					"issue owner does not match expected owner",
					"refresh issue state before retrying",
					map[string]any{"expected_owner": *expectedOwner, "current_owner": currentOwner})
			}
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if changed && evt != nil {
			cfg.Publish().Event(in.ProjectID, *evt)
		}
		evt, err = scopedMutationEvent(ctx, cfg.DB, evt)
		if err != nil {
			return nil, err
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "claimIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/claim",
	}, func(ctx context.Context, in *api.ClaimRequest) (*api.ClaimResponse, error) {
		ctx, principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization,
			api.ClaimActionBody{Holder: in.Body.Actor}, federationTransportOperation("claimIssue"), true)
		if err != nil {
			return nil, err
		}
		actor := strings.TrimSpace(principal.Holder)
		if principal.AuthenticatedHost {
			actor, err = attributedActor(ctx, in.Body.Actor)
			if err != nil {
				return nil, err
			}
		}
		if err := validateActor(actor); err != nil {
			return nil, err
		}
		if in.Body.Force && in.Body.IfUnowned {
			return nil, api.NewError(400, "validation",
				"force and if_unowned are mutually exclusive", "choose one claim mode", nil)
		}
		if principal.Enrollment && in.Body.TTLSeconds == nil {
			return nil, api.NewError(400, "validation",
				"federation claim credentials require ttl_seconds",
				"pass ttl_seconds to bound the assignment; enrollment claims cannot be permanent", nil)
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if in.Body.TTLSeconds != nil {
			forwarded, handled, forwardErr := forwardTimedAssignmentClaim(ctx, cfg, in, issue, actor)
			if handled {
				return forwarded, forwardErr
			}
		}
		if err := requireFederatedIssueClaimForPrincipal(ctx, cfg, in.ProjectID, issue, principal.ClaimPrincipal); err != nil {
			return nil, err
		}

		var ttl time.Duration
		if in.Body.TTLSeconds != nil {
			ttl = time.Duration(*in.Body.TTLSeconds) * time.Second
		}
		var result db.ClaimResult
		err = cfg.DB.RetryTransient(ctx, func() error {
			var err error
			result, err = cfg.DB.ClaimOwner(ctx, db.ClaimOwnerParams{
				IssueID: issue.ID, Actor: actor, Force: in.Body.Force, IfUnowned: in.Body.IfUnowned,
				TTL: ttl, Now: time.Now().UTC(),
			})
			return err
		})
		if errors.Is(err, db.ErrAlreadyAssigned) {
			currentOwner := "unknown"
			if result.CurrentOwner != nil {
				currentOwner = *result.CurrentOwner
			}
			hint := "use --force to reassign"
			if in.Body.IfUnowned {
				hint = "choose another issue, or omit if_unowned only for a deliberate retry"
			}
			details := map[string]any{"current_owner": currentOwner}
			if result.Issue.AssignmentExpiresOn != nil {
				details["assignment_expires_on"] = result.Issue.AssignmentExpiresOn
			}
			return nil, api.NewError(409, "already_claimed",
				fmt.Sprintf("issue is already assigned to %s", currentOwner),
				hint,
				details)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}

		if result.Changed {
			cfg.Publish().Events(in.ProjectID, result.Events)
		}
		var replayEvents []db.Event
		if principal.Enrollment && result.Event != nil && result.Event.Type == "issue.assignment_renewed" {
			replayEvents, err = assignmentReplayEvents(ctx, cfg.DB, result.Issue, *result.Event)
			if err != nil {
				return nil, internalAPIError(err)
			}
		}
		claimEvents, _, err := scopedMutationEvents(ctx, cfg.DB, result.Events)
		if err != nil {
			return nil, err
		}
		var claimEvent *db.Event
		if len(claimEvents) > 0 {
			claimEvent = &claimEvents[len(claimEvents)-1]
		}

		out := &api.ClaimResponse{}
		out.Body.Issue = result.Issue
		out.Body.Event = claimEvent
		out.Body.Events = nonNilEvents(claimEvents)
		out.Body.ReplayEvents = replayEvents
		out.Body.Changed = result.Changed
		out.Body.PreviousOwner = result.PreviousOwner
		if principal.Enrollment {
			shapeEnrollmentClaimResponse(&out.Body)
		}
		return out, nil
	})
}

// enrollmentAssignmentEventTypes lists the event types whose payloads carry
// only assignment state (owner, expiry, timestamps). Every other event type
// such as issue.created, issue.snapshot, or issue.updated carries project
// content that claim-only federation enrollments have no read authority over.
var enrollmentAssignmentEventTypes = map[string]bool{
	"issue.assigned":           true,
	"issue.unassigned":         true,
	"issue.assignment_renewed": true,
	"issue.assignment_expired": true,
}

// shapeEnrollmentClaimResponse projects a claim response down to what a
// claim-only federation enrollment may see. Enrollment principals hold
// write-shaped authority (bounded timed assignment) without read authority
// over project content, so the response keeps assignment state and stable
// identifiers only: the issue is reduced to its identity and assignment
// fields, and event lists are restricted to assignment lifecycle events. The
// spoke consumes the assignment events, then reloads the issue from its own
// replicated database; direct enrollment callers get the same assignment-only
// view. Local, identity, and full-authority callers keep the unshaped
// response.
func shapeEnrollmentClaimResponse(body *api.ClaimResponseBody) {
	body.Issue = db.Issue{
		UID:                 body.Issue.UID,
		ProjectID:           body.Issue.ProjectID,
		ProjectUID:          body.Issue.ProjectUID,
		ShortID:             body.Issue.ShortID,
		Owner:               body.Issue.Owner,
		AssignmentExpiresOn: body.Issue.AssignmentExpiresOn,
		UpdatedAt:           body.Issue.UpdatedAt,
	}
	body.Events = assignmentEventsForEnrollment(body.Events)
	body.ReplayEvents = assignmentEventsForEnrollment(body.ReplayEvents)
	if body.Event != nil && !enrollmentAssignmentEventTypes[body.Event.Type] {
		body.Event = nil
	}
}

// assignmentEventsForEnrollment drops every non-assignment event. It always
// returns a non-nil slice so the claim response's required events array
// survives filtering.
func assignmentEventsForEnrollment(events []db.Event) []db.Event {
	filtered := make([]db.Event, 0, len(events))
	for _, event := range events {
		if enrollmentAssignmentEventTypes[event.Type] {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

var assignmentReplayEventTypes = []string{
	"issue.assigned", "issue.unassigned",
	"issue.assignment_renewed", "issue.assignment_expired",
}

func assignmentReplayEvents(
	ctx context.Context,
	store db.Storage,
	issue db.Issue,
	final db.Event,
) ([]db.Event, error) {
	const pageSize = 1000
	events := make([]db.Event, 0)
	for afterID := int64(0); ; {
		page, err := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: afterID, ProjectID: issue.ProjectID, ThroughID: final.ID,
			IssueUID: issue.UID, Types: assignmentReplayEventTypes, Limit: pageSize,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, page...)
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}
	sort.SliceStable(events, func(i, j int) bool {
		left, right := events[i], events[j]
		if left.HLCPhysicalMS != right.HLCPhysicalMS {
			return left.HLCPhysicalMS < right.HLCPhysicalMS
		}
		if left.HLCCounter != right.HLCCounter {
			return left.HLCCounter < right.HLCCounter
		}
		if left.OriginInstanceUID != right.OriginInstanceUID {
			return left.OriginInstanceUID < right.OriginInstanceUID
		}
		return left.UID < right.UID
	})

	finalIndex := -1
	anchorIndex := -1
	for i, event := range events {
		if event.UID == final.UID {
			finalIndex = i
			break
		}
		// Only an event the enrollment may receive can anchor its replay.
		// A newer content snapshot must not cut off the assignment history.
		if event.Type == "issue.assigned" || event.Type == "issue.unassigned" {
			anchorIndex = i
		}
	}
	if finalIndex < 0 {
		return nil, fmt.Errorf("assignment renewal event %s not found in replay window", final.UID)
	}
	if anchorIndex < 0 {
		anchorIndex = 0
	}
	return append([]db.Event(nil), events[anchorIndex:finalIndex]...), nil
}

func forwardTimedAssignmentClaim(
	ctx context.Context,
	cfg ServerConfig,
	in *api.ClaimRequest,
	issue db.Issue,
	actor string,
) (*api.ClaimResponse, bool, error) {
	finishTransport, err := beginClaimFederationTransport(ctx, cfg, in.ProjectID)
	if err != nil {
		return nil, true, err
	}
	defer finishTransport()

	binding, err := cfg.DB.FederationBindingByProject(ctx, in.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	if !binding.Enabled || binding.Role != db.FederationRoleSpoke {
		return nil, false, nil
	}
	if !binding.PushEnabled {
		// A pull-only spoke cannot write to the hub. Fall through to the
		// ordinary claim gate, which returns the standard federated_read_only
		// error instead of forwarding a local-only write.
		return nil, false, nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
	if err != nil {
		return nil, true, err
	}
	body := in.Body
	body.Actor = actor
	forwarded, err := remote.ClaimIssue(ctx, cred.HubProjectID, issue.UID, body)
	if err != nil {
		return nil, true, assignmentClaimForwardError(err)
	}
	response, err := applyForwardedAssignmentClaim(ctx, cfg, in.ProjectID, issue.UID, forwarded)
	if err != nil {
		return nil, true, claimAPIError(err)
	}
	return response, true, nil
}

func assignmentClaimForwardError(err error) error {
	if statusErr, ok := errors.AsType[*claimHubStatusError](err); ok {
		var envelope api.ErrorEnvelope
		if json.Unmarshal([]byte(statusErr.Body), &envelope) == nil && envelope.Error.Code != "" {
			return api.NewError(statusErr.StatusCode, envelope.Error.Code, envelope.Error.Message,
				envelope.Error.Hint, map[string]any(envelope.Error.Data))
		}
	}
	return claimForwardError(err)
}
