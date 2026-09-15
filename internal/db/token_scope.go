package db

import (
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/uid"
)

// APITokenScopeKind identifies an immutable native token grant.
type APITokenScopeKind string

const (
	// APITokenScopeIssueSubtree permits ordinary work on one issue subtree.
	APITokenScopeIssueSubtree APITokenScopeKind = "issue_subtree"
)

// APITokenScope is the redacted, stable identity of one native token grant.
type APITokenScope struct {
	Kind         APITokenScopeKind `json:"kind"`
	ProjectUID   string            `json:"project_uid"`
	RootIssueUID string            `json:"root_issue_uid"`
}

// ValidateAPITokenGrant validates the all-null unscoped form or the complete,
// finite issue-subtree form. Domain existence and project role are checked by
// the daemon when it mints and uses a scoped token.
func ValidateAPITokenGrant(scope *APITokenScope, expiresAt *time.Time) error {
	if scope == nil {
		if expiresAt != nil {
			return errors.New("token expiration requires a scope")
		}
		return nil
	}
	if expiresAt == nil || expiresAt.IsZero() {
		return errors.New("scoped token requires a finite expiration")
	}
	if scope.Kind != APITokenScopeIssueSubtree {
		return fmt.Errorf("unknown token scope kind %q", scope.Kind)
	}
	if !uid.Valid(scope.ProjectUID) {
		return errors.New("scoped token project_uid must be a valid ULID")
	}
	if !uid.Valid(scope.RootIssueUID) {
		return errors.New("scoped token root_issue_uid must be a valid ULID")
	}
	return nil
}

// ActiveAPITokenGrantMatches reports whether current is still the exact live
// credential admitted at request authentication time.
func ActiveAPITokenGrantMatches(current, admitted APIToken, now time.Time) bool {
	if current.ID == 0 || current.ID != admitted.ID || current.Actor != admitted.Actor ||
		current.RevokedAt != nil || current.Scope == nil || admitted.Scope == nil ||
		current.ExpiresAt == nil || admitted.ExpiresAt == nil ||
		*current.Scope != *admitted.Scope {
		return false
	}
	return current.ExpiresAt.Equal(*admitted.ExpiresAt) && now.UTC().Before(current.ExpiresAt.UTC())
}
