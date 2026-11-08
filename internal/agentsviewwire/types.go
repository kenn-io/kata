// Package agentsviewwire defines the discovery JSON shared by API and policy code.
package agentsviewwire

// Descriptor is the JSON wire shape signed by a project's root. Field order
// and names are part of the existing descriptor signing format.
type Descriptor struct {
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

// RouteObservation identifies one per-hop route generation.
type RouteObservation struct {
	InstanceUID         string `json:"instance_uid"`
	ProjectUID          string `json:"project_uid"`
	BindingUID          string `json:"binding_uid,omitempty"`
	UpstreamInstanceUID string `json:"upstream_instance_uid,omitempty"`
	ResetEpoch          int64  `json:"reset_epoch,omitempty"`
}

// Discovery contains a signed descriptor and observed route.
type Discovery struct {
	Descriptor  Descriptor         `json:"descriptor"`
	InstanceUID string             `json:"instance_uid"`
	Route       []RouteObservation `json:"route"`
}
