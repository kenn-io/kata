package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

const pendingClaimSelect = `SELECT id, request_uid, project_id, issue_id, issue_uid,
       holder, holder_instance_uid, client_kind, claim_kind, ttl_seconds,
       purpose, requested_at, last_attempt_at, last_error, rejected_at, resolved_at
  FROM pending_claim_requests`

// EnqueuePendingClaim stores or returns an unresolved offline claim request.
func (s *Store) EnqueuePendingClaim(ctx context.Context, input db.PendingClaimParams) (db.PendingClaimRequest, error) {
	now := db.ClaimNow(input.Now)
	if err := db.ValidateClaimPrincipal(input.Principal); err != nil {
		return db.PendingClaimRequest{}, err
	}
	if input.ClaimKind != "hard" && input.ClaimKind != "timed" {
		return db.PendingClaimRequest{}, fmt.Errorf("%w: claim_kind must be hard or timed", db.ErrClaimValidation)
	}
	var ttlSeconds any
	if input.ClaimKind == "timed" {
		if err := db.ValidateTimedClaimTTL(input.TTL); err != nil {
			return db.PendingClaimRequest{}, err
		}
		ttlSeconds = int64(input.TTL / time.Second)
	}

	var output db.PendingClaimRequest
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.PendingClaimRequest{}
		issue, _, err := resolveClaimIssueTx(ctx, tx, input.ProjectID, input.IssueRef, true)
		if err != nil {
			return err
		}
		existing, err := activePendingClaimRequestTx(ctx, tx, issue.UID, input.Principal, true)
		if err == nil {
			output = existing
			return nil
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		requestUID, err := katauid.New()
		if err != nil {
			return fmt.Errorf("generate pending claim request uid: %w", err)
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO pending_claim_requests(
  request_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
  client_kind, claim_kind, ttl_seconds, purpose, requested_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (issue_uid, holder_instance_uid, holder, client_kind)
  WHERE rejected_at IS NULL AND resolved_at IS NULL
DO NOTHING RETURNING id`, requestUID, issue.ProjectID, issue.ID, issue.UID,
			input.Principal.Holder, input.Principal.HolderInstanceUID, input.Principal.ClientKind,
			input.ClaimKind, ttlSeconds, input.Purpose, formatStoredTime(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			output, err = activePendingClaimRequestTx(ctx, tx, issue.UID, input.Principal, true)
			return err
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		output, err = pendingClaimRequestByIDTx(ctx, tx, id, false)
		return err
	})
	return output, err
}

// ResolvePendingClaim marks a pending request resolved and caches its claim.
func (s *Store) ResolvePendingClaim(ctx context.Context, requestUID string, claim db.IssueClaim) error {
	requestUID = strings.TrimSpace(requestUID)
	if requestUID == "" {
		return db.ErrNotFound
	}
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		pending, err := pendingClaimRequestByUIDTx(ctx, tx, requestUID, true)
		if err != nil {
			return err
		}
		if pending.RejectedAt != nil {
			return fmt.Errorf("%w: pending claim rejected", db.ErrClaimValidation)
		}
		if pending.ResolvedAt != nil {
			return nil
		}
		issue, _, err := resolveClaimIssueTx(ctx, tx, pending.ProjectID, pending.IssueUID, true)
		if err != nil {
			return err
		}
		if err := db.ValidatePendingClaimResolution(pending, issue, claim); err != nil {
			return err
		}
		now := db.ClaimNow(claim.UpdatedAt)
		claim.ProjectID = issue.ProjectID
		claim.IssueID = issue.ID
		claim.IssueUID = issue.UID
		if claim.AcquiredAt.IsZero() {
			claim.AcquiredAt = now
		}
		if claim.UpdatedAt.IsZero() {
			claim.UpdatedAt = now
		}
		if err := s.applyClaimStatusTx(ctx, tx, issue.ProjectID, issue.UID, db.ClaimStatus{
			Held: true, Holder: db.PrincipalForClaim(claim), Claim: &claim, HubNow: now,
		}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE pending_claim_requests
SET resolved_at=$1, last_error=NULL WHERE request_uid=$2`, formatStoredTime(now), requestUID)
		return mapSQLError(err, nil)
	})
}

// RejectPendingClaim marks a pending request terminally rejected.
func (s *Store) RejectPendingClaim(ctx context.Context, requestUID, reason string, nowInput time.Time) error {
	requestUID = strings.TrimSpace(requestUID)
	if requestUID == "" {
		return db.ErrNotFound
	}
	stamp := formatStoredTime(db.ClaimNow(nowInput))
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE pending_claim_requests
SET rejected_at=$1, last_attempt_at=$1, last_error=$2
WHERE request_uid=$3 AND rejected_at IS NULL AND resolved_at IS NULL`, stamp, reason, requestUID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			_, err = pendingClaimRequestByUIDTx(ctx, tx, requestUID, false)
		}
		return err
	})
}

// ListPendingClaimRequests returns unresolved requests for one project.
func (s *Store) ListPendingClaimRequests(ctx context.Context, projectID int64, limit int) ([]db.PendingClaimRequest, error) {
	query := pendingClaimSelect + ` WHERE project_id=$1 AND rejected_at IS NULL AND resolved_at IS NULL
ORDER BY requested_at ASC, id ASC`
	args := []any{projectID}
	if limit > 0 {
		args = append(args, limit)
		query += ` LIMIT $2`
	}
	return s.listPendingClaimRequests(ctx, query, args...)
}

// ListPendingClaimRequestsForIssue returns unresolved requests for one issue.
func (s *Store) ListPendingClaimRequestsForIssue(
	ctx context.Context,
	projectID int64,
	issueUID string,
	limit int,
) ([]db.PendingClaimRequest, error) {
	query := pendingClaimSelect + ` WHERE project_id=$1 AND issue_uid=$2
AND rejected_at IS NULL AND resolved_at IS NULL ORDER BY requested_at DESC, id DESC`
	args := []any{projectID, issueUID}
	if limit > 0 {
		args = append(args, limit)
		query += ` LIMIT $3`
	}
	return s.listPendingClaimRequests(ctx, query, args...)
}

func (s *Store) listPendingClaimRequests(ctx context.Context, query string, args ...any) ([]db.PendingClaimRequest, error) {
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := []db.PendingClaimRequest{}
	for rows.Next() {
		pending, err := scanPendingClaimRequest(rows)
		if err != nil {
			return nil, err
		}
		output = append(output, pending)
	}
	return output, mapSQLError(rows.Err(), nil)
}

// MarkPendingClaimAttempt records a retry attempt and its latest error.
func (s *Store) MarkPendingClaimAttempt(ctx context.Context, requestUID, lastError string, nowInput time.Time) error {
	requestUID = strings.TrimSpace(requestUID)
	if requestUID == "" {
		return db.ErrNotFound
	}
	stamp := formatStoredTime(db.ClaimNow(nowInput))
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE pending_claim_requests
SET last_attempt_at=$1, last_error=$2
WHERE request_uid=$3 AND rejected_at IS NULL AND resolved_at IS NULL`, stamp, lastError, requestUID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			_, err = pendingClaimRequestByUIDTx(ctx, tx, requestUID, false)
		}
		return err
	})
}

// ClaimStatusRefreshError returns the latest status-refresh failure marker.
func (s *Store) ClaimStatusRefreshError(
	ctx context.Context,
	projectID int64,
	issueUID string,
) (db.ClaimStatusRefreshError, error) {
	key := db.ClaimStatusRefreshErrorKey(projectID, issueUID)
	if key == "" {
		return db.ClaimStatusRefreshError{}, db.ErrNotFound
	}
	var raw string
	err := s.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ClaimStatusRefreshError{}, db.ErrNotFound
	}
	if err != nil {
		return db.ClaimStatusRefreshError{}, mapSQLError(err, nil)
	}
	var stored struct {
		StatusCode    int       `json:"status_code"`
		LastAttemptAt time.Time `json:"last_attempt_at"`
		LastError     string    `json:"last_error"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return db.ClaimStatusRefreshError{}, fmt.Errorf("decode claim status refresh error: %w", err)
	}
	return db.ClaimStatusRefreshError{
		ProjectID: projectID, IssueUID: strings.TrimSpace(issueUID), StatusCode: stored.StatusCode,
		LastAttemptAt: stored.LastAttemptAt, LastError: stored.LastError,
	}, nil
}

// MarkClaimStatusRefreshError records a transient status-refresh failure.
func (s *Store) MarkClaimStatusRefreshError(
	ctx context.Context,
	projectID int64,
	issueUID string,
	statusCode int,
	lastError string,
	nowInput time.Time,
) error {
	key := db.ClaimStatusRefreshErrorKey(projectID, issueUID)
	if key == "" {
		return db.ErrNotFound
	}
	stored := struct {
		StatusCode    int       `json:"status_code"`
		LastAttemptAt time.Time `json:"last_attempt_at"`
		LastError     string    `json:"last_error"`
	}{statusCode, db.ClaimNow(nowInput), lastError}
	raw, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode claim status refresh error: %w", err)
	}
	_, err = s.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, string(raw))
	return mapSQLError(err, nil)
}

// ClearClaimStatusRefreshError removes a status-refresh failure marker.
func (s *Store) ClearClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string) error {
	key := db.ClaimStatusRefreshErrorKey(projectID, issueUID)
	if key == "" {
		return db.ErrNotFound
	}
	_, err := s.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, key)
	return mapSQLError(err, nil)
}

// UpsertClaimCache stores a live claim as the cached authoritative status.
func (s *Store) UpsertClaimCache(ctx context.Context, claim db.IssueClaim) error {
	return s.ApplyClaimStatus(ctx, claim.ProjectID, claim.IssueUID, db.ClaimStatus{
		Held: true, Holder: db.PrincipalForClaim(claim), Claim: &claim, HubNow: claim.UpdatedAt,
	})
}

// ApplyClaimStatus reconciles local cache with an authoritative claim status.
func (s *Store) ApplyClaimStatus(ctx context.Context, projectID int64, issueUID string, status db.ClaimStatus) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		return s.applyClaimStatusTx(ctx, tx, projectID, issueUID, status)
	})
}

// CheckClaimGate verifies whether a holder may mutate an issue with cached claim state.
func (s *Store) CheckClaimGate(ctx context.Context, input db.ClaimGateParams) error {
	now := db.ClaimNow(input.Now)
	if err := db.ValidateClaimPrincipal(input.Principal); err != nil {
		return err
	}
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issueRef := strings.TrimSpace(input.IssueRef)
		if issueRef == "" {
			return db.ErrNotFound
		}
		issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+
			` WHERE i.project_id=$1 AND (i.short_id=$2 OR i.uid=$2) FOR UPDATE OF i`,
			input.ProjectID, issueRef))
		if err != nil {
			return err
		}
		live, err := claimForIssueTx(ctx, tx, issue.UID, true)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if claimTimedExpired(live, now) {
			return nil
		}
		if !db.SameClaimGateHolder(live, input.Principal) {
			return db.ErrClaimDenied
		}
		return nil
	})
}

func (s *Store) applyClaimStatusTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	issueUID string,
	status db.ClaimStatus,
) error {
	issue, _, err := resolveClaimIssueTx(ctx, tx, projectID, issueUID, true)
	if err != nil {
		return err
	}
	now := db.ClaimNow(status.HubNow)
	if status.Held && status.Claim != nil {
		if err := db.ValidateStatusClaimIssueIdentity(issue, *status.Claim); err != nil {
			return err
		}
	}
	latestUpdatedAt, hasLatest, err := latestClaimUpdatedAtTx(ctx, tx, issue.UID)
	if err != nil {
		return err
	}
	if hasLatest && !status.HubNow.IsZero() && status.HubNow.Before(latestUpdatedAt) {
		return assertSingleLiveClaimTx(ctx, tx, issue.UID)
	}
	live, liveErr := claimForIssueTx(ctx, tx, issue.UID, true)
	if liveErr != nil && !errors.Is(liveErr, db.ErrNotFound) {
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
	claim, err := db.NormalizeCachedClaim(status, issue, now)
	if err != nil {
		return err
	}
	if liveErr == nil && live.ClaimUID == claim.ClaimUID {
		if db.StaleSameClaimRefresh(live, claim) {
			return assertSingleLiveClaimTx(ctx, tx, issue.UID)
		}
		if err := updateCachedClaimTx(ctx, tx, live.ID, claim); err != nil {
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

func activePendingClaimRequestTx(
	ctx context.Context,
	queryer claimQueryer,
	issueUID string,
	principal db.ClaimPrincipal,
	lock bool,
) (db.PendingClaimRequest, error) {
	query := pendingClaimSelect + ` WHERE issue_uid=$1 AND holder_instance_uid=$2
AND holder=$3 AND client_kind=$4 AND rejected_at IS NULL AND resolved_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	pending, err := scanPendingClaimRequest(queryer.QueryRowContext(ctx, query, issueUID,
		principal.HolderInstanceUID, principal.Holder, principal.ClientKind))
	if err == nil || !errors.Is(err, db.ErrNotFound) || principal.HolderInstanceUID == "" {
		return pending, err
	}
	query = pendingClaimSelect + ` WHERE issue_uid=$1 AND holder_instance_uid=''
AND holder=$2 AND client_kind=$3 AND rejected_at IS NULL AND resolved_at IS NULL
ORDER BY requested_at ASC, id ASC LIMIT 1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanPendingClaimRequest(queryer.QueryRowContext(ctx, query, issueUID,
		principal.Holder, principal.ClientKind))
}

func pendingClaimRequestByIDTx(
	ctx context.Context,
	queryer claimQueryer,
	id int64,
	lock bool,
) (db.PendingClaimRequest, error) {
	query := pendingClaimSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanPendingClaimRequest(queryer.QueryRowContext(ctx, query, id))
}

func pendingClaimRequestByUIDTx(
	ctx context.Context,
	queryer claimQueryer,
	requestUID string,
	lock bool,
) (db.PendingClaimRequest, error) {
	query := pendingClaimSelect + ` WHERE request_uid=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanPendingClaimRequest(queryer.QueryRowContext(ctx, query, requestUID))
}

func scanPendingClaimRequest(row rowScanner) (db.PendingClaimRequest, error) {
	var pending db.PendingClaimRequest
	var ttlSeconds sql.NullInt64
	var requestedAt string
	var lastAttemptAt, lastError, rejectedAt, resolvedAt sql.NullString
	err := row.Scan(&pending.ID, &pending.RequestUID, &pending.ProjectID, &pending.IssueID,
		&pending.IssueUID, &pending.Holder, &pending.HolderInstanceUID, &pending.ClientKind,
		&pending.ClaimKind, &ttlSeconds, &pending.Purpose, &requestedAt, &lastAttemptAt,
		&lastError, &rejectedAt, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PendingClaimRequest{}, db.ErrNotFound
	}
	if err != nil {
		return db.PendingClaimRequest{}, mapSQLError(err, nil)
	}
	pending.RequestedAt, err = parseStoredTime(requestedAt)
	if err != nil {
		return db.PendingClaimRequest{}, fmt.Errorf("parse pending requested_at: %w", err)
	}
	if ttlSeconds.Valid {
		pending.TTLSeconds = &ttlSeconds.Int64
	}
	if lastError.Valid {
		pending.LastError = &lastError.String
	}
	if pending.LastAttemptAt, err = parseNullableStoredTime(lastAttemptAt); err != nil {
		return db.PendingClaimRequest{}, fmt.Errorf("parse pending last_attempt_at: %w", err)
	}
	if pending.RejectedAt, err = parseNullableStoredTime(rejectedAt); err != nil {
		return db.PendingClaimRequest{}, fmt.Errorf("parse pending rejected_at: %w", err)
	}
	if pending.ResolvedAt, err = parseNullableStoredTime(resolvedAt); err != nil {
		return db.PendingClaimRequest{}, fmt.Errorf("parse pending resolved_at: %w", err)
	}
	return pending, nil
}

func latestClaimUpdatedAtTx(ctx context.Context, tx *sql.Tx, issueUID string) (time.Time, bool, error) {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT updated_at FROM issue_claims
WHERE issue_uid=$1 ORDER BY updated_at DESC, id DESC LIMIT 1`, issueUID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, mapSQLError(err, nil)
	}
	parsed, err := parseStoredTime(stored)
	return parsed, err == nil, err
}

func insertCachedClaimTx(ctx context.Context, tx *sql.Tx, claim db.IssueClaim) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO issue_claims(
  claim_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
  client_kind, purpose, claim_kind, acquired_at, expires_at, revision, updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, claim.ClaimUID,
		claim.ProjectID, claim.IssueID, claim.IssueUID, claim.Holder, claim.HolderInstanceUID,
		claim.ClientKind, claim.Purpose, claim.ClaimKind, formatStoredTime(claim.AcquiredAt),
		nullableStoredTime(claim.ExpiresAt), claim.Revision, formatStoredTime(claim.UpdatedAt))
	return mapSQLError(err, nil)
}

func updateCachedClaimTx(ctx context.Context, tx *sql.Tx, id int64, claim db.IssueClaim) error {
	_, err := tx.ExecContext(ctx, `UPDATE issue_claims SET holder=$1, holder_instance_uid=$2,
client_kind=$3, purpose=$4, claim_kind=$5, acquired_at=$6, expires_at=$7,
revision=$8, updated_at=$9 WHERE id=$10 AND released_at IS NULL`, claim.Holder,
		claim.HolderInstanceUID, claim.ClientKind, claim.Purpose, claim.ClaimKind,
		formatStoredTime(claim.AcquiredAt), nullableStoredTime(claim.ExpiresAt), claim.Revision,
		formatStoredTime(claim.UpdatedAt), id)
	return mapSQLError(err, nil)
}

func releaseCachedClaimTx(ctx context.Context, tx *sql.Tx, id int64, reason string, now time.Time) error {
	stamp := formatStoredTime(now)
	_, err := tx.ExecContext(ctx, `UPDATE issue_claims SET released_at=$1, release_reason=$2,
revision=revision+1, updated_at=$1 WHERE id=$3 AND released_at IS NULL`, stamp, reason, id)
	return mapSQLError(err, nil)
}

func assertSingleLiveClaimTx(ctx context.Context, tx *sql.Tx, issueUID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_claims
WHERE issue_uid=$1 AND released_at IS NULL`, issueUID).Scan(&count); err != nil {
		return mapSQLError(err, nil)
	}
	if count > 1 {
		return fmt.Errorf("%w: multiple live claims for issue", db.ErrClaimValidation)
	}
	return nil
}
