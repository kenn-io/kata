package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerActionsHandlers installs POST /actions/close and /actions/reopen.
// CloseIssue and ReopenIssue return changed=false with a nil event when the
// issue is already in the target state; both fields propagate verbatim into
// the MutationResponse envelope.
func registerActionsHandlers(humaAPI huma.API, cfg ServerConfig) {
	var closeDeliveries pendingCloseDeliveries
	huma.Register(humaAPI, huma.Operation{
		OperationID: "closeIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/close",
	}, func(ctx context.Context, in *api.CloseActionRequest) (*api.MutationResponse, error) {
		if (in.IdempotencyKey != "" || in.IfMatch != "") &&
			in.Body.RetryProtocol != api.CloseRetryProtocol {
			return nil, api.NewError(400, "retry_protocol_required",
				"retry_protocol close-v1 is required with close retry headers", "", nil)
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		// Owner-local TUI closes bypass substance / evidence validation:
		// the interactive human path is "press x to close" and a 40-char
		// rationale prompt would just annoy the user. Forwarded and
		// non-loopback TCP callers cannot assert this exception through
		// the request body.
		// Structural guards (parent-close, throttle, repeated-message)
		// still apply, so the audit trail still gates lazy parent-closes
		// and reviewers can spot the no-evidence rows.
		//
		// Reason defaulting is handled here, at the handler boundary,
		// so the db layer never silently coerces an empty reason. The
		// TUI client always sends "done"; the explicit fallback below
		// covers older clients and keeps the policy visible.
		if in.Body.Source == "tui" && in.Body.Reason == "" {
			in.Body.Reason = "done"
		}
		ifMatchRev, err := parseOptionalIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		// The TUI bypass is scoped to reason="done" — the only shape
		// the interactive "press x to close" path ever produces. A
		// caller sending source="tui" with reason="duplicate" or
		// reason="superseded" is either a misconfigured client or an
		// agent trying to route around the evidence-target check by
		// claiming a TUI origin; require full validation in that case
		// so duplicate/superseded closes still must carry their typed
		// targets and won't corrupt the audit trail.
		tuiBypass := tuiBypassAllowed(ctx, in.Body.Source, in.Body.Reason)
		idempotencyFingerprint := ""
		if in.IdempotencyKey != "" {
			release, err := cfg.DB.AcquireIdempotencyLock(ctx, in.ProjectID, in.IdempotencyKey)
			if err != nil {
				return nil, internalAPIError(err)
			}
			defer func() { _ = release() }()
			if pending, ok := closeDeliveries.get(in.ProjectID, in.IdempotencyKey); ok {
				retryFingerprint := closeIdempotencyFingerprint(
					pending.issueUID, in.Ref, actor, in.Body.Reason, in.Body.Message, in.Body.Source,
					in.Body.Evidence, in.Body.DryRun, ifMatchRev)
				if err := closeDeliveries.publishCommitted(
					ctx, cfg, in.ProjectID, in.IdempotencyKey, retryFingerprint,
				); err != nil {
					return nil, err
				}
			}

			match, err := lookupCloseIdempotencyMatch(ctx, cfg, in.ProjectID, in.IdempotencyKey)
			if err != nil {
				return nil, err
			}
			if match != nil {
				idempotencyFingerprint = closeIdempotencyFingerprint(
					match.IssueUID, in.Ref, actor, in.Body.Reason, in.Body.Message, in.Body.Source,
					in.Body.Evidence, in.Body.DryRun, ifMatchRev)
				return closeIdempotencyResponse(ctx, cfg, match, idempotencyFingerprint)
			}
		}
		includeDeleted := db.IncludeDeletedNo
		if in.IdempotencyKey != "" {
			includeDeleted = db.IncludeDeletedYes
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, includeDeleted)
		if err != nil {
			return nil, err
		}
		if in.IdempotencyKey != "" {
			idempotencyFingerprint = closeIdempotencyFingerprint(
				issue.UID, in.Ref, actor, in.Body.Reason, in.Body.Message, in.Body.Source,
				in.Body.Evidence, in.Body.DryRun, ifMatchRev)
		}
		if issue.DeletedAt != nil {
			return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
		if ifMatchRev != nil && issue.Revision != *ifMatchRev {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("issue revision is %d", issue.Revision), "", nil)
		}
		// Already-closed short-circuit. CloseIssue itself returns
		// changed=false for this case; short-circuiting before the
		// guards (and substance / evidence validation) keeps idempotent
		// retries from failing with 400 / 409 / 429 when the retry
		// happens to omit fields the validator requires, when a child
		// has landed since the original close, or when the throttle
		// window is hot. Validation only gates real state transitions.
		if issue.Status == "closed" {
			if in.IdempotencyKey != "" {
				return nil, api.NewError(409, "issue_already_closed",
					"issue was already closed by another request",
					"omit the idempotency key to accept the current state", nil)
			}
			out := &api.MutationResponse{}
			out.Body.Issue = issue
			return out, nil
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		if !tuiBypass {
			if err := ValidateCloseInput(in.Body.Reason, in.Body.Message, in.Body.Evidence); err != nil {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
			if err := validateEvidenceTargets(ctx, cfg.DB, in.ProjectID, issue.ShortID, in.Body.Evidence); err != nil {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
		}
		if err := CheckParentCloseCompleteness(ctx, cfg.DB, issue.ID, issue.ShortID, issue.ProjectID); err != nil {
			return nil, api.NewError(409, "parent_has_open_children", err.Error(), "", nil)
		}
		now := time.Now()
		dbEvidence := evidenceToDB(in.Body.Evidence)
		// Burst/prose throttles are opt-in via [close.throttle] enabled=true
		// for operators who want stricter pacing.
		if cfg.CloseThrottle.SiblingBurstEnabled {
			if parentRef, cohort, refusal := CheckSiblingCloseThrottle(
				ctx, cfg.DB, issue, actor, now, cfg.CloseThrottle.SiblingBurstWindow); refusal != nil {
				// Dry-run is side-effect-free: surface the 429 but skip persisting
				// an audit event so kata events --tail doesn't fill with would-be
				// refusals from validation probes.
				if !in.Body.DryRun {
					if err := emitThrottledEvent(ctx, cfg, issue, actor,
						db.CloseThrottledPayload{
							Reason: db.CloseThrottleReasonSiblingBurst,
							Parent: parentRef,
							Cohort: cohort,
						}); err != nil {
						return nil, internalAPIError(err)
					}
				}
				return nil, api.NewError(429, "sibling_throttle", refusal.Error(), "", nil)
			}
			if priorRef, parentRef, refusal := CheckRepeatedMessageGuard(
				ctx, cfg.DB, issue,
				actor, in.Body.Reason, in.Body.Message, now); refusal != nil {
				if !in.Body.DryRun {
					if err := emitThrottledEvent(ctx, cfg, issue, actor,
						db.CloseThrottledPayload{
							Reason: db.CloseThrottleReasonDuplicateMessage,
							Parent: parentRef,
							Prior:  &priorRef,
						}); err != nil {
						return nil, internalAPIError(err)
					}
				}
				return nil, api.NewError(429, "duplicate_message", refusal.Error(), "", nil)
			}
		}
		// Dry-run: report what would happen after all guards run, but
		// skip the DB mutation. Validation, evidence-target resolution,
		// parent completeness, sibling-throttle, and repeated-message
		// guards all run first so their refusals surface in dry-run
		// output too.
		if in.Body.DryRun {
			out := &api.MutationResponse{}
			out.Body.Issue = issue
			return out, nil
		}
		var updated db.Issue
		var evt *db.Event
		var events []db.Event
		var changed bool
		err = cfg.DB.RetryTransient(ctx, func() error {
			var err error
			evt = nil
			events = nil
			updated, events, changed, err = cfg.DB.CloseIssueGuarded(ctx, db.CloseIssueParams{
				IssueID: issue.ID, ExpectedProjectID: in.ProjectID,
				Reason: in.Body.Reason, Actor: actor,
				Message: in.Body.Message, Evidence: dbEvidence, IfMatchRev: ifMatchRev,
				IdempotencyKey: in.IdempotencyKey, IdempotencyFingerprint: idempotencyFingerprint,
			})
			if len(events) > 0 {
				evt = &events[0]
			}
			if err != nil && in.IdempotencyKey != "" && len(events) > 0 {
				closeDeliveries.remember(in.ProjectID, in.IdempotencyKey,
					issue.UID, idempotencyFingerprint, events)
			}
			return err
		})
		if err == nil && changed {
			closeDeliveries.discard(in.ProjectID, in.IdempotencyKey, idempotencyFingerprint)
			cfg.Publish().Events(in.ProjectID, events)
		}
		if in.IdempotencyKey != "" && (err != nil || !changed) {
			if recoveryErr := closeDeliveries.publishCommitted(
				ctx, cfg, in.ProjectID, in.IdempotencyKey, idempotencyFingerprint,
			); recoveryErr != nil {
				return nil, recoveryErr
			}
		}
		if err != nil {
			// A connection can fail after the database commits. The keyed
			// receipt proves whether this attempt landed. When it did, publish
			// the complete event batch returned by that attempt before replying.
			if in.IdempotencyKey != "" {
				reuse, lookupErr := tryCloseIdempotencyMatch(
					ctx, cfg, in.ProjectID, in.IdempotencyKey, idempotencyFingerprint)
				if lookupErr != nil {
					return nil, lookupErr
				}
				if reuse != nil {
					return reuse, nil
				}
			}
			if revisionConflict, ok := errors.AsType[*db.RevisionConflictError](err); ok {
				return nil, api.NewError(412, "revision_conflict",
					fmt.Sprintf("issue revision is %d", revisionConflict.CurrentRevision), "", nil)
			}
			// In-transaction guard re-fires when a concurrent link/create
			// added an open child between the read-side guard and the
			// close write. Map it to the same 409 code so clients see
			// one consistent error shape; recompute the listing for the
			// friendlier message.
			if errors.Is(err, db.ErrOpenChildren) {
				detail := err.Error()
				if listErr := CheckParentCloseCompleteness(ctx, cfg.DB,
					issue.ID, issue.ShortID, issue.ProjectID); listErr != nil {
					detail = listErr.Error()
				}
				return nil, api.NewError(409, "parent_has_open_children", detail, "", nil)
			}
			if errors.Is(err, db.ErrIssueProjectChanged) {
				return nil, api.NewError(409, "issue_moved",
					"issue moved to another project before the close committed",
					"resolve the issue in its current project and retry with a fresh idempotency key", nil)
			}
			if errors.Is(err, db.ErrFederatedReadOnly) {
				return nil, federationReadOnlyError(err)
			}
			return nil, internalAPIError(err)
		}
		// A database retry can observe that its first attempt committed even
		// when the commit response was lost. Recover that attempt's receipt
		// before returning a plain no-op response.
		if !changed && in.IdempotencyKey != "" {
			reuse, err := tryCloseIdempotencyMatch(
				ctx, cfg, in.ProjectID, in.IdempotencyKey, idempotencyFingerprint)
			if err != nil {
				return nil, err
			}
			if reuse != nil {
				return reuse, nil
			}
			return nil, api.NewError(409, "issue_already_closed",
				"issue was closed by another request before this close committed",
				"retry after reopening, or omit the idempotency key to accept the current state", nil)
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "reopenIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/reopen",
	}, func(ctx context.Context, in *api.ActionRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if issue.Status == "open" {
			out := &api.MutationResponse{}
			out.Body.Issue = issue
			return out, nil
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		var updated db.Issue
		var evt *db.Event
		var changed bool
		err = cfg.DB.RetryTransient(ctx, func() error {
			var err error
			updated, evt, changed, err = cfg.DB.ReopenIssue(ctx, issue.ID, actor)
			return err
		})
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if changed && evt != nil {
			cfg.Publish().Event(in.ProjectID, *evt)
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})
}

type pendingCloseDeliveryKey struct {
	projectID      int64
	idempotencyKey string
}

type pendingCloseDelivery struct {
	issueUID    string
	fingerprint string
	events      []db.Event
}

// pendingCloseDeliveries retains the event identities returned by a close
// whose commit response was ambiguous. A later exact retry can verify those
// events in storage and deliver them before it returns the stored receipt.
type pendingCloseDeliveries struct {
	mu      sync.Mutex
	pending map[pendingCloseDeliveryKey]pendingCloseDelivery
}

func (p *pendingCloseDeliveries) remember(
	projectID int64, key, issueUID, fingerprint string, events []db.Event,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[pendingCloseDeliveryKey]pendingCloseDelivery)
	}
	p.pending[pendingCloseDeliveryKey{projectID: projectID, idempotencyKey: key}] = pendingCloseDelivery{
		issueUID: issueUID, fingerprint: fingerprint, events: append([]db.Event(nil), events...),
	}
}

func (p *pendingCloseDeliveries) get(
	projectID int64, key string,
) (pendingCloseDelivery, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delivery, ok := p.pending[pendingCloseDeliveryKey{projectID: projectID, idempotencyKey: key}]
	return delivery, ok
}

func (p *pendingCloseDeliveries) discard(projectID int64, key, fingerprint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mapKey := pendingCloseDeliveryKey{projectID: projectID, idempotencyKey: key}
	if delivery, ok := p.pending[mapKey]; ok && delivery.fingerprint == fingerprint {
		delete(p.pending, mapKey)
	}
}

func (p *pendingCloseDeliveries) publishCommitted(
	ctx context.Context, cfg ServerConfig, projectID int64, key, fingerprint string,
) error {
	delivery, ok := p.get(projectID, key)
	if !ok || delivery.fingerprint != fingerprint {
		return nil
	}
	uids := make([]string, len(delivery.events))
	for i := range delivery.events {
		uids[i] = delivery.events[i].UID
	}
	stored, err := cfg.DB.EventsByUIDs(ctx, projectID, uids)
	if errors.Is(err, db.ErrNotFound) {
		p.discard(projectID, key, fingerprint)
		return nil
	}
	if err != nil {
		return internalAPIError(err)
	}
	if !sameCloseEventBatch(delivery.events, stored) {
		return internalAPIError(errors.New("stored close event batch does not match the committed attempt"))
	}
	cfg.Publish().Events(projectID, stored)
	p.discard(projectID, key, fingerprint)
	return nil
}

func sameCloseEventBatch(expected, stored []db.Event) bool {
	if len(expected) != len(stored) {
		return false
	}
	for i := range expected {
		if expected[i].UID != stored[i].UID ||
			expected[i].ContentHash != stored[i].ContentHash {
			return false
		}
	}
	return true
}

func tryCloseIdempotencyMatch(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	key, fingerprint string,
) (*api.MutationResponse, error) {
	match, err := lookupCloseIdempotencyMatch(ctx, cfg, projectID, key)
	if err != nil {
		return nil, err
	}
	if match == nil {
		return nil, nil
	}
	return closeIdempotencyResponse(ctx, cfg, match, fingerprint)
}

func lookupCloseIdempotencyMatch(
	ctx context.Context, cfg ServerConfig, projectID int64, key string,
) (*db.IdempotencyMatch, error) {
	match, err := cfg.DB.LookupIssueMutationIdempotency(
		ctx, projectID, "issue.closed", key, time.Now().Add(-idempotencyWindow))
	if err != nil {
		return nil, internalAPIError(err)
	}
	return match, nil
}

func closeIdempotencyResponse(
	ctx context.Context, cfg ServerConfig, match *db.IdempotencyMatch, fingerprint string,
) (*api.MutationResponse, error) {
	if match.Fingerprint != fingerprint {
		return nil, api.NewError(409, "idempotency_mismatch",
			"idempotency key matched a prior close with a different fingerprint",
			"use a fresh key or send the exact original close request", nil)
	}
	current, err := cfg.DB.IssueByID(ctx, match.IssueID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	original := match.Event
	out := &api.MutationResponse{}
	out.Body.Issue = current
	out.Body.OriginalEvent = &original
	out.Body.Reused = true
	return out, nil
}

func closeIdempotencyFingerprint(
	issueUID, requestRef, actor, reason, message, source string,
	evidence []api.Evidence,
	dryRun bool,
	ifMatchRev *int64,
) string {
	encoded, _ := json.Marshal(struct {
		IssueUID   string         `json:"issue_uid"`
		RequestRef string         `json:"request_ref"`
		Actor      string         `json:"actor"`
		Reason     string         `json:"reason"`
		Message    string         `json:"message"`
		Source     string         `json:"source"`
		Evidence   []api.Evidence `json:"evidence"`
		DryRun     bool           `json:"dry_run"`
		IfMatchRev *int64         `json:"if_match_revision"`
	}{
		IssueUID: issueUID, RequestRef: requestRef,
		Actor: actor, Reason: reason, Message: message,
		Source: source, Evidence: evidence, DryRun: dryRun, IfMatchRev: ifMatchRev,
	})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// validateEvidenceTargets resolves duplicate-of and superseded-by issue
// refs in the same project and rejects targets that are missing or that
// point at the issue being closed. ValidateCloseInput already checks that
// the issue ref is non-empty; this is the database-backed half of the
// check that the pure-function validator (used by unit tests) intentionally
// omits.
//
// Errors are plain so the caller can wrap them in the 400 validation
// envelope alongside the other shape-check errors.
func validateEvidenceTargets(
	ctx context.Context, store db.Storage,
	projectID int64, closingShortID string, evidence []api.Evidence,
) error {
	for i, e := range evidence {
		switch e.Type {
		case api.EvidenceDuplicateOf, api.EvidenceSupersededBy:
		default:
			continue
		}
		target, err := resolveIssueRef(ctx, store, projectID, e.IssueRef, db.IncludeDeletedNo)
		if err != nil {
			return fmt.Errorf("evidence[%d] %s target %q does not exist in this project",
				i, e.Type, e.IssueRef)
		}
		if target.ShortID == closingShortID {
			return fmt.Errorf("evidence[%d] %s cannot reference the issue being closed (%s)",
				i, e.Type, closingShortID)
		}
	}
	return nil
}

// evidenceToDB performs the 1:1 conversion from the api wire type to the
// db storage type, mirroring the pattern used for LinkChanges /
// AtomicEditChanges. The db package can't import api directly because
// internal/api already imports internal/db; both types remain
// field-for-field identical and the daemon handles the boundary.
func evidenceToDB(in []api.Evidence) []db.Evidence {
	if len(in) == 0 {
		return nil
	}
	out := make([]db.Evidence, len(in))
	for i, e := range in {
		out[i] = db.Evidence{
			Type:      string(e.Type),
			SHA:       e.SHA,
			URL:       e.URL,
			Command:   e.Command,
			Paths:     e.Paths,
			Account:   e.Account,
			Rationale: e.Rationale,
			IssueRef:  e.IssueRef,
		}
	}
	return out
}
