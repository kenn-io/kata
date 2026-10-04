package api

// ConfigureFederationSigningRequest selects daemon-local signing references for
// an existing enrollment. HubURL optionally pins the key to a replacement hub.
type ConfigureFederationSigningRequest struct {
	ProjectUID string `path:"project_uid"`
	Body       ConfigureFederationSigningRequestBody
}

// ConfigureFederationSigningRequestBody contains references, never secret bytes.
type ConfigureFederationSigningRequestBody struct {
	KeyID   string `json:"key_id"`
	KeyFile string `json:"key_file,omitempty"`
	KeyEnv  string `json:"key_env,omitempty"`
	HubURL  string `json:"hub_url,omitempty"`
}
