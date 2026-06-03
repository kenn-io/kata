package tui

import "context"

// KataAPI is the daemon surface the TUI consumes. It is owned by this
// package (the consumer) and covers exactly the methods the TUI calls
// today — no speculative future-proofing for a remote engine. When that
// engine lands it will satisfy this interface structurally and the
// surface can grow alongside concrete need.
//
// The narrower listAPI / detailAPI / labelLister interfaces continue to
// type the call sites that only need a slice of the surface. KataAPI is
// the union held by Model.api so a single value can be passed to those
// narrower seams.
type KataAPI interface {
	listAPI
	detailAPI
	federationSpokeAPI

	ListProjectsWithStats(ctx context.Context) ([]ProjectSummaryWithStats, error)
	ListLabels(ctx context.Context, projectID int64) ([]LabelCount, error)
	ResolveProject(ctx context.Context, startPath string) (*ResolveResp, error)
}

type federationSpokeAPI interface {
	GetInstance(ctx context.Context) (InstanceInfo, error)
	ListProjects(ctx context.Context) ([]ProjectSummary, error)
	FederationStatus(ctx context.Context) (FederationStatusBody, error)
	CreateFederationReplica(ctx context.Context, body CreateFederationReplicaInput) (FederationReplicaResult, error)
}

type federationHubAdminAPI interface {
	GetInstance(ctx context.Context) (InstanceInfo, error)
	ListProjects(ctx context.Context) ([]ProjectSummary, error)
	EnsureProject(ctx context.Context, name string) (ProjectSummary, error)
	EnableFederation(ctx context.Context, projectID int64, actor string) (ProjectFederationMetadata, error)
	CreateFederationEnrollment(ctx context.Context, body CreateFederationEnrollmentInput) (FederationEnrollment, error)
}

type federationEnrollmentAPI interface {
	ProjectFederation(ctx context.Context, hubProjectID int64) (ProjectFederationMetadata, error)
}
