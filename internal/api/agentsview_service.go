package api

import "go.kenn.io/kata/internal/agentsviewwire"

const AgentsViewServicePath = "/api/v1/projects/{project_id}/transcript-service"
const FederationAgentsViewServicePath = "/api/v1/projects/{project_id}/federation/transcript-service"

// These wire paths and operation IDs retain their published draft names for
// route and federation-signature compatibility while Go APIs use AgentsView.

type AgentsViewServiceRequest struct {
	ProjectID int64 `path:"project_id" required:"true" minimum:"1"`
}

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

type AgentsViewServiceResponse struct {
	CacheControl string `header:"Cache-Control"`
	Body         AgentsViewDiscovery
}

type AgentsViewDescriptor = agentsviewwire.Descriptor
type AgentsViewRouteObservation = agentsviewwire.RouteObservation
type AgentsViewDiscovery = agentsviewwire.Discovery

// The aliases preserve source compatibility for consumers of the initial
// draft API. New code should use the AgentsView names.
const TranscriptServicePath = AgentsViewServicePath
const FederationTranscriptServicePath = FederationAgentsViewServicePath

type TranscriptServiceRequest = AgentsViewServiceRequest
type FederationTranscriptServiceRequest = FederationAgentsViewServiceRequest
type TranscriptServiceResponse = AgentsViewServiceResponse
