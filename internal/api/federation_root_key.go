package api

// RepinFederationRootKeyRequest previews or replaces one root project's pin
// with this daemon's loaded root signing key.
type RepinFederationRootKeyRequest struct {
	ProjectName string `path:"project_name"`
	Body        struct {
		Preflight bool `json:"preflight,omitempty"`
		// PreviousKeyFile is an absolute path on the daemon host to the key the
		// project is pinned to. When given, the repin is a signed rotation.
		PreviousKeyFile string `json:"previous_key_file,omitempty"`
		// StrandRelays accepts that an unsigned repin leaves the project's
		// active relay enrollments unable to sync. Without it, an unsigned
		// repin is refused while such enrollments exist.
		StrandRelays bool `json:"strand_relays,omitempty"`
	}
}

// FederationRootKeyRepinResult describes a root key repin or its preflight.
type FederationRootKeyRepinResult struct {
	ProjectName    string `json:"project_name"`
	ProjectUID     string `json:"project_uid"`
	Status         string `json:"status" enum:"ready,current,pinned,repinned,rotated"`
	Method         string `json:"method" enum:"none,pin,signed_rotation,unsigned_repin"`
	KeyFile        string `json:"key_file,omitempty"`
	CreatesKeyFile bool   `json:"creates_key_file,omitempty"`
	KeyID          string `json:"key_id,omitempty"`
	PreviousKeyID  string `json:"previous_key_id,omitempty"`
	// ActiveRelayEnrollments counts the grants pinned to the project's current
	// key. An unsigned repin strands them.
	ActiveRelayEnrollments int64  `json:"active_relay_enrollments"`
	Consequence            string `json:"consequence,omitempty"`
}

// RepinFederationRootKeyResponse returns the repin result.
type RepinFederationRootKeyResponse struct {
	Body FederationRootKeyRepinResult
}
