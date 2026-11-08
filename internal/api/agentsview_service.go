package api

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

// AgentsViewDescriptor is the JSON wire shape signed by a project's root.
// Field order and names preserve the existing descriptor serialization.
type AgentsViewDescriptor struct {
	Version         int    `json:"version"`
	ProjectUID      string `json:"project_uid"`
	AuthorityUID    string `json:"authority_uid"`
	ServiceID       string `json:"service_id"`
	LocatorBase     string `json:"locator_base"`
	EnvelopeVersion int    `json:"envelope_version"`
	MaxBytes        int64  `json:"max_bytes"`
	IssuedAt        int64  `json:"issued_at"`
	ExpiresAt       int64  `json:"expires_at"`
	KeyID           string `json:"key_id"`
	Signature       []byte `json:"signature,omitempty"`
}

// AgentsViewRouteObservation is the authenticated, per-hop route generation.
type AgentsViewRouteObservation struct {
	InstanceUID         string `json:"instance_uid"`
	ProjectUID          string `json:"project_uid"`
	BindingUID          string `json:"binding_uid,omitempty"`
	UpstreamInstanceUID string `json:"upstream_instance_uid,omitempty"`
	ResetEpoch          int64  `json:"reset_epoch,omitempty"`
}

type AgentsViewDiscovery struct {
	Descriptor  AgentsViewDescriptor         `json:"descriptor"`
	InstanceUID string                       `json:"instance_uid"`
	Route       []AgentsViewRouteObservation `json:"route"`
}

// The aliases preserve source compatibility for consumers of the initial
// draft API. New code should use the AgentsView names.
const TranscriptServicePath = AgentsViewServicePath
const FederationTranscriptServicePath = FederationAgentsViewServicePath

type TranscriptServiceRequest = AgentsViewServiceRequest
type FederationTranscriptServiceRequest = FederationAgentsViewServiceRequest
type TranscriptServiceResponse = AgentsViewServiceResponse
