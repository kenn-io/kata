package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	katauid "go.kenn.io/kata/internal/uid"
)

var (
	// ErrClaimRequired is retained for legacy claim-action error mapping.
	ErrClaimRequired = errors.New("claim required")
	// ErrClaimDenied reports that another holder owns the relevant claim.
	ErrClaimDenied = errors.New("claim denied")
	// ErrClaimNotHeld reports release or renew by a non-holder.
	ErrClaimNotHeld = errors.New("claim not held")
	// ErrClaimExpired reports that the caller's matching timed claim is stale.
	ErrClaimExpired = errors.New("claim expired")
	// ErrClaimValidation reports an invalid claim request or state transition.
	ErrClaimValidation = errors.New("claim validation")
	// ErrPendingClaimNotAuthoritative reports that a pending claim cannot prove exclusivity.
	ErrPendingClaimNotAuthoritative = errors.New("pending claim not authoritative")
)

const (
	minTimedClaimTTL = time.Minute
	maxTimedClaimTTL = 24 * time.Hour
)

type claimStore interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AcquireClaim creates a live claim for an issue, or returns the current live
// holder when arbitration denies the request.
func (d *DB) AcquireClaim(ctx context.Context, p AcquireClaimParams) (LeaseResult, error) {
	now := claimNow(p.Now)
	if err := validateClaimPrincipal(p.Principal); err != nil {
		return LeaseResult{}, err
	}
	if p.ClaimKind != "hard" && p.ClaimKind != "timed" {
		return LeaseResult{}, fmt.Errorf("%w: claim_kind must be hard or timed", ErrClaimValidation)
	}
	var expiresAt *time.Time
	if p.ClaimKind == "timed" {
		if err := validateTimedClaimTTL(p.TTL); err != nil {
			return LeaseResult{}, err
		}
		expires := now.Add(p.TTL).UTC()
		expiresAt = &expires
	}

	var out LeaseResult
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, projectName, err := resolveClaimIssueTx(ctx, conn, p.ProjectID, p.IssueRef)
		if err != nil {
			return err
		}
		expiredEvents, err := d.expireTimedClaimsTx(ctx, conn, now, 0)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, expiredEvents...)
		live, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if err == nil {
			out = resultForClaimWithEvents(live, sameClaimPrincipal(live, p.Principal), out.Events)
			if sameClaimPrincipal(live, p.Principal) {
				return nil
			}
			return ErrClaimDenied
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		claimUID, err := katauid.New()
		if err != nil {
			return fmt.Errorf("generate claim uid: %w", err)
		}
		acquiredAt := now.UTC().Format(sqliteTimeFormat)
		var expiresValue any
		if expiresAt != nil {
			expiresValue = expiresAt.UTC().Format(sqliteTimeFormat)
		}
		res, err := conn.ExecContext(ctx, `
			INSERT INTO issue_claims(
			  claim_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
			  client_kind, purpose, claim_kind, acquired_at, expires_at, updated_at
			)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			claimUID, issue.ProjectID, issue.ID, issue.UID, p.Principal.Holder,
			p.Principal.HolderInstanceUID, p.Principal.ClientKind, p.Purpose,
			p.ClaimKind, acquiredAt, expiresValue, acquiredAt)
		if err != nil {
			return fmt.Errorf("insert issue claim: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("issue claim last id: %w", err)
		}
		claim, err := claimByIDTx(ctx, conn, id)
		if err != nil {
			return err
		}
		evt, err := d.insertClaimEventTx(ctx, conn, claimEventInput{
			ProjectID: issue.ProjectID, ProjectName: projectName, IssueID: issue.ID,
			Type: "claim.acquired", Actor: p.Principal.Holder, Claim: claim,
		})
		if err != nil {
			return err
		}
		out = resultForClaimWithEvents(claim, true, out.Events)
		out.Event = &evt
		return nil
	})
	return out, err
}

// RenewClaim extends a live timed claim held by the same principal.
func (d *DB) RenewClaim(ctx context.Context, p RenewClaimParams) (LeaseResult, error) {
	now := claimNow(p.Now)
	if err := validateClaimPrincipal(p.Principal); err != nil {
		return LeaseResult{}, err
	}
	if err := validateTimedClaimTTL(p.TTL); err != nil {
		return LeaseResult{}, err
	}

	var out LeaseResult
	expiredOutcome := false
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, _, err := resolveClaimIssueTx(ctx, conn, p.ProjectID, p.IssueRef)
		if err != nil {
			return err
		}
		liveBefore, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		expiredEvents, err := d.expireTimedClaimsTx(ctx, conn, now, 0)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, expiredEvents...)
		live, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if errors.Is(err, ErrNotFound) {
			if claimExpiredThisPass(liveBefore, p.Principal, now) {
				expired, expiredErr := claimByIDTx(ctx, conn, liveBefore.ID)
				if expiredErr != nil {
					return expiredErr
				}
				out = resultForClaimWithEvents(expired, false, out.Events)
				expiredOutcome = true
				return nil
			}
			return ErrClaimNotHeld
		}
		if err != nil {
			return err
		}
		out = resultForClaimWithEvents(live, sameClaimPrincipal(live, p.Principal), out.Events)
		if !sameClaimPrincipal(live, p.Principal) {
			return ErrClaimNotHeld
		}
		if live.ClaimKind != "timed" {
			return fmt.Errorf("%w: hard claims cannot be renewed", ErrClaimValidation)
		}
		expiresAt := now.Add(p.TTL).UTC().Format(sqliteTimeFormat)
		if _, err := conn.ExecContext(ctx, `
			UPDATE issue_claims
			   SET expires_at = ?, revision = revision + 1, updated_at = ?
			 WHERE id = ?`,
			expiresAt, now.UTC().Format(sqliteTimeFormat), live.ID); err != nil {
			return fmt.Errorf("renew issue claim: %w", err)
		}
		renewed, err := claimByIDTx(ctx, conn, live.ID)
		if err != nil {
			return err
		}
		out = resultForClaimWithEvents(renewed, true, out.Events)
		return nil
	})
	if err == nil && expiredOutcome {
		err = ErrClaimExpired
	}
	return out, err
}

// ReleaseClaim releases a live claim only when the requester is the holder.
func (d *DB) ReleaseClaim(ctx context.Context, p ReleaseClaimParams) (LeaseResult, error) {
	now := claimNow(p.Now)
	if err := validateClaimPrincipal(p.Principal); err != nil {
		return LeaseResult{}, err
	}

	var out LeaseResult
	expiredOutcome := false
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, projectName, err := resolveClaimIssueTx(ctx, conn, p.ProjectID, p.IssueRef)
		if err != nil {
			return err
		}
		liveBefore, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		expiredEvents, err := d.expireTimedClaimsTx(ctx, conn, now, 0)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, expiredEvents...)
		live, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if errors.Is(err, ErrNotFound) {
			if claimExpiredThisPass(liveBefore, p.Principal, now) {
				expired, expiredErr := claimByIDTx(ctx, conn, liveBefore.ID)
				if expiredErr != nil {
					return expiredErr
				}
				out = resultForClaimWithEvents(expired, false, out.Events)
				expiredOutcome = true
				return nil
			}
			return ErrClaimNotHeld
		}
		if err != nil {
			return err
		}
		out = resultForClaimWithEvents(live, sameClaimPrincipal(live, p.Principal), out.Events)
		if !sameClaimPrincipal(live, p.Principal) {
			return ErrClaimNotHeld
		}
		released, evt, err := d.releaseClaimTx(ctx, conn, live, issue.ID, projectName,
			"claim.released", p.Principal.Holder, p.Reason, now)
		if err != nil {
			return err
		}
		out = resultForClaimWithEvents(released, true, out.Events)
		out.Event = &evt
		return nil
	})
	if err == nil && expiredOutcome {
		err = ErrClaimExpired
	}
	return out, err
}

// ForceReleaseClaim releases any live claim for the issue. Authorization is
// intentionally enforced above this DB helper.
func (d *DB) ForceReleaseClaim(ctx context.Context, p ForceReleaseClaimParams) (LeaseResult, error) {
	now := claimNow(p.Now)
	if strings.TrimSpace(p.Actor) == "" {
		return LeaseResult{}, fmt.Errorf("%w: actor is required", ErrClaimValidation)
	}

	var out LeaseResult
	expiredOutcome := false
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, projectName, err := resolveClaimIssueTx(ctx, conn, p.ProjectID, p.IssueRef)
		if err != nil {
			return err
		}
		liveBefore, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		expiredEvents, err := d.expireTimedClaimsTx(ctx, conn, now, 0)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, expiredEvents...)
		live, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if errors.Is(err, ErrNotFound) {
			if claimTimedExpiredThisPass(liveBefore, now) {
				expired, expiredErr := claimByIDTx(ctx, conn, liveBefore.ID)
				if expiredErr != nil {
					return expiredErr
				}
				out = resultForClaimWithEvents(expired, false, out.Events)
				expiredOutcome = true
				return nil
			}
			return ErrClaimNotHeld
		}
		if err != nil {
			return err
		}
		released, evt, err := d.releaseClaimTx(ctx, conn, live, issue.ID, projectName,
			"claim.force_released", p.Actor, p.Reason, now)
		if err != nil {
			return err
		}
		out = resultForClaimWithEvents(released, true, out.Events)
		out.Event = &evt
		return nil
	})
	if err == nil && expiredOutcome {
		err = ErrClaimExpired
	}
	return out, err
}

// ClaimStatus returns the live claim after expiring stale timed claims.
func (d *DB) ClaimStatus(ctx context.Context, projectID int64, issueRef string, now time.Time) (ClaimStatus, error) {
	now = claimNow(now)
	out := ClaimStatus{HubNow: now}
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, _, err := resolveClaimIssueTx(ctx, conn, projectID, issueRef)
		if err != nil {
			return err
		}
		expiredEvents, err := d.expireTimedClaimsTx(ctx, conn, now, 0)
		if err != nil {
			return err
		}
		live, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if errors.Is(err, ErrNotFound) {
			out = ClaimStatus{HubNow: now, Events: expiredEvents}
			return nil
		}
		if err != nil {
			return err
		}
		out = ClaimStatus{
			Held:   true,
			Holder: principalForClaim(live),
			Claim:  &live,
			HubNow: now,
			Events: expiredEvents,
		}
		return nil
	})
	return out, err
}

// ClaimStatusReadOnly returns the currently cached live claim without expiring
// timed claims or writing audit events. It is for display-only surfaces that
// must not mutate local spoke state.
func (d *DB) ClaimStatusReadOnly(ctx context.Context, projectID int64, issueRef string, now time.Time) (ClaimStatus, error) {
	now = claimNow(now)
	issue, _, err := resolveClaimIssueTx(ctx, d, projectID, issueRef)
	if err != nil {
		return ClaimStatus{}, err
	}
	live, err := liveClaimForIssueTx(ctx, d, issue.UID)
	if errors.Is(err, ErrNotFound) {
		return ClaimStatus{HubNow: now}, nil
	}
	if err != nil {
		return ClaimStatus{}, err
	}
	return ClaimStatus{
		Held:   true,
		Holder: principalForClaim(live),
		Claim:  &live,
		HubNow: now,
	}, nil
}

// EnqueuePendingClaim stores or returns an unresolved offline claim request.
func (d *DB) EnqueuePendingClaim(ctx context.Context, p PendingClaimParams) (PendingClaimRequest, error) {
	now := claimNow(p.Now)
	if err := validateClaimPrincipal(p.Principal); err != nil {
		return PendingClaimRequest{}, err
	}
	if p.ClaimKind != "hard" && p.ClaimKind != "timed" {
		return PendingClaimRequest{}, fmt.Errorf("%w: claim_kind must be hard or timed", ErrClaimValidation)
	}
	var ttlSeconds *int64
	if p.ClaimKind == "timed" {
		if err := validateTimedClaimTTL(p.TTL); err != nil {
			return PendingClaimRequest{}, err
		}
		seconds := int64(p.TTL / time.Second)
		ttlSeconds = &seconds
	}

	var out PendingClaimRequest
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, _, err := resolveClaimIssueTx(ctx, conn, p.ProjectID, p.IssueRef)
		if err != nil {
			return err
		}
		out, err = activePendingClaimRequestForPrincipalTx(ctx, conn, issue.UID, p.Principal)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		requestUID, err := katauid.New()
		if err != nil {
			return fmt.Errorf("generate pending claim request uid: %w", err)
		}
		res, err := conn.ExecContext(ctx, `
			INSERT INTO pending_claim_requests(
			  request_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
			  client_kind, claim_kind, ttl_seconds, purpose, requested_at
			)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			requestUID, issue.ProjectID, issue.ID, issue.UID, p.Principal.Holder,
			p.Principal.HolderInstanceUID, p.Principal.ClientKind, p.ClaimKind, ttlSeconds,
			p.Purpose, now.UTC().Format(sqliteTimeFormat))
		if err != nil {
			return fmt.Errorf("insert pending claim request: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("pending claim request last id: %w", err)
		}
		out, err = pendingClaimRequestByIDTx(ctx, conn, id)
		return err
	})
	return out, err
}

// ResolvePendingClaim marks a pending request resolved and caches the hub claim.
func (d *DB) ResolvePendingClaim(ctx context.Context, requestUID string, claim IssueClaim) error {
	requestUID = strings.TrimSpace(requestUID)
	if requestUID == "" {
		return ErrNotFound
	}
	return d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		pending, err := pendingClaimRequestByUIDTx(ctx, conn, requestUID)
		if err != nil {
			return err
		}
		if pending.RejectedAt != nil {
			return fmt.Errorf("%w: pending claim rejected", ErrClaimValidation)
		}
		if pending.ResolvedAt != nil {
			return nil
		}
		issue, _, err := resolveClaimIssueTx(ctx, conn, pending.ProjectID, pending.IssueUID)
		if err != nil {
			return err
		}
		if err := validatePendingClaimResolution(pending, issue, claim); err != nil {
			return err
		}
		now := claimNow(claim.UpdatedAt)
		claim.ProjectID = issue.ProjectID
		claim.IssueID = issue.ID
		claim.IssueUID = issue.UID
		if claim.AcquiredAt.IsZero() {
			claim.AcquiredAt = now
		}
		if claim.UpdatedAt.IsZero() {
			claim.UpdatedAt = now
		}
		if err := d.applyClaimStatusTx(ctx, conn, issue.ProjectID, issue.UID, ClaimStatus{
			Held:   true,
			Holder: principalForClaim(claim),
			Claim:  &claim,
			HubNow: now,
		}); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE pending_claim_requests
			   SET resolved_at = ?, last_error = NULL
			 WHERE request_uid = ?`,
			now.UTC().Format(sqliteTimeFormat), requestUID); err != nil {
			return fmt.Errorf("resolve pending claim request: %w", err)
		}
		return nil
	})
}

// RejectPendingClaim marks a pending request terminally rejected.
func (d *DB) RejectPendingClaim(ctx context.Context, requestUID, reason string, now time.Time) error {
	requestUID = strings.TrimSpace(requestUID)
	if requestUID == "" {
		return ErrNotFound
	}
	stamp := claimNow(now).UTC().Format(sqliteTimeFormat)
	return d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE pending_claim_requests
			   SET rejected_at = ?, last_attempt_at = ?, last_error = ?
			 WHERE request_uid = ? AND rejected_at IS NULL AND resolved_at IS NULL`,
			stamp, stamp, reason, requestUID)
		if err != nil {
			return fmt.Errorf("reject pending claim request: %w", err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reject pending claim rows affected: %w", err)
		}
		if changed == 0 {
			_, err := pendingClaimRequestByUIDTx(ctx, conn, requestUID)
			return err
		}
		return nil
	})
}

// ListPendingClaimRequests returns unresolved pending claim requests for a project.
func (d *DB) ListPendingClaimRequests(ctx context.Context, projectID int64, limit int) ([]PendingClaimRequest, error) {
	q := pendingClaimRequestSelect + `
		 WHERE project_id = ? AND rejected_at IS NULL AND resolved_at IS NULL
		 ORDER BY requested_at ASC, id ASC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("list pending claim requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingClaimRequest
	for rows.Next() {
		pending, err := scanPendingClaimRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pending)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending claim requests rows: %w", err)
	}
	return out, nil
}

// ListPendingClaimRequestsForIssue returns unresolved pending claim requests for an issue.
func (d *DB) ListPendingClaimRequestsForIssue(
	ctx context.Context,
	projectID int64,
	issueUID string,
	limit int,
) ([]PendingClaimRequest, error) {
	q := pendingClaimRequestSelect + `
		 WHERE project_id = ? AND issue_uid = ? AND rejected_at IS NULL AND resolved_at IS NULL
		 ORDER BY requested_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.QueryContext(ctx, q, projectID, issueUID)
	if err != nil {
		return nil, fmt.Errorf("list pending claim requests for issue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingClaimRequest
	for rows.Next() {
		pending, err := scanPendingClaimRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pending)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending claim requests for issue rows: %w", err)
	}
	return out, nil
}

// MarkPendingClaimAttempt records a retry attempt and latest retry error.
func (d *DB) MarkPendingClaimAttempt(ctx context.Context, requestUID, lastError string, now time.Time) error {
	requestUID = strings.TrimSpace(requestUID)
	if requestUID == "" {
		return ErrNotFound
	}
	stamp := claimNow(now).UTC().Format(sqliteTimeFormat)
	return d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE pending_claim_requests
			   SET last_attempt_at = ?, last_error = ?
			 WHERE request_uid = ? AND rejected_at IS NULL AND resolved_at IS NULL`,
			stamp, lastError, requestUID)
		if err != nil {
			return fmt.Errorf("mark pending claim attempt: %w", err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("mark pending claim rows affected: %w", err)
		}
		if changed == 0 {
			_, err := pendingClaimRequestByUIDTx(ctx, conn, requestUID)
			return err
		}
		return nil
	})
}

// ClaimStatusRefreshError returns the latest throttling marker for show refresh.
func (d *DB) ClaimStatusRefreshError(
	ctx context.Context,
	projectID int64,
	issueUID string,
) (ClaimStatusRefreshError, error) {
	key := claimStatusRefreshErrorKey(projectID, issueUID)
	if key == "" {
		return ClaimStatusRefreshError{}, ErrNotFound
	}
	var raw string
	err := d.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimStatusRefreshError{}, ErrNotFound
	}
	if err != nil {
		return ClaimStatusRefreshError{}, fmt.Errorf("read claim status refresh error: %w", err)
	}
	var stored struct {
		StatusCode    int       `json:"status_code"`
		LastAttemptAt time.Time `json:"last_attempt_at"`
		LastError     string    `json:"last_error"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return ClaimStatusRefreshError{}, fmt.Errorf("decode claim status refresh error: %w", err)
	}
	return ClaimStatusRefreshError{
		ProjectID:     projectID,
		IssueUID:      strings.TrimSpace(issueUID),
		StatusCode:    stored.StatusCode,
		LastAttemptAt: stored.LastAttemptAt,
		LastError:     stored.LastError,
	}, nil
}

// MarkClaimStatusRefreshError records a transient show claim-status refresh failure.
func (d *DB) MarkClaimStatusRefreshError(
	ctx context.Context,
	projectID int64,
	issueUID string,
	statusCode int,
	lastError string,
	now time.Time,
) error {
	key := claimStatusRefreshErrorKey(projectID, issueUID)
	if key == "" {
		return ErrNotFound
	}
	stored := struct {
		StatusCode    int       `json:"status_code"`
		LastAttemptAt time.Time `json:"last_attempt_at"`
		LastError     string    `json:"last_error"`
	}{
		StatusCode:    statusCode,
		LastAttemptAt: claimNow(now),
		LastError:     lastError,
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode claim status refresh error: %w", err)
	}
	_, err = d.ExecContext(ctx, `
		INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, string(raw))
	if err != nil {
		return fmt.Errorf("mark claim status refresh error: %w", err)
	}
	return nil
}

// ClearClaimStatusRefreshError removes a show claim-status refresh failure marker.
func (d *DB) ClearClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string) error {
	key := claimStatusRefreshErrorKey(projectID, issueUID)
	if key == "" {
		return ErrNotFound
	}
	_, err := d.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("clear claim status refresh error: %w", err)
	}
	return nil
}

func claimStatusRefreshErrorKey(projectID int64, issueUID string) string {
	issueUID = strings.TrimSpace(issueUID)
	if projectID == 0 || issueUID == "" {
		return ""
	}
	return fmt.Sprintf("claim_status_refresh_error:%d:%s", projectID, issueUID)
}

// UpsertClaimCache stores a live claim as the local cached authoritative status.
func (d *DB) UpsertClaimCache(ctx context.Context, claim IssueClaim) error {
	return d.ApplyClaimStatus(ctx, claim.ProjectID, claim.IssueUID, ClaimStatus{
		Held:   true,
		Holder: principalForClaim(claim),
		Claim:  &claim,
		HubNow: claim.UpdatedAt,
	})
}

// ApplyClaimStatus reconciles local cache with a hub claim status response.
func (d *DB) ApplyClaimStatus(ctx context.Context, projectID int64, issueUID string, status ClaimStatus) error {
	return d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		return d.applyClaimStatusTx(ctx, conn, projectID, issueUID, status)
	})
}

// CheckClaimGate verifies whether a holder may mutate a federated issue.
func (d *DB) CheckClaimGate(ctx context.Context, p ClaimGateParams) error {
	now := claimNow(p.Now)
	if err := validateClaimPrincipal(p.Principal); err != nil {
		return err
	}
	return d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		issue, err := resolveClaimGateIssueTx(ctx, conn, p.ProjectID, p.IssueRef)
		if err != nil {
			return err
		}
		live, err := liveClaimForIssueTx(ctx, conn, issue.UID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if live.ClaimKind == "timed" && live.ExpiresAt != nil && !live.ExpiresAt.After(now) {
			return nil
		}
		if !sameClaimGateHolder(live, p.Principal) {
			return ErrClaimDenied
		}
		return nil
	})
}

func resolveClaimGateIssueTx(ctx context.Context, tx claimStore, projectID int64, issueRef string) (Issue, error) {
	issueRef = strings.TrimSpace(issueRef)
	if issueRef == "" {
		return Issue{}, ErrNotFound
	}
	const q = `
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at
		  FROM issues i
		  JOIN projects p ON p.id = i.project_id
		 WHERE i.project_id = ?
		   AND (i.short_id = ? OR i.uid = ?)`
	var issue Issue
	err := tx.QueryRowContext(ctx, q, projectID, issueRef, issueRef).Scan(
		&issue.ID, &issue.UID, &issue.ProjectID, &issue.ProjectUID, &issue.ShortID,
		&issue.Title, &issue.Body, &issue.Status, &issue.ClosedReason, &issue.Owner,
		&issue.Priority, &issue.Author, &issue.Metadata, &issue.Revision,
		&issue.RecurrenceID, &issue.OccurrenceKey, &issue.CreatedAt, &issue.UpdatedAt,
		&issue.ClosedAt, &issue.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNotFound
	}
	if err != nil {
		return Issue{}, fmt.Errorf("resolve claim gate issue: %w", err)
	}
	return issue, nil
}

// ExpireTimedClaims releases stale timed claims and emits one claim.expired
// event for each row this call transitions from live to released.
func (d *DB) ExpireTimedClaims(ctx context.Context, now time.Time, limit int) ([]Event, error) {
	now = claimNow(now)
	var out []Event
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		events, err := d.expireTimedClaimsTx(ctx, conn, now, limit)
		if err != nil {
			return err
		}
		out = events
		return nil
	})
	return out, err
}

// ExpireTimedClaimsForProject releases stale timed claims for one project.
func (d *DB) ExpireTimedClaimsForProject(ctx context.Context, projectID int64, now time.Time, limit int) ([]Event, error) {
	now = claimNow(now)
	var out []Event
	err := d.withImmediateClaimTx(ctx, func(conn *sql.Conn) error {
		events, err := d.expireTimedClaimsForProjectTx(ctx, conn, projectID, now, limit)
		if err != nil {
			return err
		}
		out = events
		return nil
	})
	return out, err
}

// ExpireTimedClaimsTx releases stale timed claims without emitting events.
func ExpireTimedClaimsTx(ctx context.Context, tx claimStore, now time.Time) error {
	stamp := claimNow(now).UTC().Format(sqliteTimeFormat)
	if _, err := tx.ExecContext(ctx, `
		UPDATE issue_claims
		   SET released_at = ?,
		       release_reason = COALESCE(release_reason, 'expired'),
		       revision = revision + 1,
		       updated_at = ?
		 WHERE released_at IS NULL
		   AND claim_kind = 'timed'
		   AND expires_at <= ?`,
		stamp, stamp, stamp); err != nil {
		return fmt.Errorf("expire timed claims: %w", err)
	}
	return nil
}

func (d *DB) expireTimedClaimsTx(ctx context.Context, tx claimStore, now time.Time, limit int) ([]Event, error) {
	expired, err := expiredTimedClaimsTx(ctx, tx, now, limit)
	if err != nil {
		return nil, err
	}
	return d.expireTimedClaimRowsTx(ctx, tx, expired, now)
}

func (d *DB) expireTimedClaimsForProjectTx(
	ctx context.Context,
	tx claimStore,
	projectID int64,
	now time.Time,
	limit int,
) ([]Event, error) {
	expired, err := expiredTimedClaimsForProjectTx(ctx, tx, projectID, now, limit)
	if err != nil {
		return nil, err
	}
	return d.expireTimedClaimRowsTx(ctx, tx, expired, now)
}

func (d *DB) expireTimedClaimRowsTx(
	ctx context.Context,
	tx claimStore,
	expired []IssueClaim,
	now time.Time,
) ([]Event, error) {
	events := make([]Event, 0, len(expired))
	stamp := now.UTC().Format(sqliteTimeFormat)
	for _, claim := range expired {
		res, err := tx.ExecContext(ctx, `
			UPDATE issue_claims
			   SET released_at = ?,
			       release_reason = 'expired',
			       revision = revision + 1,
			       updated_at = ?
			 WHERE id = ? AND released_at IS NULL`,
			stamp, stamp, claim.ID)
		if err != nil {
			return nil, fmt.Errorf("expire timed claim %s: %w", claim.ClaimUID, err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("expire timed claim rows affected: %w", err)
		}
		if changed == 0 {
			continue
		}
		released, err := claimByIDTx(ctx, tx, claim.ID)
		if err != nil {
			return nil, err
		}
		evt, err := d.insertClaimEventTx(ctx, tx, claimEventInput{
			ProjectID: released.ProjectID, IssueID: released.IssueID,
			Type: "claim.expired", Actor: "system", Claim: released, Reason: "expired",
		})
		if err != nil {
			return nil, err
		}
		events = append(events, evt)
	}
	return events, nil
}

func (d *DB) applyClaimStatusTx(ctx context.Context, tx claimStore, projectID int64, issueUID string, status ClaimStatus) error {
	issue, _, err := resolveClaimIssueTx(ctx, tx, projectID, issueUID)
	if err != nil {
		return err
	}
	now := claimNow(status.HubNow)
	if status.Held && status.Claim != nil {
		if err := validateStatusClaimIssueIdentity(issue, *status.Claim); err != nil {
			return err
		}
	}
	latestUpdatedAt, hasLatest, err := latestClaimUpdatedAtForIssueTx(ctx, tx, issue.UID)
	if err != nil {
		return err
	}
	if hasLatest && !status.HubNow.IsZero() && status.HubNow.Before(latestUpdatedAt) {
		return assertSingleLiveClaimTx(ctx, tx, issue.UID)
	}
	live, liveErr := liveClaimForIssueTx(ctx, tx, issue.UID)
	if liveErr != nil && !errors.Is(liveErr, ErrNotFound) {
		return liveErr
	}

	if !status.Held || status.Claim == nil {
		if liveErr == nil {
			if err := releaseCachedClaimTx(ctx, tx, live.ID, "status_refresh", now); err != nil {
				return err
			}
		}
		return assertSingleLiveClaimTx(ctx, tx, issue.UID)
	}

	claim, err := normalizeCachedClaim(status, issue, now)
	if err != nil {
		return err
	}
	if liveErr == nil && live.ClaimUID == claim.ClaimUID {
		if staleSameClaimRefresh(live, claim) {
			return assertSingleLiveClaimTx(ctx, tx, issue.UID)
		}
		if err := updateCachedClaimInPlaceTx(ctx, tx, live.ID, claim); err != nil {
			return err
		}
		return assertSingleLiveClaimTx(ctx, tx, issue.UID)
	}
	if liveErr == nil {
		if err := releaseCachedClaimTx(ctx, tx, live.ID, "status_refresh_replaced", now); err != nil {
			return err
		}
	}
	if err := insertCachedClaimTx(ctx, tx, claim); err != nil {
		return err
	}
	return assertSingleLiveClaimTx(ctx, tx, issue.UID)
}

func validatePendingClaimResolution(pending PendingClaimRequest, issue Issue, claim IssueClaim) error {
	if claim.IssueUID != issue.UID {
		return fmt.Errorf("%w: pending claim issue mismatch", ErrClaimValidation)
	}
	if claim.Holder != pending.Holder {
		return fmt.Errorf("%w: pending claim holder mismatch", ErrClaimValidation)
	}
	if pending.HolderInstanceUID != "" && claim.HolderInstanceUID != pending.HolderInstanceUID {
		return fmt.Errorf("%w: pending claim holder instance mismatch", ErrClaimValidation)
	}
	if claim.ClientKind != pending.ClientKind {
		return fmt.Errorf("%w: pending claim client kind mismatch", ErrClaimValidation)
	}
	if claim.ClaimKind != pending.ClaimKind {
		return fmt.Errorf("%w: pending claim kind mismatch", ErrClaimValidation)
	}
	if claim.ClaimKind == "timed" && claim.ExpiresAt == nil {
		return fmt.Errorf("%w: timed pending claim requires expires_at", ErrClaimValidation)
	}
	return nil
}

func validateStatusClaimIssueIdentity(issue Issue, claim IssueClaim) error {
	if claim.IssueUID != "" && claim.IssueUID != issue.UID {
		return fmt.Errorf("%w: status claim issue mismatch", ErrClaimValidation)
	}
	return nil
}

func normalizeCachedClaim(status ClaimStatus, issue Issue, now time.Time) (IssueClaim, error) {
	claim := *status.Claim
	claim.ProjectID = issue.ProjectID
	claim.IssueID = issue.ID
	claim.IssueUID = issue.UID
	if claim.Holder == "" {
		claim.Holder = status.Holder.Holder
	}
	if claim.HolderInstanceUID == "" {
		claim.HolderInstanceUID = status.Holder.HolderInstanceUID
	}
	if claim.ClientKind == "" {
		claim.ClientKind = status.Holder.ClientKind
	}
	if claim.AcquiredAt.IsZero() {
		claim.AcquiredAt = now
	}
	if claim.UpdatedAt.IsZero() {
		claim.UpdatedAt = now
	}
	if claim.Revision == 0 {
		claim.Revision = 1
	}
	if !katauid.Valid(claim.ClaimUID) {
		return IssueClaim{}, fmt.Errorf("%w: invalid claim uid", ErrClaimValidation)
	}
	if err := validateClaimPrincipal(principalForClaim(claim)); err != nil {
		return IssueClaim{}, err
	}
	if claim.ClaimKind != "hard" && claim.ClaimKind != "timed" {
		return IssueClaim{}, fmt.Errorf("%w: claim_kind must be hard or timed", ErrClaimValidation)
	}
	if claim.ClaimKind == "timed" && claim.ExpiresAt == nil {
		return IssueClaim{}, fmt.Errorf("%w: timed claim requires expires_at", ErrClaimValidation)
	}
	if claim.ClaimKind == "hard" && claim.ExpiresAt != nil {
		return IssueClaim{}, fmt.Errorf("%w: hard claim cannot expire", ErrClaimValidation)
	}
	return claim, nil
}

func insertCachedClaimTx(ctx context.Context, tx claimStore, claim IssueClaim) error {
	var expires any
	if claim.ExpiresAt != nil {
		expires = claim.ExpiresAt.UTC().Format(sqliteTimeFormat)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO issue_claims(
		  claim_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
		  client_kind, purpose, claim_kind, acquired_at, expires_at, revision, updated_at
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		claim.ClaimUID, claim.ProjectID, claim.IssueID, claim.IssueUID, claim.Holder,
		claim.HolderInstanceUID, claim.ClientKind, claim.Purpose, claim.ClaimKind,
		claim.AcquiredAt.UTC().Format(sqliteTimeFormat), expires, claim.Revision,
		claim.UpdatedAt.UTC().Format(sqliteTimeFormat)); err != nil {
		return fmt.Errorf("insert cached issue claim: %w", err)
	}
	return nil
}

func updateCachedClaimInPlaceTx(ctx context.Context, tx claimStore, id int64, claim IssueClaim) error {
	var expires any
	if claim.ExpiresAt != nil {
		expires = claim.ExpiresAt.UTC().Format(sqliteTimeFormat)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE issue_claims
		   SET holder = ?,
		       holder_instance_uid = ?,
		       client_kind = ?,
		       purpose = ?,
		       claim_kind = ?,
		       acquired_at = ?,
		       expires_at = ?,
		       revision = ?,
		       updated_at = ?
		 WHERE id = ? AND released_at IS NULL`,
		claim.Holder, claim.HolderInstanceUID, claim.ClientKind, claim.Purpose,
		claim.ClaimKind, claim.AcquiredAt.UTC().Format(sqliteTimeFormat), expires,
		claim.Revision, claim.UpdatedAt.UTC().Format(sqliteTimeFormat), id); err != nil {
		return fmt.Errorf("update cached issue claim: %w", err)
	}
	return nil
}

func releaseCachedClaimTx(ctx context.Context, tx claimStore, id int64, reason string, now time.Time) error {
	stamp := now.UTC().Format(sqliteTimeFormat)
	if _, err := tx.ExecContext(ctx, `
		UPDATE issue_claims
		   SET released_at = ?, release_reason = ?, revision = revision + 1, updated_at = ?
		 WHERE id = ? AND released_at IS NULL`,
		stamp, reason, stamp, id); err != nil {
		return fmt.Errorf("release cached issue claim: %w", err)
	}
	return nil
}

func staleSameClaimRefresh(live, incoming IssueClaim) bool {
	if incoming.UpdatedAt.Before(live.UpdatedAt) {
		return true
	}
	return incoming.UpdatedAt.Equal(live.UpdatedAt) && incoming.Revision < live.Revision
}

func assertSingleLiveClaimTx(ctx context.Context, tx claimStore, issueUID string) error {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`,
		issueUID).Scan(&n); err != nil {
		return fmt.Errorf("count live issue claims: %w", err)
	}
	if n > 1 {
		return fmt.Errorf("%w: multiple live claims for issue", ErrClaimValidation)
	}
	return nil
}

func (d *DB) withImmediateClaimTx(ctx context.Context, fn func(*sql.Conn) error) error {
	return RetryLockContention(ctx, func() error {
		conn, err := d.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire conn: %w", err)
		}
		defer func() { _ = conn.Close() }()

		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE TRANSACTION"); err != nil {
			return fmt.Errorf("begin immediate: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			}
		}()

		if err := fn(conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		committed = true
		return nil
	})
}

func (d *DB) releaseClaimTx(
	ctx context.Context,
	tx claimStore,
	claim IssueClaim,
	issueID int64,
	projectName, eventType, actor, reason string,
	now time.Time,
) (IssueClaim, Event, error) {
	stamp := now.UTC().Format(sqliteTimeFormat)
	if _, err := tx.ExecContext(ctx, `
		UPDATE issue_claims
		   SET released_at = ?, release_reason = ?, revision = revision + 1, updated_at = ?
		 WHERE id = ? AND released_at IS NULL`,
		stamp, reason, stamp, claim.ID); err != nil {
		return IssueClaim{}, Event{}, fmt.Errorf("release issue claim: %w", err)
	}
	released, err := claimByIDTx(ctx, tx, claim.ID)
	if err != nil {
		return IssueClaim{}, Event{}, err
	}
	evt, err := d.insertClaimEventTx(ctx, tx, claimEventInput{
		ProjectID: claim.ProjectID, ProjectName: projectName, IssueID: issueID,
		Type: eventType, Actor: actor, Claim: released, Reason: reason,
	})
	if err != nil {
		return IssueClaim{}, Event{}, err
	}
	return released, evt, nil
}

type claimWorkMutationInput struct {
	ProjectID         int64
	ProjectName       string
	IssueID           int64
	IssueUID          string
	OffendingEventUID string
	EventType         string
	Actor             string
	HolderInstanceUID string
	RequireClaim      bool
}

type federationIngestClaimAuditIssue struct {
	UID          string
	RequireClaim bool
}

func (d *DB) annotateFederationIngestClaimWorkTx(
	ctx context.Context,
	tx claimStore,
	projectID int64,
	projectName string,
	ev RemoteEvent,
) ([]Event, error) {
	issueUIDs, err := federationIngestClaimAuditIssueUIDs(ev)
	if err != nil {
		return nil, err
	}
	if len(issueUIDs) == 0 {
		return nil, nil
	}
	var events []Event
	claimsExpired := false
	ensureClaimsExpired := func() error {
		if claimsExpired {
			return nil
		}
		expiredEvents, err := d.expireTimedClaimsForProjectTx(ctx, tx, projectID, time.Now().UTC(), 0)
		if err != nil {
			return err
		}
		events = append(events, expiredEvents...)
		claimsExpired = true
		return nil
	}
	for _, issue := range issueUIDs {
		issueID, err := issueIDByUIDForClaimAuditTx(ctx, tx, projectID, issue.UID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := ensureClaimsExpired(); err != nil {
			return nil, err
		}
		auditEvents, err := d.annotateClaimWorkMutationTx(ctx, tx, claimWorkMutationInput{
			ProjectID:         projectID,
			ProjectName:       projectName,
			IssueID:           issueID,
			IssueUID:          issue.UID,
			OffendingEventUID: ev.EventUID,
			EventType:         ev.Type,
			Actor:             ev.Actor,
			HolderInstanceUID: ev.OriginInstanceUID,
			RequireClaim:      issue.RequireClaim,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, auditEvents...)
	}
	return events, nil
}

func federationIngestClaimAuditIssueUIDs(ev RemoteEvent) ([]federationIngestClaimAuditIssue, error) {
	payload := payloadMap(ev.Payload)
	out := make([]federationIngestClaimAuditIssue, 0, 1)
	seen := map[string]struct{}{}
	add := func(issue federationIngestClaimAuditIssue) {
		uid := issue.UID
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		out = append(out, issue)
	}
	if claimWorkMutationRequiresClaim(ev.Type) {
		issueUID := ""
		if ev.IssueUID != nil {
			issueUID = *ev.IssueUID
		}
		if issueUID == "" {
			uid, err := payloadIssueUID(ev, payload)
			if err != nil {
				return nil, err
			}
			issueUID = uid
		}
		add(federationIngestClaimAuditIssue{UID: issueUID, RequireClaim: true})
	}
	if ev.Type == "issue.snapshot" {
		issueUID := ""
		if ev.IssueUID != nil {
			issueUID = *ev.IssueUID
		}
		if issueUID == "" {
			uid, err := payloadIssueUID(ev, payload)
			if err != nil {
				return nil, err
			}
			issueUID = uid
		}
		add(federationIngestClaimAuditIssue{UID: issueUID, RequireClaim: true})
	}
	if ev.Type == "issue.created" || ev.Type == "issue.snapshot" || claimWorkMutationRequiresPeerClaim(ev.Type) {
		for _, ref := range payloadReferencedIssueUIDs(ev, payload) {
			add(federationIngestClaimAuditIssue{
				UID:          ref,
				RequireClaim: true,
			})
		}
	}
	return out, nil
}

func (d *DB) annotateClaimWorkMutationTx(
	ctx context.Context,
	tx claimStore,
	in claimWorkMutationInput,
) ([]Event, error) {
	hub, err := enabledHubFederationBindingTx(ctx, tx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	if !hub {
		return nil, nil
	}
	events, err := d.expireTimedClaimsForProjectTx(ctx, tx, in.ProjectID, time.Now().UTC(), 0)
	if err != nil {
		return nil, err
	}
	if in.OffendingEventUID == "" {
		uid, err := latestClaimOffendingEventUIDTx(ctx, tx, in.ProjectID, in.IssueUID, in.EventType, in.Actor)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		in.OffendingEventUID = uid
	}
	shouldAuditClaim := in.RequireClaim || claimWorkMutationRequiresClaim(in.EventType)
	live, err := liveClaimForIssueTx(ctx, tx, in.IssueUID)
	if errors.Is(err, ErrNotFound) {
		return events, nil
	}
	if err != nil {
		return nil, err
	}
	if !shouldAuditClaim {
		return events, nil
	}
	if !claimWorkCoveredByLiveClaim(live, in.HolderInstanceUID, in.Actor) {
		evt, err := d.insertClaimEventTx(ctx, tx, claimEventInput{
			ProjectID: in.ProjectID, ProjectName: in.ProjectName, IssueID: in.IssueID,
			Type: "claim.violated", Actor: in.Actor, Claim: live, Reason: "uncovered_work",
			OffendingEventUID: in.OffendingEventUID, OffendingEventType: in.EventType,
			OffendingOriginInstanceUID: in.HolderInstanceUID,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, evt)
	}
	if in.EventType == "issue.closed" {
		_, evt, err := d.releaseClaimTx(ctx, tx, live, in.IssueID, in.ProjectName,
			"claim.released", in.Actor, "issue_closed", time.Now().UTC())
		if err != nil {
			return nil, err
		}
		events = append(events, evt)
	}
	return events, nil
}

func claimWorkMutationRequiresClaim(eventType string) bool {
	switch eventType {
	case "issue.updated", "issue.assigned", "issue.unassigned",
		"issue.priority_set", "issue.priority_cleared",
		"issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored",
		"issue.labeled", "issue.unlabeled", "issue.linked", "issue.unlinked",
		"issue.links_changed", "issue.metadata_updated":
		return true
	default:
		return false
	}
}

func claimWorkMutationRequiresPeerClaim(eventType string) bool {
	switch eventType {
	case "issue.linked", "issue.unlinked", "issue.links_changed":
		return true
	default:
		return false
	}
}

func claimWorkCoveredByLiveClaim(claim IssueClaim, holderInstanceUID, actor string) bool {
	return claim.HolderInstanceUID == holderInstanceUID && claim.Holder == actor
}

func enabledHubFederationBindingTx(ctx context.Context, tx claimStore, projectID int64) (bool, error) {
	var role string
	var enabled int
	err := tx.QueryRowContext(ctx,
		`SELECT role, enabled FROM federation_bindings WHERE project_id = ?`, projectID).
		Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check claim federation binding: %w", err)
	}
	return enabled == 1 && role == string(FederationRoleHub), nil
}

func issueIDByUIDForClaimAuditTx(ctx context.Context, tx claimStore, projectID int64, issueUID string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM issues WHERE project_id = ? AND uid = ?`, projectID, issueUID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lookup claim audit issue: %w", err)
	}
	return id, nil
}

func latestClaimOffendingEventUIDTx(
	ctx context.Context,
	tx claimStore,
	projectID int64,
	issueUID string,
	eventType string,
	actor string,
) (string, error) {
	var uid string
	err := tx.QueryRowContext(ctx, `
		SELECT uid
		  FROM events
		 WHERE project_id = ?
		   AND issue_uid = ?
		   AND type = ?
		   AND actor = ?
		 ORDER BY id DESC
		 LIMIT 1`, projectID, issueUID, eventType, actor).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup claim violation offender event: %w", err)
	}
	return uid, nil
}

type claimEventInput struct {
	ProjectID                  int64
	ProjectName                string
	IssueID                    int64
	Type                       string
	Actor                      string
	Claim                      IssueClaim
	Reason                     string
	OffendingEventUID          string
	OffendingEventType         string
	OffendingOriginInstanceUID string
}

func (d *DB) insertClaimEventTx(ctx context.Context, tx claimStore, in claimEventInput) (Event, error) {
	payload := map[string]any{
		"claim_uid":           in.Claim.ClaimUID,
		"holder":              in.Claim.Holder,
		"holder_instance_uid": in.Claim.HolderInstanceUID,
		"client_kind":         in.Claim.ClientKind,
		"claim_kind":          in.Claim.ClaimKind,
		"purpose":             in.Claim.Purpose,
		"acquired_at":         in.Claim.AcquiredAt.UTC().Format(sqliteTimeFormat),
	}
	if in.Claim.ExpiresAt != nil {
		payload["expires_at"] = in.Claim.ExpiresAt.UTC().Format(sqliteTimeFormat)
	}
	if in.Claim.ReleasedAt != nil {
		payload["released_at"] = in.Claim.ReleasedAt.UTC().Format(sqliteTimeFormat)
	}
	if in.Reason != "" {
		payload["reason"] = in.Reason
	}
	if in.Type == "claim.violated" {
		payload["issue_uid"] = in.Claim.IssueUID
		payload["offending_event_uid"] = in.OffendingEventUID
		payload["offending_event_type"] = in.OffendingEventType
		payload["offending_origin_instance_uid"] = in.OffendingOriginInstanceUID
		payload["actor"] = in.Actor
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal claim event payload: %w", err)
	}
	eventUID, err := katauid.New()
	if err != nil {
		return Event{}, fmt.Errorf("generate event uid: %w", err)
	}
	now := time.Now().UTC()
	createdAt := now.Format(sqliteTimeFormat)
	clock, err := nextClaimEventHLC(ctx, tx, now)
	if err != nil {
		return Event{}, fmt.Errorf("next event hlc: %w", err)
	}
	projectUID, projectName, err := claimEventProjectIdentityTx(ctx, tx, in.ProjectID, in.ProjectName)
	if err != nil {
		return Event{}, err
	}
	issueUID := in.Claim.IssueUID
	contentHash, err := EventContentHash(EventHashInput{
		UID:               eventUID,
		OriginInstanceUID: d.instanceUID,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          &issueUID,
		Type:              in.Type,
		Actor:             in.Actor,
		HLCPhysicalMS:     clock.PhysicalMS,
		HLCCounter:        clock.Counter,
		CreatedAt:         createdAt,
		Payload:           json.RawMessage(b),
	})
	if err != nil {
		return Event{}, fmt.Errorf("content hash: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO events(
		  uid, origin_instance_uid, project_id, project_name, issue_id, issue_uid,
		  type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventUID, d.instanceUID, in.ProjectID, projectName, in.IssueID, issueUID,
		in.Type, in.Actor, string(b), clock.PhysicalMS, clock.Counter, contentHash, createdAt)
	if err != nil {
		return Event{}, fmt.Errorf("insert claim event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	e, err := scanEvent(tx.QueryRowContext(ctx, eventSelectByID, id))
	if err != nil {
		return Event{}, fmt.Errorf("read claim event: %w", err)
	}
	return e, nil
}

// ClaimViolationSummary is the display-ready shape for unresolved
// claim.violated audit events.
type ClaimViolationSummary struct {
	EventID                    int64     `json:"event_id"`
	EventUID                   string    `json:"event_uid"`
	IssueUID                   string    `json:"issue_uid"`
	IssueShortID               string    `json:"short_id,omitempty"`
	OffendingEventUID          string    `json:"offending_event_uid,omitempty"`
	OffendingEventType         string    `json:"offending_event_type,omitempty"`
	OffendingOriginInstanceUID string    `json:"offending_origin_instance_uid,omitempty"`
	Actor                      string    `json:"actor,omitempty"`
	Reason                     string    `json:"reason,omitempty"`
	At                         time.Time `json:"at"`
}

// UnresolvedClaimViolationsForIssue returns recent unresolved violations for a
// single issue plus the full unresolved count. A release, expiry, or force
// release resolves prior violations; without a release boundary, violations are
// considered after the first claim acquisition. If the issue has never had a
// claim acquisition, no violations are unresolved for display yet.
func (d *DB) UnresolvedClaimViolationsForIssue(
	ctx context.Context,
	projectID int64,
	issueUID string,
	limit int,
) ([]ClaimViolationSummary, int64, error) {
	if limit < 0 {
		limit = 0
	}
	cutoff, err := claimViolationCutoffForIssue(ctx, d, projectID, issueUID)
	if err != nil {
		return nil, 0, err
	}
	count, err := countUnresolvedClaimViolationsForIssue(ctx, d, projectID, issueUID, cutoff)
	if err != nil {
		return nil, 0, err
	}
	if limit == 0 {
		return []ClaimViolationSummary{}, count, nil
	}
	rows, err := d.QueryContext(ctx, claimViolationSelect+`
		 WHERE e.project_id = ?
		   AND e.issue_uid = ?
		   AND e.type = 'claim.violated'
		   AND e.id > ?
		 ORDER BY e.id DESC
		 LIMIT ?`, projectID, issueUID, cutoff, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list unresolved claim violations for issue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	violations, err := scanClaimViolationSummaries(rows)
	if err != nil {
		return nil, 0, err
	}
	return violations, count, nil
}

// UnresolvedClaimViolationsForProject returns the most recent unresolved
// violations across one project plus the full unresolved count.
func (d *DB) UnresolvedClaimViolationsForProject(
	ctx context.Context,
	projectID int64,
	limit int,
) ([]ClaimViolationSummary, int64, error) {
	if limit < 0 {
		limit = 0
	}
	count, err := countUnresolvedClaimViolationsForProject(ctx, d, projectID)
	if err != nil {
		return nil, 0, err
	}
	if limit == 0 {
		return []ClaimViolationSummary{}, count, nil
	}
	rows, err := d.QueryContext(ctx, claimViolationSelect+`
		 WHERE e.project_id = ?
		   AND e.type = 'claim.violated'
		   AND e.id > COALESCE(
				(SELECT MAX(r.id)
				   FROM events r
				  WHERE r.project_id = e.project_id
				    AND r.issue_uid = e.issue_uid
				    AND r.type IN ('claim.released', 'claim.expired', 'claim.force_released')),
				(SELECT MIN(a.id)
				   FROM events a
				  WHERE a.project_id = e.project_id
				    AND a.issue_uid = e.issue_uid
				    AND a.type = 'claim.acquired'),
				9223372036854775807)
		 ORDER BY e.id DESC
		 LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list unresolved claim violations for project: %w", err)
	}
	defer func() { _ = rows.Close() }()
	violations, err := scanClaimViolationSummaries(rows)
	if err != nil {
		return nil, 0, err
	}
	return violations, count, nil
}

const claimViolationSelect = `
	SELECT e.id, e.uid, COALESCE(e.issue_uid, json_extract(e.payload, '$.issue_uid'), ''),
	       COALESCE(i.short_id, ''),
	       COALESCE(json_extract(e.payload, '$.offending_event_uid'),
	                json_extract(e.payload, '$.event_uid'), ''),
	       COALESCE(json_extract(e.payload, '$.offending_event_type'),
	                json_extract(e.payload, '$.event_type'), ''),
	       COALESCE(json_extract(e.payload, '$.offending_origin_instance_uid'),
	                json_extract(e.payload, '$.origin_instance_uid'), ''),
	       COALESCE(json_extract(e.payload, '$.actor'), e.actor, ''),
	       COALESCE(json_extract(e.payload, '$.reason'), ''),
	       e.created_at
	  FROM events e
	  LEFT JOIN issues i ON i.project_id = e.project_id AND i.uid = e.issue_uid`

func claimViolationCutoffForIssue(ctx context.Context, q queryer, projectID int64, issueUID string) (int64, error) {
	var cutoff int64
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT MAX(id)
			   FROM events
			  WHERE project_id = ?
			    AND issue_uid = ?
			    AND type IN ('claim.released', 'claim.expired', 'claim.force_released')),
				(SELECT MIN(id)
			   FROM events
			  WHERE project_id = ?
			    AND issue_uid = ?
			    AND type = 'claim.acquired'),
			9223372036854775807)`,
		projectID, issueUID, projectID, issueUID).Scan(&cutoff)
	if err != nil {
		return 0, fmt.Errorf("claim violation cutoff: %w", err)
	}
	return cutoff, nil
}

func countUnresolvedClaimViolationsForIssue(
	ctx context.Context,
	q queryer,
	projectID int64,
	issueUID string,
	cutoff int64,
) (int64, error) {
	var count int64
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM events
		 WHERE project_id = ?
		   AND issue_uid = ?
		   AND type = 'claim.violated'
		   AND id > ?`, projectID, issueUID, cutoff).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count unresolved claim violations for issue: %w", err)
	}
	return count, nil
}

func countUnresolvedClaimViolationsForProject(ctx context.Context, q queryer, projectID int64) (int64, error) {
	var count int64
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM events e
		 WHERE e.project_id = ?
		   AND e.type = 'claim.violated'
		   AND e.id > COALESCE(
				(SELECT MAX(r.id)
				   FROM events r
				  WHERE r.project_id = e.project_id
				    AND r.issue_uid = e.issue_uid
				    AND r.type IN ('claim.released', 'claim.expired', 'claim.force_released')),
				(SELECT MIN(a.id)
				   FROM events a
				  WHERE a.project_id = e.project_id
				    AND a.issue_uid = e.issue_uid
				    AND a.type = 'claim.acquired'),
				9223372036854775807)`, projectID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count unresolved claim violations for project: %w", err)
	}
	return count, nil
}

func scanClaimViolationSummaries(rows *sql.Rows) ([]ClaimViolationSummary, error) {
	out := []ClaimViolationSummary{}
	for rows.Next() {
		var v ClaimViolationSummary
		if err := rows.Scan(
			&v.EventID, &v.EventUID, &v.IssueUID, &v.IssueShortID,
			&v.OffendingEventUID, &v.OffendingEventType, &v.OffendingOriginInstanceUID,
			&v.Actor, &v.Reason, &v.At,
		); err != nil {
			return nil, fmt.Errorf("scan claim violation: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claim violations: %w", err)
	}
	return out, nil
}

func resolveClaimIssueTx(ctx context.Context, tx claimStore, projectID int64, issueRef string) (Issue, string, error) {
	issueRef = strings.TrimSpace(issueRef)
	if issueRef == "" {
		return Issue{}, "", ErrNotFound
	}
	const q = `
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at, p.name
		  FROM issues i
		  JOIN projects p ON p.id = i.project_id
		 WHERE i.project_id = ?
		   AND (i.short_id = ? OR i.uid = ?)`
	var issue Issue
	var projectName string
	err := tx.QueryRowContext(ctx, q, projectID, issueRef, issueRef).Scan(
		&issue.ID, &issue.UID, &issue.ProjectID, &issue.ProjectUID, &issue.ShortID,
		&issue.Title, &issue.Body, &issue.Status, &issue.ClosedReason, &issue.Owner,
		&issue.Priority, &issue.Author, &issue.Metadata, &issue.Revision,
		&issue.RecurrenceID, &issue.OccurrenceKey, &issue.CreatedAt, &issue.UpdatedAt,
		&issue.ClosedAt, &issue.DeletedAt, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, "", ErrNotFound
	}
	if err != nil {
		return Issue{}, "", fmt.Errorf("resolve claim issue: %w", err)
	}
	return issue, projectName, nil
}

const issueClaimSelect = `SELECT id, claim_uid, project_id, issue_id, issue_uid,
       holder, holder_instance_uid, client_kind, purpose, claim_kind,
       acquired_at, expires_at, released_at, release_reason, revision, updated_at
  FROM issue_claims`

func liveClaimForIssueTx(ctx context.Context, tx claimStore, issueUID string) (IssueClaim, error) {
	return scanIssueClaim(tx.QueryRowContext(ctx,
		issueClaimSelect+` WHERE issue_uid = ? AND released_at IS NULL`, issueUID))
}

func claimByIDTx(ctx context.Context, tx claimStore, id int64) (IssueClaim, error) {
	return scanIssueClaim(tx.QueryRowContext(ctx, issueClaimSelect+` WHERE id = ?`, id))
}

const pendingClaimRequestSelect = `SELECT id, request_uid, project_id, issue_id, issue_uid,
       holder, holder_instance_uid, client_kind, claim_kind, ttl_seconds, purpose, requested_at,
       last_attempt_at, last_error, rejected_at, resolved_at
  FROM pending_claim_requests`

func pendingClaimRequestByIDTx(ctx context.Context, tx claimStore, id int64) (PendingClaimRequest, error) {
	return scanPendingClaimRequest(tx.QueryRowContext(ctx, pendingClaimRequestSelect+` WHERE id = ?`, id))
}

func pendingClaimRequestByUIDTx(ctx context.Context, tx claimStore, requestUID string) (PendingClaimRequest, error) {
	return scanPendingClaimRequest(tx.QueryRowContext(ctx, pendingClaimRequestSelect+` WHERE request_uid = ?`, requestUID))
}

func activePendingClaimRequestForPrincipalTx(
	ctx context.Context,
	tx claimStore,
	issueUID string,
	principal ClaimPrincipal,
) (PendingClaimRequest, error) {
	pending, err := scanPendingClaimRequest(tx.QueryRowContext(ctx, pendingClaimRequestSelect+`
		 WHERE issue_uid = ?
		   AND holder_instance_uid = ?
		   AND holder = ?
		   AND client_kind = ?
		   AND rejected_at IS NULL AND resolved_at IS NULL
		 ORDER BY requested_at ASC, id ASC
		 LIMIT 1`, issueUID, principal.HolderInstanceUID, principal.Holder, principal.ClientKind))
	if err == nil || !errors.Is(err, ErrNotFound) || principal.HolderInstanceUID == "" {
		return pending, err
	}
	// Older JSONL imports may carry active pending rows from before
	// holder_instance_uid was persisted. Treat those as holder/client scoped.
	return scanPendingClaimRequest(tx.QueryRowContext(ctx, pendingClaimRequestSelect+`
		 WHERE issue_uid = ?
		   AND holder_instance_uid = ''
		   AND holder = ?
		   AND client_kind = ?
		   AND rejected_at IS NULL AND resolved_at IS NULL
		 ORDER BY requested_at ASC, id ASC
		 LIMIT 1`, issueUID, principal.Holder, principal.ClientKind))
}

func latestClaimUpdatedAtForIssueTx(ctx context.Context, tx claimStore, issueUID string) (time.Time, bool, error) {
	var updatedAt time.Time
	err := tx.QueryRowContext(ctx, `
		SELECT updated_at
		  FROM issue_claims
		 WHERE issue_uid = ?
		 ORDER BY updated_at DESC, id DESC
		 LIMIT 1`, issueUID).Scan(&updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("latest issue claim updated_at: %w", err)
	}
	return updatedAt, true, nil
}

func expiredTimedClaimsTx(ctx context.Context, tx claimStore, now time.Time, limit int) ([]IssueClaim, error) {
	q := issueClaimSelect + `
		 WHERE released_at IS NULL
		   AND claim_kind = 'timed'
		   AND expires_at <= ?
		 ORDER BY expires_at ASC, id ASC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := tx.QueryContext(ctx, q, now.UTC().Format(sqliteTimeFormat))
	if err != nil {
		return nil, fmt.Errorf("list expired timed claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueClaim
	for rows.Next() {
		claim, err := scanIssueClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired timed claims rows: %w", err)
	}
	return out, nil
}

func expiredTimedClaimsForProjectTx(
	ctx context.Context,
	tx claimStore,
	projectID int64,
	now time.Time,
	limit int,
) ([]IssueClaim, error) {
	q := issueClaimSelect + `
		 WHERE project_id = ?
		   AND released_at IS NULL
		   AND claim_kind = 'timed'
		   AND expires_at <= ?
		 ORDER BY expires_at ASC, id ASC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := tx.QueryContext(ctx, q, projectID, now.UTC().Format(sqliteTimeFormat))
	if err != nil {
		return nil, fmt.Errorf("list expired timed claims for project: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueClaim
	for rows.Next() {
		claim, err := scanIssueClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired timed claims for project rows: %w", err)
	}
	return out, nil
}

func nextClaimEventHLC(ctx context.Context, tx claimStore, now time.Time) (eventHLCTimestamp, error) {
	var last eventHLCTimestamp
	err := tx.QueryRowContext(ctx, `
		SELECT hlc_physical_ms, hlc_counter
		  FROM events
		 ORDER BY hlc_physical_ms DESC, hlc_counter DESC
		 LIMIT 1`).Scan(&last.PhysicalMS, &last.Counter)
	if errors.Is(err, sql.ErrNoRows) {
		return nextEventHLCValue(eventHLCTimestamp{}, now), nil
	}
	if err != nil {
		return eventHLCTimestamp{}, err
	}
	return nextEventHLCValue(last, now), nil
}

func claimEventProjectIdentityTx(ctx context.Context, tx claimStore, projectID int64, projectName string) (string, string, error) {
	var storedUID, storedName string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid, name FROM projects WHERE id = ?`, projectID).
		Scan(&storedUID, &storedName); err != nil {
		return "", "", fmt.Errorf("resolve claim event project identity: %w", err)
	}
	if projectName == "" {
		projectName = storedName
	}
	return storedUID, projectName, nil
}

func scanIssueClaim(r rowScanner) (IssueClaim, error) {
	var (
		c             IssueClaim
		expiresAt     sql.NullTime
		releasedAt    sql.NullTime
		releaseReason sql.NullString
	)
	err := r.Scan(&c.ID, &c.ClaimUID, &c.ProjectID, &c.IssueID, &c.IssueUID,
		&c.Holder, &c.HolderInstanceUID, &c.ClientKind, &c.Purpose, &c.ClaimKind,
		&c.AcquiredAt, &expiresAt, &releasedAt, &releaseReason, &c.Revision, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IssueClaim{}, ErrNotFound
	}
	if err != nil {
		return IssueClaim{}, fmt.Errorf("scan issue claim: %w", err)
	}
	if expiresAt.Valid {
		c.ExpiresAt = &expiresAt.Time
	}
	if releasedAt.Valid {
		c.ReleasedAt = &releasedAt.Time
	}
	if releaseReason.Valid {
		c.ReleaseReason = &releaseReason.String
	}
	return c, nil
}

func scanPendingClaimRequest(r rowScanner) (PendingClaimRequest, error) {
	var (
		p             PendingClaimRequest
		ttlSeconds    sql.NullInt64
		lastAttemptAt sql.NullTime
		lastError     sql.NullString
		rejectedAt    sql.NullTime
		resolvedAt    sql.NullTime
	)
	err := r.Scan(&p.ID, &p.RequestUID, &p.ProjectID, &p.IssueID, &p.IssueUID,
		&p.Holder, &p.HolderInstanceUID, &p.ClientKind, &p.ClaimKind, &ttlSeconds,
		&p.Purpose, &p.RequestedAt, &lastAttemptAt, &lastError, &rejectedAt, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingClaimRequest{}, ErrNotFound
	}
	if err != nil {
		return PendingClaimRequest{}, fmt.Errorf("scan pending claim request: %w", err)
	}
	if ttlSeconds.Valid {
		p.TTLSeconds = &ttlSeconds.Int64
	}
	if lastAttemptAt.Valid {
		p.LastAttemptAt = &lastAttemptAt.Time
	}
	if lastError.Valid {
		p.LastError = &lastError.String
	}
	if rejectedAt.Valid {
		p.RejectedAt = &rejectedAt.Time
	}
	if resolvedAt.Valid {
		p.ResolvedAt = &resolvedAt.Time
	}
	return p, nil
}

func validateClaimPrincipal(p ClaimPrincipal) error {
	if !katauid.Valid(p.HolderInstanceUID) {
		return fmt.Errorf("%w: invalid holder instance uid", ErrClaimValidation)
	}
	if strings.TrimSpace(p.Holder) == "" {
		return fmt.Errorf("%w: holder is required", ErrClaimValidation)
	}
	return nil
}

func validateTimedClaimTTL(ttl time.Duration) error {
	if ttl < minTimedClaimTTL || ttl > maxTimedClaimTTL {
		return fmt.Errorf("%w: timed claim ttl must be between 60s and 24h", ErrClaimValidation)
	}
	return nil
}

func claimNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func sameClaimPrincipal(c IssueClaim, p ClaimPrincipal) bool {
	return c.Holder == p.Holder &&
		c.HolderInstanceUID == p.HolderInstanceUID &&
		c.ClientKind == p.ClientKind
}

func sameClaimGateHolder(c IssueClaim, p ClaimPrincipal) bool {
	return c.Holder == p.Holder &&
		c.HolderInstanceUID == p.HolderInstanceUID
}

func claimExpiredThisPass(c IssueClaim, p ClaimPrincipal, now time.Time) bool {
	return sameClaimPrincipal(c, p) && claimTimedExpiredThisPass(c, now)
}

func claimTimedExpiredThisPass(c IssueClaim, now time.Time) bool {
	return c.ID != 0 &&
		c.ClaimKind == "timed" &&
		c.ExpiresAt != nil &&
		!c.ExpiresAt.After(now)
}

func principalForClaim(c IssueClaim) ClaimPrincipal {
	return ClaimPrincipal{
		HolderInstanceUID: c.HolderInstanceUID,
		Holder:            c.Holder,
		ClientKind:        c.ClientKind,
	}
}

func resultForClaim(c IssueClaim, granted bool) LeaseResult {
	return LeaseResult{
		Granted: granted,
		Holder:  principalForClaim(c),
		Claim:   &c,
	}
}

func resultForClaimWithEvents(c IssueClaim, granted bool, events []Event) LeaseResult {
	out := resultForClaim(c, granted)
	out.Events = events
	return out
}
