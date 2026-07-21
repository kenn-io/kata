package daemon

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// EmbeddingProfile is the daemon-side representation of the public service
// profile. Standalone construction always uses the zero value.
type EmbeddingProfile string

const embeddingProfileRestricted EmbeddingProfile = "restricted"

// HostCapability is the internal authority class forwarded through the public adapter.
type HostCapability string

const (
	hostCapabilityRead     HostCapability = "read"
	hostCapabilityWrite    HostCapability = "write"
	hostCapabilityManage   HostCapability = "manage"
	hostCapabilityFederate HostCapability = "federate"
)

// HostOperationKind is the internal domain classification for a route.
type HostOperationKind string

const (
	hostOperationServiceRead               HostOperationKind = "service_read"
	hostOperationProjectRead               HostOperationKind = "project_read"
	hostOperationTaskRead                  HostOperationKind = "task_read"
	hostOperationTaskMutation              HostOperationKind = "task_mutation"
	hostOperationTaskAdministration        HostOperationKind = "task_administration"
	hostOperationProjectAdministration     HostOperationKind = "project_administration"
	hostOperationTokenAdministration       HostOperationKind = "token_administration"
	hostOperationFederationRead            HostOperationKind = "federation_read"
	hostOperationFederationAdministration  HostOperationKind = "federation_administration"
	hostOperationFederationTransport       HostOperationKind = "federation_transport"
	hostOperationIntegrationAdministration HostOperationKind = "integration_administration"
)

// HostOperationPolicy carries Kata-owned route metadata through the public adapter.
type HostOperationPolicy struct {
	Kind       HostOperationKind
	Capability HostCapability
	Mutation   bool
	LongLived  bool
	restricted bool
}

var hostOperationPolicies = buildHostOperationPolicies()

func buildHostOperationPolicies() map[string]HostOperationPolicy {
	policies := make(map[string]HostOperationPolicy)
	registerServiceOperationPolicies(policies)
	registerProjectOperationPolicies(policies)
	registerTaskOperationPolicies(policies)
	registerFederationOperationPolicies(policies)
	return policies
}

func registerHostOperations(
	policies map[string]HostOperationPolicy,
	policy HostOperationPolicy,
	operationIDs ...string,
) {
	for _, operationID := range operationIDs {
		if _, exists := policies[operationID]; exists {
			panic("duplicate host operation policy: " + operationID)
		}
		policies[operationID] = policy
	}
}

func hostOperationPolicy(operationID string) (HostOperationPolicy, bool) {
	policy, ok := hostOperationPolicies[operationID]
	return policy, ok
}

func withEmbeddingProfile(humaAPI huma.API, profile EmbeddingProfile) {
	if profile != embeddingProfileRestricted {
		return
	}
	humaAPI.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		operation := ctx.Operation()
		if operation == nil {
			writeHostAccessError(ctx, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		policy, ok := hostOperationPolicy(operation.OperationID)
		if !ok || policy.restricted {
			writeHostAccessError(ctx, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		next(ctx)
	})
}

func registerServiceOperationPolicies(policies map[string]HostOperationPolicy) {
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationServiceRead, Capability: hostCapabilityRead,
	}, "ping", "health", "instance", "openAPI")
}
