package mcpevents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	scanPageSize         = 100
	maxScanPages         = 16
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
		return s.attempt(ctx, c, row, nil)
	}
	if row.State != db.MCPEventStateActive {
		return stepIdle
	}
	if row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
		return s.end(ctx, c, row, "expired", false)
	}
	return s.advance(ctx, c, row)
}

func principalOf(row db.MCPEventSubscription) Principal {
	return Principal{Authority: row.PrincipalAuthority, Subject: row.PrincipalSubject, Actor: row.Actor}
}

// catalogVersion fingerprints what a subscription can see: its visible
// project set and, for an issue filter, whether that issue is visible. The
// row stores the version its subscriber was last told about, so a restart or
// another daemon process compares against the same baseline. Grants that
// change visibility change this set; access changes that leave it alone, such
// as another user's new project, do not. The issue part matters because the
// filter drops other issues' events, yet deleting or restoring an ancestor
// can hide or reveal the selected issue under a scoped credential.
func catalogVersion(projects []Project, issueVisible bool) string {
	h := sha256.New()
	for _, project := range projects {
		_, _ = h.Write([]byte(project.UID))
		_, _ = h.Write([]byte{0})
	}
	if issueVisible {
		_, _ = h.Write([]byte("issue-visible"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// selectedIssueVisible reports whether an issue filter's issue is visible
// now. A subscription without a filter reports false, a fixed value.
func selectedIssueVisible(ctx context.Context, auth Authorized, args Arguments) (bool, error) {
	if args.IssueUID == "" {
		return false, nil
	}
	err := auth.AuthorizeOccurrence(ctx, Occurrence{IssueUID: args.IssueUID})
	if errors.Is(err, ErrOccurrenceHidden) {
		return false, nil
	}
	return err == nil, err
}

// advance reads the archive after the settled cursor and stages the next
// occurrence for delivery, settling rows that need no delivery.
func (s *Service) advance(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	var args Arguments
	if err := json.Unmarshal(row.Arguments, &args); err != nil {
		// Stored arguments that cannot be read never will be: end the row
		// instead of retrying forever. No notice: nothing can be trusted to
		// build one for it.
		return s.endPermanent(ctx, c, row, "arguments_unreadable")
	}
	// Every delivery attempt starts from a fresh Reacquire. An Authorized is a
	// snapshot of the principal's grants; the host Revalidate hook only
	// rechecks what can change inside that snapshot. The worker therefore
	// never holds an Authorized across attempts or retries: a revoked,
	// narrowed or expired principal must be seen by the very next attempt.
	auth, err := s.authority.Reacquire(ctx, principalOf(row))
	if err != nil {
		return s.authorityFailure(ctx, c, row, err)
	}
	projects, err := ResolveProjects(ctx, auth, args)
	if err != nil {
		return s.authorityFailure(ctx, c, row, err)
	}
	issueVisible, err := selectedIssueVisible(ctx, auth, args)
	if err != nil {
		return s.authorityFailure(ctx, c, row, err)
	}
	if version := catalogVersion(projects, issueVisible); version != row.CatalogVersion {
		// Catalog removals can hide their own event; access grants can expose
		// requests behind this cursor. Signal a reload without project or issue
		// identities before scanning under the new visibility.
		return s.queueGap(ctx, c, row, row.CursorEventID, row.CursorEventID, &version)
	}
	after := row.CursorEventID
	scanned := false
	// inPlace is set once a maxAgeMs skip needs a gap; skipTo is the last
	// stale occurrence in the run. The control reports the settled position
	// from before the run, while the row's own cursor steps over all of it.
	inPlace, skipTo := false, int64(0)
	for range maxScanPages {
		start := after
		scan, err := Scan(ctx, auth, s.daemonUID, args, projects, after, scanPageSize)
		if err != nil {
			return s.authorityFailure(ctx, c, row, err)
		}
		if scan.Reset {
			// A reset above the cursor: move to head and tell the client.
			head := max(scan.ResetHead, scan.Next, row.CursorEventID)
			return s.queueGap(ctx, c, row, head, head, nil)
		}
		for _, m := range scan.Matches {
			if staleReplay(row, m) {
				if row.GapUnacked {
					// The subscribe response already told the client; skip
					// quietly rather than queue a duplicate control.
					after = max(after, m.SourceEventID)
					continue
				}
				// Start (or extend) one in-place gap over the whole run of
				// stale occurrences, so a long replay costs a single control
				// and a single continuity bump.
				skipTo, inPlace = m.SourceEventID, true
				continue
			}
			// A departure-side move arrives as the from-project with no issue,
			// so this authorizes against the project the subscriber can see.
			err := auth.AuthorizeOccurrence(ctx, Occurrence{ProjectUID: m.ProjectUID, IssueUID: m.IssueUID})
			if err == nil && m.NeedsRequest {
				err = requireRequest(ctx, auth, m.IssueUID, args.For)
			}
			switch {
			case err == nil:
				if inPlace {
					return s.queueGap(ctx, c, row, row.CursorEventID, skipTo, nil)
				}
				return s.stage(ctx, c, row, args, auth, m)
			case errors.Is(err, ErrOccurrenceHidden):
				after = max(after, m.SourceEventID)
			default:
				return s.authorityFailure(ctx, c, row, err)
			}
		}
		// Nothing on this page needs delivery. A scoped read's Next only moves
		// with events, so a page that does not advance ends the scan.
		after = max(after, scan.Next)
		if after == start {
			scanned = true
			break
		}
	}
	if inPlace {
		return s.queueGap(ctx, c, row, row.CursorEventID, skipTo, nil)
	}
	if after > row.CursorEventID {
		in := db.MCPEventDeliveryUpdate{CursorEventID: &after}
		clearReplayPast(&in, row, after)
		ok, err := s.write(ctx, c, in)
		if err != nil || !ok {
			return s.writeFailed(ctx, err)
		}
	}
	if !scanned {
		// The page budget ran out with more to read.
		return stepAgain
	}
	s.markIdle(row, c.head, projects)
	return stepIdle
}

// requireRequest hides a lifecycle change of an issue that holds no request
// for the recipient, because the recipient's inbox did not change.
func requireRequest(ctx context.Context, auth Authorized, issueUID, recipient string) error {
	concerns, err := auth.LifecycleMayChangeInbox(ctx, issueUID, recipient)
	if err != nil {
		return err
	}
	if !concerns {
		return ErrOccurrenceHidden
	}
	return nil
}

// staleReplay reports whether a maxAgeMs replay filter skips the occurrence.
func staleReplay(row db.MCPEventSubscription, m Match) bool {
	return row.ReplayCutoff != nil && row.ReplayHead != nil && m.SourceEventID <= *row.ReplayHead && m.Timestamp.Before(*row.ReplayCutoff)
}

func clearReplayPast(in *db.MCPEventDeliveryUpdate, row db.MCPEventSubscription, cursor int64) {
	if row.ReplayHead != nil && cursor >= *row.ReplayHead {
		in.ClearReplay = true
	}
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
	ok, err := s.write(ctx, c, in)
	if err != nil || !ok {
		if err == nil && in.RequireExpired && ctx.Err() == nil {
			// Refresh preserves the claim and generation. Reload its lifetime
			// before deciding what to deliver; the loop still checks fencing.
			return stepAgain
		}
		return s.writeFailed(ctx, err)
	}
	if notify {
		return stepAgain
	}
	return stepIdle
}

// queueGap queues a gap control (§9.3). bodyPos is the position the control
// reports to the client and the watermark it settles at; cursor is where the
// row's own cursor moves. A reset-to-head gap uses the head for both. An
// in-place gap reports the settled position while the row steps over the
// skipped occurrence, so the client's checkpoint never passes undelivered
// replay.
// A non-nil catalog records the visible project set the gap reports.
func (s *Service) queueGap(ctx context.Context, c *claim, row db.MCPEventSubscription, bodyPos, cursor int64, catalog *string) stepResult {
	random, err := randomHex(16)
	if err != nil {
		return stepError
	}
	reported := EncodeCursor(s.key, row.ID, Cursor{Incarnation: row.Incarnation, ContinuityGeneration: row.ContinuityGeneration + 1, EventID: bodyPos})
	body, err := json.Marshal(struct {
		Type   string `json:"type"`
		Cursor string `json:"cursor"`
	}{"gap", reported})
	if err != nil {
		return stepError
	}
	zero, gap := 0, true
	in := db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID,
		BumpGeneration:   true, BumpContinuity: true, GapUnacked: &gap, CursorEventID: &cursor,
		Pending:      &db.MCPEventPending{Kind: db.MCPEventPendingGap, MessageID: "msg_gap_" + random, Body: body, NextCursor: &bodyPos},
		AttemptCount: &zero, ClearNextAttempt: true, CatalogVersion: catalog,
	}
	clearReplayPast(&in, row, cursor)
	ok, err := s.write(ctx, c, in)
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	return stepAgain
}

// stage persists the next occurrence as the row's pending delivery and makes
// its first attempt. The body is fixed here and resent byte for byte on every
// retry.
func (s *Service) stage(ctx context.Context, c *claim, row db.MCPEventSubscription, args Arguments, auth Authorized, m Match) stepResult {
	body, err := json.Marshal(Envelope{
		EventID: m.EventKey, Name: EventName, Timestamp: m.Timestamp.UTC().Format(time.RFC3339Nano),
		Data: Payload{
			Recipient: args.For, Reason: m.Reason, ProjectUID: m.ProjectUID, IssueUID: m.IssueUID, SourceEventUID: m.SourceEventUID,
		},
		Cursor: s.cursorFor(row, m.SourceEventID),
	})
	if err != nil || len(body) > MaxEnvelopeBytes {
		// This occurrence can never be sent. Dead-letter it and continue.
		slog.Warn("MCP Events occurrence dropped", "category", "envelope_unsendable")
		next := m.SourceEventID
		in := db.MCPEventDeliveryUpdate{CursorEventID: &next, IncrementDeadLetter: true}
		clearReplayPast(&in, row, next)
		ok, err := s.write(ctx, c, in)
		if err != nil || !ok {
			return s.writeFailed(ctx, err)
		}
		return stepAgain
	}
	next, zero := m.SourceEventID, 0
	ok, err := s.write(ctx, c, db.MCPEventDeliveryUpdate{
		Pending:      &db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: m.EventKey, Body: body, NextCursor: &next},
		AttemptCount: &zero, ClearNextAttempt: true,
	})
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	row.PendingKind, row.PendingMessageID, row.PendingBody, row.PendingNextCursor, row.AttemptCount = db.MCPEventPendingEvent, m.EventKey, body, &next, 0
	return s.attempt(ctx, c, row, auth)
}

// attempt makes one delivery attempt of the row's pending item. auth, when
// non-nil, is the decision the caller just reacquired for this attempt.
func (s *Service) attempt(ctx context.Context, c *claim, row db.MCPEventSubscription, auth Authorized) stepResult {
	now := s.now().UTC()
	isEvent := row.PendingKind == db.MCPEventPendingEvent
	if row.AttemptCount >= maxAttempts {
		// A previous claimant died during the last attempt.
		return s.exhaust(ctx, c, row)
	}
	if isEvent || row.PendingKind == db.MCPEventPendingGap {
		// Events and gaps require a live grant on every attempt, including
		// retries that run before the expiry sweep. Termination notices may
		// still be delivered after the grant has ended.
		if row.State != db.MCPEventStateActive {
			return s.settle(ctx, c, row, false, "")
		}
		if row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
			return s.end(ctx, c, row, "expired", false)
		}
	}
	if isEvent || row.PendingKind == db.MCPEventPendingGap {
		if auth == nil {
			// Every attempt reacquires the principal, including retries (see
			// advance for why the host Revalidate contract needs this).
			var err error
			if auth, err = s.authority.Reacquire(ctx, principalOf(row)); err != nil {
				return s.authorityFailure(ctx, c, row, err)
			}
		}
	}
	if row.PendingKind == db.MCPEventPendingGap {
		if auth == nil {
			return s.authorityFailure(ctx, c, row, ErrAuthorityUnavailable)
		}
		var args Arguments
		if err := json.Unmarshal(row.Arguments, &args); err != nil {
			return s.endPermanent(ctx, c, row, "arguments_unreadable")
		}
		if _, err := ResolveProjects(ctx, auth, args); err != nil {
			return s.authorityFailure(ctx, c, row, err)
		}
	}
	if isEvent {
		if auth == nil {
			return s.authorityFailure(ctx, c, row, ErrAuthorityUnavailable)
		}
		var pending struct {
			Data struct {
				ProjectUID string `json:"project_uid"`
				IssueUID   string `json:"issue_uid"`
			} `json:"data"`
		}
		if err := json.Unmarshal(row.PendingBody, &pending); err != nil {
			return s.settle(ctx, c, row, false, "")
		}
		err := auth.AuthorizeOccurrence(ctx, Occurrence{ProjectUID: pending.Data.ProjectUID, IssueUID: pending.Data.IssueUID})
		switch {
		case err == nil:
		case errors.Is(err, ErrOccurrenceHidden):
			// Visibility changed since the item was queued: drop it unsent.
			return s.settle(ctx, c, row, false, "")
		default:
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
	// A 410 says the endpoint is gone for good, as Standard Webhooks senders
	// treat it: end the subscription with no notice, which could not arrive.
	// A control for an already ended row is abandoned.
	case sendErr == nil && receipt.StatusCode == http.StatusGone:
		if row.State != db.MCPEventStateActive {
			return s.settle(ctx, c, row, false, "http_4xx")
		}
		return s.end(ctx, c, row, "endpoint_gone", false)
	// A 413 comes from a live endpoint rejecting this one occurrence, so it
	// neither records nor clears failure aging (§9.2).
	case sendErr == nil && receipt.StatusCode == http.StatusRequestEntityTooLarge:
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

// acknowledge settles a 2xx: the item is done, failure aging clears, and an
// acknowledged gap control clears gap_unacked.
func (s *Service) acknowledge(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	now := s.now().UTC()
	zero, none := 0, ""
	in := db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID, ClearPending: true, AttemptCount: &zero, ClearNextAttempt: true,
		ClearFailure: true, LastDeliveryAt: &now, LastError: &none,
	}
	switch row.PendingKind {
	case db.MCPEventPendingEvent:
		settleCursor(&in, row)
	case db.MCPEventPendingGap:
		acknowledged := false
		in.GapUnacked = &acknowledged
	}
	ok, err := s.write(ctx, c, in)
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	return stepAgain
}

// settleCursor moves an event's settlement watermark into the update.
func settleCursor(in *db.MCPEventDeliveryUpdate, row db.MCPEventSubscription) {
	if row.PendingNextCursor == nil {
		return
	}
	next := *row.PendingNextCursor
	in.CursorEventID = &next
	clearReplayPast(in, row, next)
}

// settle drops the pending item without a successful delivery: a 410 or 413,
// an unauthorized occurrence, or an item for a row that is no longer active.
// deadLetter counts it. The cursor settles for events so the next occurrence
// still delivers, and next_attempt_at clears so the row stays due.
func (s *Service) settle(ctx context.Context, c *claim, row db.MCPEventSubscription, deadLetter bool, category string) stepResult {
	zero := 0
	in := db.MCPEventDeliveryUpdate{
		PendingMessageID: row.PendingMessageID, ClearPending: true, AttemptCount: &zero, ClearNextAttempt: true,
	}
	if category != "" {
		in.LastError = &category
	}
	if row.PendingKind == db.MCPEventPendingEvent {
		settleCursor(&in, row)
	}
	in.IncrementDeadLetter = deadLetter
	ok, err := s.write(ctx, c, in)
	if err != nil || !ok {
		return s.writeFailed(ctx, err)
	}
	return stepAgain
}

// exhaust ends an item's retry budget: an event is dead-lettered (a count, no
// retained item) and a control is abandoned. The subscription continues.
func (s *Service) exhaust(ctx context.Context, c *claim, row db.MCPEventSubscription) stepResult {
	return s.settle(ctx, c, row, row.PendingKind == db.MCPEventPendingEvent, "")
}

// failed records a failed attempt (§9.1, §9.2): it ages the endpoint, ends a
// dead one, and either schedules the next attempt or exhausts the budget.
func (s *Service) failed(ctx context.Context, c *claim, row db.MCPEventSubscription, number int, category string, retryAfter time.Duration) stepResult {
	for {
		now := s.now().UTC()
		since := now
		if row.FailedSince != nil {
			since = *row.FailedSince
		}
		if row.State == db.MCPEventStateActive && row.FailedAttempts+1 >= deadEndpointAttempts && now.Sub(since) > deadEndpointAge {
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
				PendingMessageID: row.PendingMessageID, RecordFailure: true, LastError: &category,
				ClearPending: true, AttemptCount: &zero, ClearNextAttempt: true,
				IncrementDeadLetter: row.PendingKind == db.MCPEventPendingEvent,
			}
			if row.PendingKind == db.MCPEventPendingEvent {
				settleCursor(&in, row)
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
			PendingMessageID: row.PendingMessageID, RecordFailure: true, LastError: &category,
			NextAttemptAt: &next, ReleaseClaim: true,
		})
		if err != nil || !ok {
			return s.writeFailed(ctx, err)
		}
		return stepBusy
	}
}
