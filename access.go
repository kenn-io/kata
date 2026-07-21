package kata

import (
	"context"
	"database/sql"
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

// Capability is the product-neutral authority class an operation requires.
// Embedding hosts map these classes to their own roles and grants.
type Capability string

// Capability values form the stable authority vocabulary exposed to hosts.
const (
	CapabilityRead     Capability = "read"
	CapabilityWrite    Capability = "write"
	CapabilityManage   Capability = "manage"
	CapabilityFederate Capability = "federate"
)

// OperationKind describes the domain boundary of a matched route. The values
// are owned by Kata so embedding hosts do not have to duplicate route policy.
type OperationKind string

// Operation kinds group routes by their data and administration boundary.
const (
	OperationServiceRead               OperationKind = "service_read"
	OperationProjectRead               OperationKind = "project_read"
	OperationTaskRead                  OperationKind = "task_read"
	OperationTaskMutation              OperationKind = "task_mutation"
	OperationTaskAdministration        OperationKind = "task_administration"
	OperationProjectAdministration     OperationKind = "project_administration"
	OperationTokenAdministration       OperationKind = "token_administration"
	OperationFederationRead            OperationKind = "federation_read"
	OperationFederationAdministration  OperationKind = "federation_administration"
	OperationFederationTransport       OperationKind = "federation_transport"
	OperationIntegrationAdministration OperationKind = "integration_administration"
)

// OperationPolicy is Kata's deny-by-default classification of one route.
// Mutation and LongLived let a host apply browser and resource controls without
// inferring behavior from an HTTP verb or operation name.
type OperationPolicy struct {
	Kind       OperationKind
	Capability Capability
	Mutation   bool
	LongLived  bool
}

// Operation identifies the matched Kata HTTP operation without assigning any
// host-specific meaning to it.
type Operation struct {
	ID         string
	Method     string
	Path       string
	PathParams map[string]string
	Policy     OperationPolicy

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

// Transaction is the narrow database/sql surface supplied to a transaction
// fence. Both SQLite and PostgreSQL transactions implement it.
type Transaction interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// TransactionFence revalidates authority from inside the active storage
// transaction. Returning an error aborts and rolls back the domain mutation.
type TransactionFence func(context.Context, Transaction) error

// AccessDecision carries state needed after a request is admitted. Lease may
// be nil for bounded responses; long-lived operations require one.
type AccessDecision struct {
	Lease AccessLease
	// TransactionFence should be present on every successful decision. Kata
	// invokes it when handling the operation begins a storage transaction,
	// before the transaction's first domain write, and retains its database
	// locks through commit or rollback.
	TransactionFence TransactionFence
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
