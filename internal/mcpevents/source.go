package mcpevents

import (
	"context"
	"slices"
)

// Project identifies one project visible to a principal.
type Project struct {
	UID  string
	ID   int64
	Name string
}

// Occurrence is one durable event row, or the project and issue identity of
// one, as the worker matches it against subscriptions.
type Occurrence struct {
	EventID    int64 // global events.id
	EventUID   string
	Type       string
	ProjectUID string
	IssueUID   string
	Payload    []byte
}

// Authority reconstructs the full authorization context of a persisted
// principal. It is implemented by the daemon, which owns the decision
// functions, and injected. It never allows on a missing context.
type Authority interface {
	// Reacquire returns ErrPrincipalRevoked when the principal no longer holds
	// authority, and ErrAuthorityUnavailable when no decision can be made now.
	Reacquire(ctx context.Context, p Principal) (Authorized, error)
}

// Authorized is one principal's current read authority.
type Authorized interface {
	// VisibleProjects returns the visible projects, intersected with selection
	// when it is non-nil, sorted by UID.
	VisibleProjects(ctx context.Context, selection []string) ([]Project, error)
	// AuthorizeOccurrence rechecks that the occurrence's project and issue are
	// visible, returning ErrOccurrenceHidden when they are not.
	AuthorizeOccurrence(ctx context.Context, o Occurrence) error
	// LifecycleMayChangeInbox reports whether closing, reopening, deleting or
	// restoring the issue can change recipient's inbox: the issue holds a
	// valid request for recipient, or the change can move other issues in or
	// out of the principal's scope. A hidden or missing issue cannot.
	LifecycleMayChangeInbox(ctx context.Context, issueUID, recipient string) (bool, error)
	// Scoped reports a credential confined to one issue subtree. Deleting,
	// restoring or reparenting an issue can move other issues, and their
	// requests, in or out of its scope.
	Scoped() bool
}

// ResolveProjects returns the visible projects for a subscription. An
// explicit selection with none visible returns ErrScopeRemoved; a dynamic
// selection keeps an empty catalog, which is not a loss of authority.
func ResolveProjects(ctx context.Context, a Authorized, args Arguments) ([]Project, error) {
	projects, err := a.VisibleProjects(ctx, args.ProjectUIDs)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 && !args.Dynamic() {
		return nil, ErrScopeRemoved
	}
	return projects, nil
}

// sortedUnique reports whether uids is strictly ascending.
func sortedUnique(uids []string) bool {
	return slices.IsSorted(uids) && len(slices.Compact(slices.Clone(uids))) == len(uids)
}
