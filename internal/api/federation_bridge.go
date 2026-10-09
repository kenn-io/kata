package api

import "go.kenn.io/kata/internal/db"

// ConnectFederationBridgeRequest uses exactly one configured hub credential.
// Preflight resolves the account and project without issuing a transport grant.
type ConnectFederationBridgeRequest struct {
	Body struct {
		HubCatalog      string `json:"hub_catalog" minLength:"1"`
		HubProject      string `json:"hub_project" minLength:"1"`
		ProjectName     string `json:"project_name" minLength:"1"`
		Actor           string `json:"actor,omitempty"`
		Preflight       bool   `json:"preflight,omitempty"`
		ServeDownstream bool   `json:"serve_downstream,omitempty"`
	}
}

// FederationBridgeBody describes the selected project, account and connection without credentials.
type FederationBridgeBody struct {
	HubCatalog      string `json:"hub_catalog"`
	HubURL          string `json:"hub_url"`
	HubInstanceUID  string `json:"hub_instance_uid"`
	HubProjectUID   string `json:"hub_project_uid"`
	HubProjectID    int64  `json:"hub_project_id"`
	ProjectName     string `json:"project_name"`
	ProjectID       int64  `json:"project_id,omitempty"`
	LocalAccount    string `json:"local_account"`
	UpstreamAccount string `json:"upstream_account"`
	Direction       string `json:"direction" enum:"bidirectional"`
	Status          string `json:"status" enum:"ready,connected"`
}

// ConnectFederationBridgeResponse returns the selected bridge preflight or enrollment result.
type ConnectFederationBridgeResponse struct{ Body FederationBridgeBody }

// FederationBridgeStatusRequest selects one locally named bridge.
type FederationBridgeStatusRequest struct {
	ProjectName string `path:"project_name"`
}

// FederationBridgeStatusBody reports the current selected bridge and credential status.
type FederationBridgeStatusBody struct {
	ProjectID        int64                    `json:"project_id,omitempty"`
	ProjectUID       string                   `json:"project_uid"`
	ProjectName      string                   `json:"project_name"`
	HubCatalog       string                   `json:"hub_catalog,omitempty"`
	HubURL           string                   `json:"hub_url"`
	LocalAccount     string                   `json:"local_account"`
	UpstreamAccount  string                   `json:"upstream_account"`
	Direction        string                   `json:"direction" enum:"bidirectional,pull_only"`
	State            string                   `json:"state" enum:"connected,enrollment_pending,paused,offline,revoked"`
	CredentialStatus string                   `json:"credential_status"`
	Relay            *db.RelayBindingConfig   `json:"relay,omitempty"`
	Federation       *FederationProjectStatus `json:"federation,omitempty"`
}

// FederationBridgeStatusResponse returns one locally selected bridge status.
type FederationBridgeStatusResponse struct{ Body FederationBridgeStatusBody }

// DisconnectRelayRequest can attenuate only its presented transport grant.
type DisconnectRelayRequest struct {
	ProjectID     int64  `path:"project_id"`
	Authorization string `header:"Authorization"`
	Body          struct {
		SpokeInstanceUID string `json:"spoke_instance_uid"`
	}
}

// DisconnectRelayResponse reports attenuation of the presented narrow transport grant.
type DisconnectRelayResponse struct {
	Body struct {
		Revoked bool `json:"revoked"`
	}
}

// DisconnectFederationBridgeRequest previews or executes one local teardown.
type DisconnectFederationBridgeRequest struct {
	ProjectName string `path:"project_name"`
	Body        struct {
		Preflight bool `json:"preflight,omitempty"`
	}
}

// FederationBridgeDisconnectResult describes a local bridge teardown or its preflight.
type FederationBridgeDisconnectResult struct {
	ProjectName string `json:"project_name"`
	ProjectUID  string `json:"project_uid,omitempty"`
	Status      string `json:"status" enum:"ready,disconnected"`
}

// DisconnectFederationBridgeResponse returns the selected bridge teardown result.
type DisconnectFederationBridgeResponse struct {
	Body FederationBridgeDisconnectResult
}
