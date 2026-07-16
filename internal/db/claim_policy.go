package db

import (
	"fmt"
	"strings"
	"time"

	katauid "go.kenn.io/kata/internal/uid"
)

const (
	// MinTimedClaimTTL is the shortest accepted timed claim lease.
	MinTimedClaimTTL = time.Minute
	// MaxTimedClaimTTL is the longest accepted timed claim lease.
	MaxTimedClaimTTL = 24 * time.Hour
)

// ClaimNow normalizes an optional caller timestamp for claim arbitration.
func ClaimNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

// ValidateClaimPrincipal validates the stable holder tuple used by claim arbitration.
func ValidateClaimPrincipal(principal ClaimPrincipal) error {
	if !katauid.Valid(principal.HolderInstanceUID) {
		return fmt.Errorf("%w: invalid holder instance uid", ErrClaimValidation)
	}
	if strings.TrimSpace(principal.Holder) == "" {
		return fmt.Errorf("%w: holder is required", ErrClaimValidation)
	}
	return nil
}

// ValidateTimedClaimTTL enforces the supported bounded lease duration.
func ValidateTimedClaimTTL(ttl time.Duration) error {
	if ttl < MinTimedClaimTTL || ttl > MaxTimedClaimTTL {
		return fmt.Errorf("%w: timed claim ttl must be between 60s and 24h", ErrClaimValidation)
	}
	return nil
}

// SameClaimPrincipal reports whether a claim belongs to the exact holder tuple.
func SameClaimPrincipal(claim IssueClaim, principal ClaimPrincipal) bool {
	return claim.Holder == principal.Holder &&
		claim.HolderInstanceUID == principal.HolderInstanceUID &&
		claim.ClientKind == principal.ClientKind
}

// PrincipalForClaim projects the holder tuple from a persisted claim.
func PrincipalForClaim(claim IssueClaim) ClaimPrincipal {
	return ClaimPrincipal{
		HolderInstanceUID: claim.HolderInstanceUID,
		Holder:            claim.Holder,
		ClientKind:        claim.ClientKind,
	}
}

// LeaseResultFor builds the common claim arbitration result envelope.
func LeaseResultFor(claim IssueClaim, granted bool, events []Event) LeaseResult {
	return LeaseResult{
		Granted: granted,
		Holder:  PrincipalForClaim(claim),
		Claim:   &claim,
		Events:  events,
	}
}

// SameClaimGateHolder reports whether a mutation principal owns the cached
// claim. Client kind is deliberately not part of the mutation gate identity.
func SameClaimGateHolder(claim IssueClaim, principal ClaimPrincipal) bool {
	return claim.Holder == principal.Holder &&
		claim.HolderInstanceUID == principal.HolderInstanceUID
}

// ClaimStatusRefreshErrorKey returns the stable metadata key for one issue's
// latest status-refresh failure.
func ClaimStatusRefreshErrorKey(projectID int64, issueUID string) string {
	issueUID = strings.TrimSpace(issueUID)
	if projectID == 0 || issueUID == "" {
		return ""
	}
	return fmt.Sprintf("claim_status_refresh_error:%d:%s", projectID, issueUID)
}

// ValidatePendingClaimResolution verifies that a hub claim resolves the
// exact pending principal, issue, and lease kind that requested it.
func ValidatePendingClaimResolution(pending PendingClaimRequest, issue Issue, claim IssueClaim) error {
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

// ValidateStatusClaimIssueIdentity rejects a status projection for a
// different issue before freshness comparisons can turn it into a no-op.
func ValidateStatusClaimIssueIdentity(issue Issue, claim IssueClaim) error {
	if claim.IssueUID != "" && claim.IssueUID != issue.UID {
		return fmt.Errorf("%w: status claim issue mismatch", ErrClaimValidation)
	}
	return nil
}

// NormalizeCachedClaim validates and fills a hub claim before persistence in
// a backend-local status cache.
func NormalizeCachedClaim(status ClaimStatus, issue Issue, now time.Time) (IssueClaim, error) {
	if status.Claim == nil {
		return IssueClaim{}, fmt.Errorf("%w: held status requires claim", ErrClaimValidation)
	}
	claim := *status.Claim
	if err := ValidateStatusClaimIssueIdentity(issue, claim); err != nil {
		return IssueClaim{}, err
	}
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
	now = ClaimNow(now)
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
	if err := ValidateClaimPrincipal(PrincipalForClaim(claim)); err != nil {
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

// StaleSameClaimRefresh reports whether an incoming projection would move a
// cached claim backward in hub time or revision.
func StaleSameClaimRefresh(live, incoming IssueClaim) bool {
	if incoming.UpdatedAt.Before(live.UpdatedAt) {
		return true
	}
	return incoming.UpdatedAt.Equal(live.UpdatedAt) && incoming.Revision < live.Revision
}
