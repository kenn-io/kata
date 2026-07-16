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

const claimSelect = `SELECT id, claim_uid, project_id, issue_id, issue_uid,
       holder, holder_instance_uid, client_kind, purpose, claim_kind,
       acquired_at, expires_at, released_at, release_reason, revision, updated_at
  FROM issue_claims`

// AcquireClaim creates a live issue claim or reports the current holder.
func (s *Store) AcquireClaim(ctx context.Context, input db.AcquireClaimParams) (db.LeaseResult, error) {
	now := db.ClaimNow(input.Now)
	if err := db.ValidateClaimPrincipal(input.Principal); err != nil {
		return db.LeaseResult{}, err
	}
	if input.ClaimKind != "hard" && input.ClaimKind != "timed" {
		return db.LeaseResult{}, fmt.Errorf("%w: claim_kind must be hard or timed", db.ErrClaimValidation)
	}
	var expiresAt *time.Time
	if input.ClaimKind == "timed" {
		if err := db.ValidateTimedClaimTTL(input.TTL); err != nil {
			return db.LeaseResult{}, err
		}
		expires := now.Add(input.TTL)
		expiresAt = &expires
	}

	var output db.LeaseResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.LeaseResult{}
		issue, project, err := resolveClaimIssueTx(ctx, tx, input.ProjectID, input.IssueRef, true)
		if err != nil {
			return err
		}
		expired, err := s.expireTimedClaimsTx(ctx, tx, 0, now, 0)
		if err != nil {
			return err
		}
		output.Events = expired
		live, err := claimForIssueTx(ctx, tx, issue.UID, true)
		if err == nil {
			output = db.LeaseResultFor(live, db.SameClaimPrincipal(live, input.Principal), expired)
			if output.Granted {
				return nil
			}
			return db.ErrClaimDenied
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		claimUID, err := katauid.New()
		if err != nil {
			return fmt.Errorf("generate claim uid: %w", err)
		}
		var claimID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO issue_claims(
  claim_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
  client_kind, purpose, claim_kind, acquired_at, expires_at, updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$10) RETURNING id`,
			claimUID, issue.ProjectID, issue.ID, issue.UID, input.Principal.Holder,
			input.Principal.HolderInstanceUID, input.Principal.ClientKind, input.Purpose,
			input.ClaimKind, formatStoredTime(now), nullableStoredTime(expiresAt),
		).Scan(&claimID)
		if err != nil {
			return mapSQLError(err, map[string]error{"uniq_issue_claims_live_issue": db.ErrClaimDenied})
		}
		claim, err := claimByIDTx(ctx, tx, claimID, false)
		if err != nil {
			return err
		}
		event, err := s.insertClaimEventTx(ctx, tx, project, issue, "claim.acquired",
			input.Principal.Holder, claim, "")
		if err != nil {
			return err
		}
		output = db.LeaseResultFor(claim, true, expired)
		output.Event = &event
		return nil
	})
	return output, err
}

// RenewClaim extends a timed claim held by the exact principal.
func (s *Store) RenewClaim(ctx context.Context, input db.RenewClaimParams) (db.LeaseResult, error) {
	return s.renewClaim(ctx, input, s.withSerializableTx)
}

func (s *Store) renewClaim(
	ctx context.Context,
	input db.RenewClaimParams,
	runTx func(context.Context, transactionFunc) error,
) (db.LeaseResult, error) {
	if err := db.ValidateClaimPrincipal(input.Principal); err != nil {
		return db.LeaseResult{}, err
	}
	if err := db.ValidateTimedClaimTTL(input.TTL); err != nil {
		return db.LeaseResult{}, err
	}
	now := db.ClaimNow(input.Now)
	var output db.LeaseResult
	var expired bool
	err := runTx(ctx, func(tx *sql.Tx) error {
		output = db.LeaseResult{}
		expired = false
		issue, _, err := resolveClaimIssueTx(ctx, tx, input.ProjectID, input.IssueRef, true)
		if err != nil {
			return err
		}
		liveBefore, err := claimForIssueTx(ctx, tx, issue.UID, true)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		expiredEvents, err := s.expireTimedClaimsTx(ctx, tx, 0, now, 0)
		if err != nil {
			return err
		}
		output.Events = expiredEvents
		claim, err := claimForIssueTx(ctx, tx, issue.UID, true)
		if errors.Is(err, db.ErrNotFound) {
			if db.SameClaimPrincipal(liveBefore, input.Principal) && claimTimedExpired(liveBefore, now) {
				released, lookupErr := claimByIDTx(ctx, tx, liveBefore.ID, false)
				if lookupErr != nil {
					return lookupErr
				}
				output = db.LeaseResultFor(released, false, expiredEvents)
				expired = true
				return nil
			}
			return db.ErrClaimNotHeld
		}
		if err != nil {
			return err
		}
		output = db.LeaseResultFor(claim, db.SameClaimPrincipal(claim, input.Principal), expiredEvents)
		if !output.Granted {
			return db.ErrClaimNotHeld
		}
		if claim.ClaimKind != "timed" {
			return fmt.Errorf("%w: hard claims cannot be renewed", db.ErrClaimValidation)
		}
		expiresAt := now.Add(input.TTL)
		if _, err := tx.ExecContext(ctx, `UPDATE issue_claims
SET expires_at=$1, revision=revision+1, updated_at=$2 WHERE id=$3`,
			formatStoredTime(expiresAt), formatStoredTime(now), claim.ID); err != nil {
			return mapSQLError(err, nil)
		}
		renewed, err := claimByIDTx(ctx, tx, claim.ID, false)
		if err != nil {
			return err
		}
		output = db.LeaseResultFor(renewed, true, expiredEvents)
		return nil
	})
	if err == nil && expired {
		err = db.ErrClaimExpired
	}
	return output, err
}

// ReleaseClaim releases a live claim held by the exact principal.
func (s *Store) ReleaseClaim(ctx context.Context, input db.ReleaseClaimParams) (db.LeaseResult, error) {
	if err := db.ValidateClaimPrincipal(input.Principal); err != nil {
		return db.LeaseResult{}, err
	}
	return s.releaseClaim(ctx, input.ProjectID, input.IssueRef, input.Principal,
		input.Principal.Holder, input.Reason, input.Now, false, s.withSerializableTx)
}

// ForceReleaseClaim releases a live claim regardless of its holder.
func (s *Store) ForceReleaseClaim(ctx context.Context, input db.ForceReleaseClaimParams) (db.LeaseResult, error) {
	if strings.TrimSpace(input.Actor) == "" {
		return db.LeaseResult{}, fmt.Errorf("%w: actor is required", db.ErrClaimValidation)
	}
	return s.releaseClaim(ctx, input.ProjectID, input.IssueRef, db.ClaimPrincipal{},
		input.Actor, input.Reason, input.Now, true, s.withSerializableTx)
}

func (s *Store) releaseClaim(
	ctx context.Context,
	projectID int64,
	issueRef string,
	principal db.ClaimPrincipal,
	actor string,
	reason string,
	nowInput time.Time,
	force bool,
	runTx func(context.Context, transactionFunc) error,
) (db.LeaseResult, error) {
	now := db.ClaimNow(nowInput)
	var output db.LeaseResult
	var expired bool
	err := runTx(ctx, func(tx *sql.Tx) error {
		output = db.LeaseResult{}
		expired = false
		issue, project, err := resolveClaimIssueTx(ctx, tx, projectID, issueRef, true)
		if err != nil {
			return err
		}
		liveBefore, err := claimForIssueTx(ctx, tx, issue.UID, true)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		expiredEvents, err := s.expireTimedClaimsTx(ctx, tx, 0, now, 0)
		if err != nil {
			return err
		}
		output.Events = expiredEvents
		claim, err := claimForIssueTx(ctx, tx, issue.UID, true)
		if errors.Is(err, db.ErrNotFound) {
			claimExpired := claimTimedExpired(liveBefore, now) &&
				(force || db.SameClaimPrincipal(liveBefore, principal))
			if claimExpired {
				released, lookupErr := claimByIDTx(ctx, tx, liveBefore.ID, false)
				if lookupErr != nil {
					return lookupErr
				}
				output = db.LeaseResultFor(released, false, expiredEvents)
				expired = true
				return nil
			}
			return db.ErrClaimNotHeld
		}
		if err != nil {
			return err
		}
		granted := force || db.SameClaimPrincipal(claim, principal)
		output = db.LeaseResultFor(claim, granted, expiredEvents)
		if !granted {
			return db.ErrClaimNotHeld
		}
		eventType := "claim.released"
		if force {
			eventType = "claim.force_released"
		}
		released, event, err := s.releaseClaimTx(ctx, tx, project, issue, claim,
			eventType, actor, reason, now)
		if err != nil {
			return err
		}
		output = db.LeaseResultFor(released, true, expiredEvents)
		output.Event = &event
		return nil
	})
	if err == nil && expired {
		err = db.ErrClaimExpired
	}
	return output, err
}

// ClaimStatus returns the live claim after expiring timed claims.
func (s *Store) ClaimStatus(ctx context.Context, projectID int64, issueRef string, nowInput time.Time) (db.ClaimStatus, error) {
	now := db.ClaimNow(nowInput)
	output := db.ClaimStatus{HubNow: now}
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.ClaimStatus{HubNow: now}
		issue, _, err := resolveClaimIssueTx(ctx, tx, projectID, issueRef, true)
		if err != nil {
			return err
		}
		events, err := s.expireTimedClaimsTx(ctx, tx, 0, now, 0)
		if err != nil {
			return err
		}
		output.Events = events
		claim, err := claimForIssueTx(ctx, tx, issue.UID, false)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		output.Held = true
		output.Holder = db.PrincipalForClaim(claim)
		output.Claim = &claim
		return nil
	})
	return output, err
}

// ClaimStatusReadOnly returns cached claim state without expiry writes.
func (s *Store) ClaimStatusReadOnly(ctx context.Context, projectID int64, issueRef string, nowInput time.Time) (db.ClaimStatus, error) {
	issue, _, err := resolveClaimIssueTx(ctx, s.DB, projectID, issueRef, false)
	if err != nil {
		return db.ClaimStatus{}, err
	}
	claim, err := claimForIssueTx(ctx, s.DB, issue.UID, false)
	if errors.Is(err, db.ErrNotFound) {
		return db.ClaimStatus{HubNow: db.ClaimNow(nowInput)}, nil
	}
	if err != nil {
		return db.ClaimStatus{}, err
	}
	return db.ClaimStatus{
		Held: true, Holder: db.PrincipalForClaim(claim), Claim: &claim, HubNow: db.ClaimNow(nowInput),
	}, nil
}

// ExpireTimedClaims releases expired claims across all projects in bounded order.
func (s *Store) ExpireTimedClaims(ctx context.Context, now time.Time, limit int) ([]db.Event, error) {
	var events []db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		events, err = s.expireTimedClaimsTx(ctx, tx, 0, db.ClaimNow(now), limit)
		return err
	})
	return events, err
}

// ExpireTimedClaimsForProject releases expired claims in one project.
func (s *Store) ExpireTimedClaimsForProject(ctx context.Context, projectID int64, now time.Time, limit int) ([]db.Event, error) {
	var events []db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		events, err = s.expireTimedClaimsTx(ctx, tx, projectID, db.ClaimNow(now), limit)
		return err
	})
	return events, err
}

// CountLiveClaims returns active hard and unexpired timed claims for a project.
func (s *Store) CountLiveClaims(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_claims
WHERE project_id=$1 AND released_at IS NULL
  AND (claim_kind='hard' OR expires_at > $2)`, projectID, nowStoredTimestamp()).Scan(&count)
	return count, mapSQLError(err, nil)
}

// CountPendingClaims returns unresolved pending claim requests for a project.
func (s *Store) CountPendingClaims(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_claim_requests
WHERE project_id=$1 AND rejected_at IS NULL AND resolved_at IS NULL`, projectID).Scan(&count)
	return count, mapSQLError(err, nil)
}

type claimQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func resolveClaimIssueTx(
	ctx context.Context,
	queryer claimQueryer,
	projectID int64,
	issueRef string,
	lock bool,
) (db.Issue, db.Project, error) {
	issueRef = strings.TrimSpace(issueRef)
	if issueRef == "" {
		return db.Issue{}, db.Project{}, db.ErrNotFound
	}
	query := issueSelect + ` WHERE i.project_id=$1 AND (i.short_id=$2 OR i.uid=$2)`
	if lock {
		query += ` FOR UPDATE OF i`
	}
	issue, err := scanIssue(queryer.QueryRowContext(ctx, query, projectID, issueRef))
	if err != nil {
		return db.Issue{}, db.Project{}, err
	}
	project, err := scanProject(queryer.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
	return issue, project, err
}

func claimForIssueTx(ctx context.Context, queryer claimQueryer, issueUID string, lock bool) (db.IssueClaim, error) {
	query := claimSelect + ` WHERE issue_uid=$1 AND released_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanIssueClaim(queryer.QueryRowContext(ctx, query, issueUID))
}

func claimByIDTx(ctx context.Context, queryer claimQueryer, id int64, lock bool) (db.IssueClaim, error) {
	query := claimSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanIssueClaim(queryer.QueryRowContext(ctx, query, id))
}

func scanIssueClaim(row rowScanner) (db.IssueClaim, error) {
	var claim db.IssueClaim
	var acquiredAt, updatedAt string
	var expiresAt, releasedAt, releaseReason sql.NullString
	err := row.Scan(&claim.ID, &claim.ClaimUID, &claim.ProjectID, &claim.IssueID, &claim.IssueUID,
		&claim.Holder, &claim.HolderInstanceUID, &claim.ClientKind, &claim.Purpose,
		&claim.ClaimKind, &acquiredAt, &expiresAt, &releasedAt, &releaseReason,
		&claim.Revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueClaim{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueClaim{}, mapSQLError(err, nil)
	}
	claim.AcquiredAt, err = parseStoredTime(acquiredAt)
	if err != nil {
		return db.IssueClaim{}, fmt.Errorf("parse claim acquired_at: %w", err)
	}
	claim.UpdatedAt, err = parseStoredTime(updatedAt)
	if err != nil {
		return db.IssueClaim{}, fmt.Errorf("parse claim updated_at: %w", err)
	}
	if expiresAt.Valid {
		value, parseErr := parseStoredTime(expiresAt.String)
		if parseErr != nil {
			return db.IssueClaim{}, fmt.Errorf("parse claim expires_at: %w", parseErr)
		}
		claim.ExpiresAt = &value
	}
	if releasedAt.Valid {
		value, parseErr := parseStoredTime(releasedAt.String)
		if parseErr != nil {
			return db.IssueClaim{}, fmt.Errorf("parse claim released_at: %w", parseErr)
		}
		claim.ReleasedAt = &value
	}
	if releaseReason.Valid {
		claim.ReleaseReason = &releaseReason.String
	}
	return claim, nil
}

func nullableStoredTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatStoredTime(*value)
}

func claimTimedExpired(claim db.IssueClaim, now time.Time) bool {
	return claim.ClaimKind == "timed" && claim.ExpiresAt != nil && !claim.ExpiresAt.After(now)
}

func (s *Store) releaseClaimTx(
	ctx context.Context,
	tx *sql.Tx,
	project db.Project,
	issue db.Issue,
	claim db.IssueClaim,
	eventType string,
	actor string,
	reason string,
	now time.Time,
) (db.IssueClaim, db.Event, error) {
	stamp := formatStoredTime(now)
	result, err := tx.ExecContext(ctx, `UPDATE issue_claims
SET released_at=$1, release_reason=$2, revision=revision+1, updated_at=$1
WHERE id=$3 AND released_at IS NULL`, stamp, nullableString(reason), claim.ID)
	if err != nil {
		return db.IssueClaim{}, db.Event{}, mapSQLError(err, nil)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return db.IssueClaim{}, db.Event{}, db.ErrClaimNotHeld
	}
	released, err := claimByIDTx(ctx, tx, claim.ID, false)
	if err != nil {
		return db.IssueClaim{}, db.Event{}, err
	}
	event, err := s.insertClaimEventTx(ctx, tx, project, issue, eventType, actor, released, reason)
	return released, event, err
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) insertClaimEventTx(
	ctx context.Context,
	tx *sql.Tx,
	project db.Project,
	issue db.Issue,
	eventType string,
	actor string,
	claim db.IssueClaim,
	reason string,
) (db.Event, error) {
	payload := map[string]any{
		"claim_uid": claim.ClaimUID, "holder": claim.Holder,
		"holder_instance_uid": claim.HolderInstanceUID, "client_kind": claim.ClientKind,
		"claim_kind": claim.ClaimKind, "purpose": claim.Purpose,
		"acquired_at": formatStoredTime(claim.AcquiredAt),
	}
	if claim.ExpiresAt != nil {
		payload["expires_at"] = formatStoredTime(*claim.ExpiresAt)
	}
	if claim.ReleasedAt != nil {
		payload["released_at"] = formatStoredTime(*claim.ReleasedAt)
	}
	if reason != "" {
		payload["reason"] = reason
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal claim event: %w", err)
	}
	return s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &issue.ID, IssueUID: &issue.UID, Type: eventType, Actor: actor,
		Payload: string(body),
	})
}

func (s *Store) expireTimedClaimsTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	now time.Time,
	limit int,
) ([]db.Event, error) {
	conditions := []string{"released_at IS NULL", "claim_kind='timed'", "expires_at <= $1"}
	args := []any{formatStoredTime(now)}
	if projectID != 0 {
		args = append(args, projectID)
		conditions = append(conditions, fmt.Sprintf("project_id=$%d", len(args)))
	}
	//nolint:gosec // conditions are fixed SQL fragments; every value remains a bound parameter.
	query := claimSelect + ` WHERE ` + strings.Join(conditions, " AND ") +
		` ORDER BY expires_at ASC, id ASC FOR UPDATE SKIP LOCKED`
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	var claims []db.IssueClaim
	for rows.Next() {
		claim, scanErr := scanIssueClaim(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		claims = append(claims, claim)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	events := make([]db.Event, 0, len(claims))
	for _, claim := range claims {
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, claim.ProjectID))
		if err != nil {
			return nil, err
		}
		issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, claim.IssueID))
		if err != nil {
			return nil, err
		}
		_, event, err := s.releaseClaimTx(ctx, tx, project, issue, claim,
			"claim.expired", "system", "expired", now)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}
