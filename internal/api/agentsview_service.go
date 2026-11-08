package api

import "go.kenn.io/kata/internal/agentsviewwire"

// AgentsViewServicePath identifies ordinary discovery. The wire path retains
// its draft name for route and federation-signature compatibility.
const AgentsViewServicePath = "/api/v1/projects/{project_id}/transcript-service"

// FederationAgentsViewServicePath identifies enrolled relay discovery.
const FederationAgentsViewServicePath = "/api/v1/projects/{project_id}/federation/transcript-service"

// AgentsViewServiceRequest selects a project for discovery.
type AgentsViewServiceRequest struct {
	ProjectID int64 `path:"project_id" required:"true" minimum:"1"`
}

// FederationAgentsViewServiceRequest carries the selected relay generation.
type FederationAgentsViewServiceRequest struct {
	ProjectID       int64  `path:"project_id" required:"true" minimum:"1"`
	Authorization   string `header:"Authorization"`
	ProjectUID      string `query:"project_uid" required:"true" maxLength:"26"`
	AuthorityUID    string `query:"authority_uid" required:"true" maxLength:"26"`
	BindingUID      string `query:"binding_uid" required:"true" maxLength:"26"`
	PeerInstanceUID string `query:"peer_instance_uid" required:"true" maxLength:"26"`
	ProtocolVersion int    `query:"protocol_version" required:"true" minimum:"1"`
	ResetEpoch      int64  `query:"reset_epoch" required:"true" minimum:"1"`
}

// AgentsViewServiceResponse carries the configured discovery result.
type AgentsViewServiceResponse struct {
	CacheControl string `header:"Cache-Control"`
	Body         AgentsViewDiscovery
}

// AgentsViewDescriptor is the signed destination descriptor.
type AgentsViewDescriptor = agentsviewwire.Descriptor

// AgentsViewRouteObservation describes a negotiated hop.
type AgentsViewRouteObservation = agentsviewwire.RouteObservation

// AgentsViewDiscovery contains a descriptor and observed route.
type AgentsViewDiscovery = agentsviewwire.Discovery

// TranscriptServicePath is the legacy name for the discovery path.
const TranscriptServicePath = AgentsViewServicePath

// FederationTranscriptServicePath is the legacy federation path name.
const FederationTranscriptServicePath = FederationAgentsViewServicePath

// TranscriptServiceRequest is the legacy discovery request name.
type TranscriptServiceRequest = AgentsViewServiceRequest

// FederationTranscriptServiceRequest is the legacy relay request name.
type FederationTranscriptServiceRequest = FederationAgentsViewServiceRequest

// TranscriptServiceResponse is the legacy discovery response name.
type TranscriptServiceResponse = AgentsViewServiceResponse
