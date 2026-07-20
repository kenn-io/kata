package kata

import (
	"context"
	"errors"
)

// ErrAccessDenied is returned by an AccessController when a caller must not
// learn whether the requested resource exists.
var ErrAccessDenied = errors.New("kata: access denied")

// Principal is the authenticated identity supplied by an embedding host.
// Subject is a stable opaque identifier and also anchors lease ownership. Its
// exact bytes are significant; callers must supply the same canonical value
// on every request.
// Actor is the display snapshot Kata records on mutations instead of accepting
// an actor from request data.
type Principal struct {
	Subject string
	Actor   string
}

// Operation identifies the matched Kata HTTP operation without assigning any
// host-specific meaning to it.
type Operation struct {
	ID         string
	Method     string
	Path       string
	PathParams map[string]string

	// ProjectIDs and ProjectUIDs identify every project whose data the
	// operation may read or change. AllProjects is true when a global selector
	// is used or when an operation can depend on projects that cannot be safely
	// bounded before dispatch. Values are parsed and validated before
	// authorization; cross-project operations include both sides.
	ProjectIDs  []int64
	ProjectUIDs []string
	AllProjects bool
}

// AccessRequest is the complete input to one host authorization decision.
type AccessRequest struct {
	Principal Principal
	Operation Operation
}

// AccessLease revalidates a host decision while a long-lived response is
// active. Revalidate must fail as soon as the principal or resource authority
// represented by the lease is no longer current.
type AccessLease interface {
	Revalidate(context.Context) error
}

// AccessDecision carries state needed after a request is admitted. Lease may
// be nil for bounded responses; long-lived operations require one.
type AccessDecision struct {
	Lease AccessLease
}

// AccessController makes host-owned authorization decisions for a mounted
// service. A request may be repeated with a larger, cumulative project scope
// when resolving a UID, link, or graph discovers another project, so
// implementations must make retry-safe decisions. Returning ErrAccessDenied
// produces a generic not-found response; other errors make only the mounted
// service temporarily unavailable.
type AccessController interface {
	Authorize(context.Context, AccessRequest) (AccessDecision, error)
}

type principalContextKey struct{}

// WithPrincipal attaches a host-authenticated principal to an in-process
// request. It is intended for middleware immediately outside Service.Handler.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func principalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
