package mcpevents

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/db"
)

// Retry and aging values (design spec §9).
const (
	// maxAttempts is the delivery budget for one item: the first send plus
	// four retries.
	maxAttempts = 5
	// deadEndpointAge and deadEndpointAttempts end a subscription whose
	// endpoint has failed continuously: both must hold.
	deadEndpointAge      = 7 * 24 * time.Hour
	deadEndpointAttempts = 20
	jitterFraction       = 0.1
)

// retryDelays[n-1] is the wait after the nth failed attempt.
var retryDelays = [maxAttempts - 1]time.Duration{30 * time.Second, time.Minute, 3 * time.Minute, 8 * time.Minute}

// stepResult tells the worker what a step achieved.
type stepResult int

const (
	// stepAgain means state moved and the row should be read and stepped again.
	stepAgain stepResult = iota
	// stepIdle means nothing is pending and nothing is left to scan.
	stepIdle
	// stepBusy means work remains but must wait: a retry is scheduled, or the
	// per-claim step budget ran out.
	stepBusy
	// stepError means the step could not run (authority or store outage); the
	// row is retried after a short backoff.
	stepError
	// stepLost means the claim or the row's generation changed under the
	// worker, or the worker is shutting down. Nothing more is written.
	stepLost
)

// step performs one unit of delivery work for a claimed row.
func (s *Service) step(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	now := s.now().UTC()
	if row.PendingKind != db.MCPEventPendingNone {
		return s.attempt(ctx, c, row)
	}
	if row.State != db.MCPEventStateActive {
		return stepIdle
	}
	if row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
		return s.end(ctx, c, row, "expired", false)
	}
	if row.Dirty {
		return s.stage(ctx, c, row)
	}
	return stepIdle
}

func principalOf(row db.MCPEventSubscription) Principal {
	return Principal{Authority: row.PrincipalAuthority, Subject: row.PrincipalSubject, Actor: row.Actor}
}

// liveGrant reacquires the owner and resolves its selection, as every ping
// staging and attempt does. Revocation or an empty explicit selection ends
// the subscription with a terminated notice.
func (s *Service) liveGrant(ctx context.Context, row db.MCPEventSubscription) error {
	var args Arguments
	if err := json.Unmarshal(row.Arguments, &args); err != nil {
		return errArgumentsUnreadable
	}
	auth, err := s.authority.Reacquire(ctx, principalOf(row))
	if err != nil {
		return err
	}
	_, err = ResolveProjects(ctx, auth, args)
	return err
}

var errArgumentsUnreadable = errors.New("stored subscription arguments cannot be read")

// stage turns a dirty flag into one pending ping, coalescing every change
// marked since the last one, and makes its first attempt. The body is fixed
// here and resent byte for byte on every retry.
func (s *Service) stage(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	if err := s.liveGrant(ctx, row); err != nil {
		return s.authorityFailure(ctx, c, row, err)
	}
	var args Arguments
	_ = json.Unmarshal(row.Arguments, &args)
	random, err := randomHex(16)
	if err != nil {
		return stepError
	}
	eventID := "evt_" + random
	body, err := json.Marshal(Envelope{
		EventID: eventID, Name: EventName, Timestamp: s.now().UTC().Format(time.RFC3339Nano),
		Data: Payload{Recipient: args.For},
	})
	if err != nil {
		return stepError
	}
	zero := 0
	pending := db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: eventID, Body: body}
	ok, err := s.write(ctx, c, db.MCPEventDeliveryUpdate{
		Pending: &pending, ClearDirty: true, AttemptCount: &zero, ClearNextAttempt: true,
	})
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	row.Dirty, row.PendingKind, row.PendingMessageID, row.PendingBody, row.AttemptCount = false, pending.Kind, pending.MessageID, pending.Body, 0
	return s.attempt(ctx, c, row)
}

// writeFailed maps a failed conditional write to a step result.
func (s *Service) writeFailed(ctx context.Context, err error) stepResult {
	if err != nil && ctx.Err() == nil {
		return stepError
	}
	return stepLost
}

// authorityFailure turns an authority or source error into the matching
// transition. Revocation and a vanished scope end the subscription with a
// terminated notice; anything else is an outage that changes nothing.
func (s *Service) authorityFailure(ctx context.Context, c *claim, row db.MCPEventSubscription, err error) stepResult {
	switch {
	case ctx.Err() != nil:
		return stepLost
	case errors.Is(err, ErrPrincipalRevoked):
		return s.end(ctx, c, row, "principal_revoked", true)
	case errors.Is(err, ErrScopeRemoved):
		return s.end(ctx, c, row, "scope_removed", true)
	case errors.Is(err, errArgumentsUnreadable):
		return s.endPermanent(ctx, c, row, "arguments_unreadable")
	default:
		return stepError
	}
}

// endPermanent ends a row whose stored state can never be delivered from. It
// reuses the endpoint_dead reason, which the existing column already carries,
// sends no notice, and logs the category only.
func (s *Service) endPermanent(ctx context.Context, c *claim, row db.MCPEventSubscription, category string) stepResult {
	slog.Warn("MCP Events subscription ended", "category", category)
	return s.end(ctx, c, row, "endpoint_dead", false)
}

// end moves the row to ended. With notify it queues a terminated control that
// the worker then delivers; without it the claim is released.
func (s *Service) end(ctx context.Context, c *claim, row db.MCPEventSubscription, reason string, notify bool) stepResult {
	now := s.now().UTC()
	zero := 0
	state := db.MCPEventStateEnded
	in := db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID,
		State:            &state, EndReason: &reason, EndedAt: &now, BumpGeneration: true,
		AttemptCount: &zero, ClearNextAttempt: true,
	}
	if notify {
		body, id, err := terminatedBody(reason)
		if err != nil {
			return stepError
		}
		in.Pending = &db.MCPEventPending{Kind: db.MCPEventPendingTerminated, MessageID: id, Body: body}
	} else {
		in.ClearPending, in.ReleaseClaim = true, true
	}
	in.RequireExpired = reason == "expired"
	if reason == "principal_revoked" || reason == "scope_removed" {
		// The decision came from the authority of the actor stored on row.
		in.ExpectedActor = &row.Actor
	}
	ok, err := s.write(ctx, c, in)
	if err != nil || !ok {
		if err == nil && (in.RequireExpired || in.ExpectedActor != nil) && ctx.Err() == nil {
			// Refresh preserves the claim and generation but can extend the
			// lifetime or rename the actor. Reload and decide again; the loop
			// still checks fencing.
			return stepAgain
		}
		return s.writeFailed(ctx, err)
	}
	if notify {
		return stepAgain
	}
	return stepIdle
}

// attempt makes one delivery attempt of the row's pending item.
func (s *Service) attempt(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	now := s.now().UTC()
	isEvent := row.PendingKind == db.MCPEventPendingEvent
	if row.AttemptCount >= maxAttempts {
		// A previous claimant died during the last attempt.
		return s.exhaust(ctx, c, row)
	}
	if isEvent {
		// A ping requires a live grant on every attempt, including retries
		// that run before the expiry sweep. Termination notices may still be
		// delivered after the grant has ended.
		if row.State != db.MCPEventStateActive {
			return s.settle(ctx, c, row, false, "")
		}
		if row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
			return s.end(ctx, c, row, "expired", false)
		}
		if err := s.liveGrant(ctx, row); err != nil {
			return s.authorityFailure(ctx, c, row, err)
		}
	}

	keys, ok := s.signingKeys(row, now)
	if !ok {
		// A secret that cannot be decrypted (wrong or damaged key material)
		// will not become readable by retrying, and no notice can be signed.
		if row.State != db.MCPEventStateActive {
			// A control for an already ended row: abandon it, keep the reason.
			return s.settle(ctx, c, row, false, "")
		}
		return s.endPermanent(ctx, c, row, "secret_unreadable")
	}
	// Persist attempt_count + 1 before sending, and recheck in the same
	// conditional update that the row is still deliverable.
	won, err := s.write(ctx, c, db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID, RequireDeliverable: true, IncrementAttempt: true,
	})
	if err != nil || !won {
		return s.writeFailed(ctx, err)
	}
	number := row.AttemptCount + 1

	receipt, sendErr := s.webhook.Send(ctx, row.CallbackURL, row.PendingMessageID, row.ID, row.PendingBody, keys)
	if ctx.Err() != nil {
		// Shutdown, or the claim was lost: the next claimant owns the item.
		return stepLost
	}
	switch {
	case sendErr == nil && receipt.StatusCode >= 200 && receipt.StatusCode < 300:
		return s.acknowledge(ctx, c, row)
	// SEP-3415: a 410 or 413 is a non-retryable failure of this one delivery
	// that leaves the subscription itself alone. It neither records nor
	// clears failure aging (§9.2).
	case sendErr == nil && (receipt.StatusCode == http.StatusGone || receipt.StatusCode == http.StatusRequestEntityTooLarge):
		return s.settle(ctx, c, row, isEvent, "http_4xx")
	}
	var retryAfter time.Duration
	if sendErr == nil && (receipt.StatusCode == http.StatusTooManyRequests || receipt.StatusCode == http.StatusServiceUnavailable) {
		retryAfter = receipt.RetryAfter
	}
	return s.failed(ctx, c, row, number, failureCategory(receipt.StatusCode, sendErr), retryAfter)
}

// signingKeys returns the current secret and, inside a rotation window, the
// previous one.
func (s *Service) signingKeys(row db.MCPEventSubscription, now time.Time) ([]string, bool) {
	current, err := Open(s.key, row.ID, secretRoleCurrent, row.SecretEnc)
	if err != nil {
		return nil, false
	}
	keys := []string{string(current)}
	if row.PreviousSecretUntil != nil && now.Before(*row.PreviousSecretUntil) && len(row.PreviousSecretEnc) > 0 {
		if previous, err := Open(s.key, row.ID, secretRolePrevious, row.PreviousSecretEnc); err == nil {
			keys = append(keys, string(previous))
		}
	}
	return keys, true
}

// failureCategory maps a failed attempt to a §7.6 category. It never exposes
// the underlying error text.
func failureCategory(status int, err error) string {
	if err != nil {
		switch safeWebhookError(err).Reason {
		case "destination_not_allowed":
			return "destination_not_allowed"
		case "timeout":
			return "timeout"
		case "tls_error":
			return "tls_error"
		default:
			return "connection_refused"
		}
	}
	if status >= 500 {
		return "http_5xx"
	}
	return "http_4xx"
}

// acknowledge settles a 2xx: the item is done and failure aging clears.
func (s *Service) acknowledge(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	now := s.now().UTC()
	zero, none := 0, ""
	ok, err := s.write(ctx, c, db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID, ClearPending: true, AttemptCount: &zero, ClearNextAttempt: true,
		ClearFailure: true, LastDeliveryAt: &now, LastError: &none,
	})
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	return stepAgain
}

// settle drops the pending item without a successful delivery: a 410 or 413,
// or an item for a row that is no longer active. deadLetter counts it, and
// next_attempt_at clears so a newer change is not held back.
func (s *Service) settle(ctx context.Context, c *claim, row db.MCPEventSubscription, deadLetter bool, category string) stepResult {
	zero := 0
	in := db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID, ClearPending: true, AttemptCount: &zero, ClearNextAttempt: true,
		IncrementDeadLetter: deadLetter,
	}
	if category != "" {
		in.LastError = &category
	}
	ok, err := s.write(ctx, c, in)
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	return stepAgain
}

// exhaust ends an item's retry budget: a ping is dead-lettered (a count, no
// retained item) and a notice is abandoned. The subscription continues.
func (s *Service) exhaust(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	return s.settle(ctx, c, row, row.PendingKind == db.MCPEventPendingEvent, "")
}

// failed records a failed attempt (§9.1, §9.2): it ages the endpoint, ends a
// dead one, and either schedules the next attempt or exhausts the budget.
func (s *Service) failed(ctx context.Context, c *claim, row db.MCPEventSubscription, number int, category string, retryAfter time.Duration) stepResult {
	receiverFailure := category != "destination_not_allowed"
	for {
		now := s.now().UTC()
		since := now
		if row.FailedSince != nil {
			since = *row.FailedSince
		}
		if receiverFailure && row.State == db.MCPEventStateActive && row.FailedAttempts+1 >= deadEndpointAttempts && now.Sub(since) > deadEndpointAge {
			body, id, err := terminatedBody("endpoint_dead")
			if err != nil {
				return stepError
			}
			zero, reason := 0, "endpoint_dead"
			state := db.MCPEventStateEnded
			ok, err := s.write(ctx, c, db.MCPEventDeliveryUpdate{
				PendingMessageID: row.PendingMessageID, RecordFailure: true, LastError: &category,
				ExpectedFailureHistory: &db.MCPEventFailureHistory{Since: row.FailedSince, Attempts: row.FailedAttempts},
				State:                  &state, EndReason: &reason, EndedAt: &now, BumpGeneration: true,
				Pending:      &db.MCPEventPending{Kind: db.MCPEventPendingTerminated, MessageID: id, Body: body},
				AttemptCount: &zero, ClearNextAttempt: true,
			})
			if err != nil {
				return s.writeFailed(ctx, err)
			}
			if ok {
				return stepAgain
			}
			// A successful refresh keeps this claim and pending message, but
			// clears endpoint aging. Re-evaluate that history and record this
			// same attempt against the new run without sending it again.
			current, err := s.store.GetMCPEventSubscription(ctx, row.ID)
			if err != nil {
				return s.writeFailed(ctx, err)
			}
			c.mu.Lock()
			owned := current.Incarnation == c.incarnation && current.Generation == c.gen && current.ClaimToken == c.token
			c.mu.Unlock()
			if !owned || current.PendingMessageID != row.PendingMessageID || ctx.Err() != nil {
				return stepLost
			}
			row = current
			continue
		}
		if number >= maxAttempts {
			zero := 0
			in := db.MCPEventDeliveryUpdate{
				PendingMessageID: row.PendingMessageID, RecordFailure: receiverFailure, ClearFailure: !receiverFailure, LastError: &category,
				ClearPending: true, AttemptCount: &zero, ClearNextAttempt: true,
				IncrementDeadLetter: row.PendingKind == db.MCPEventPendingEvent,
			}
			ok, err := s.write(ctx, c, in)
			if err != nil || !ok {
				return s.writeFailed(ctx, err)
			}
			return stepAgain
		}
		delay := retryDelays[number-1]
		delay += time.Duration(float64(delay) * jitterFraction * min(max(s.delivery.jitter(), 0), 1))
		delay = max(delay, retryAfter)
		next := now.Add(delay)
		ok, err := s.write(ctx, c, db.MCPEventDeliveryUpdate{
			PendingMessageID: row.PendingMessageID, RecordFailure: receiverFailure, ClearFailure: !receiverFailure, LastError: &category,
			NextAttemptAt: &next, ReleaseClaim: true,
		})
		if err != nil || !ok {
			return s.writeFailed(ctx, err)
		}
		return stepBusy
	}
}
