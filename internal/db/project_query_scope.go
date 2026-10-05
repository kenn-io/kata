package db

import (
	"context"
	"slices"
	"strings"
)

type projectQueryScopeKey struct{}
type unrestrictedProjectWriteKey struct{}
type projectAccessTransactionCheckKey struct{}

// ProjectAccessTransactionCheck verifies access to a concrete project inside
// the domain transaction that will read or mutate one of its issues.
type ProjectAccessTransactionCheck func(context.Context, Transaction, string) error

// WithUnrestrictedProjectWrites represents the operator's explicit tokenless
// private-network write grant. It never grants team membership or owner access.
func WithUnrestrictedProjectWrites(ctx context.Context) context.Context {
	return context.WithValue(ctx, unrestrictedProjectWriteKey{}, true)
}

// UnrestrictedProjectWrites reports explicit tokenless private-network write authority.
func UnrestrictedProjectWrites(ctx context.Context) bool {
	allowed, _ := ctx.Value(unrestrictedProjectWriteKey{}).(bool)
	return allowed
}

// WithProjectAccessTransactionCheck installs the request's live project
// authorization check for native transactions that discover their target
// project only after request admission.
func WithProjectAccessTransactionCheck(ctx context.Context, check ProjectAccessTransactionCheck) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, projectAccessTransactionCheckKey{}, check)
}

// CheckProjectAccessTransaction authorizes projectUID in tx when the current
// request carries a dynamic project authorization check.
func CheckProjectAccessTransaction(ctx context.Context, tx Transaction, projectUID string) error {
	check, _ := ctx.Value(projectAccessTransactionCheckKey{}).(ProjectAccessTransactionCheck)
	if check == nil {
		return nil
	}
	return check(ctx, tx, projectUID)
}

// WithAuthorizedProjects constrains native query candidates to the admitted
// projects. A nil set means owner authority; an empty set denies every project.
func WithAuthorizedProjects(ctx context.Context, projectUIDs []string) context.Context {
	return context.WithValue(ctx, projectQueryScopeKey{}, slices.Clone(projectUIDs))
}

// AuthorizedProjects returns the request's candidate boundary. Callers must
// apply it before ranking, counting, hydration or limiting rows.
func AuthorizedProjects(ctx context.Context) ([]string, bool) {
	uids, ok := ctx.Value(projectQueryScopeKey{}).([]string)
	return slices.Clone(uids), ok && uids != nil
}

// AuthorizedProjectPredicate builds a bound SQL predicate for a trusted column
// expression. bind appends a value and returns its backend placeholder.
func AuthorizedProjectPredicate(ctx context.Context, column string, bind func(any) string) string {
	uids, restricted := AuthorizedProjects(ctx)
	if !restricted {
		return "1=1"
	}
	if len(uids) == 0 {
		return "1=0"
	}
	places := make([]string, len(uids))
	for i, value := range uids {
		places[i] = bind(value)
	}
	return column + " IN (" + strings.Join(places, ",") + ")"
}
