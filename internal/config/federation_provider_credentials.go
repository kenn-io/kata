package config

import (
	"slices"
	"time"
	"uuid"

	"go.kenn.io/kata/pkg/federationprovider"
)

// FederationProviderCredential retains one provider operation alongside its
// candidate token in credentials.toml. LocalProjectUID stays unchanged when
// the local replica adopts the hub project UID. Command is trusted local
// configuration retained for cleanup after a mapping is removed.
type FederationProviderCredential struct {
	RequestID        uuid.UUID                 `toml:"request_id"`
	Command          []string                  `toml:"command"`
	Intent           federationprovider.Intent `toml:"intent"`
	SpokeInstanceUID string                    `toml:"spoke_instance_uid"`
	LocalProjectUID  string                    `toml:"local_project_uid"`
	Status           federationprovider.Status `toml:"status,omitempty"`
	HubProjectUID    string                    `toml:"hub_project_uid,omitempty"`
	EnrollmentID     int64                     `toml:"enrollment_id,omitempty"`
	ExpiresAt        time.Time                 `toml:"expires_at,omitempty"`
}

// Equal compares the whole retained credential by value, including optional
// provider state. Pointer equality would reject exact retries after a file read.
func (credential FederationCredential) Equal(other FederationCredential) bool {
	ls, rs := credential.Signing, other.Signing
	credential.Signing, other.Signing = nil, nil
	if (ls == nil) != (rs == nil) || (ls != nil && *ls != *rs) {
		return false
	}
	left, right := credential.Provider, other.Provider
	credential.Provider, other.Provider = nil, nil
	leftLeave, rightLeave := credential.NativeLeave, other.NativeLeave
	credential.NativeLeave, other.NativeLeave = nil, nil
	if credential != other {
		return false
	}
	if (left == nil) != (right == nil) {
		return false
	}
	if left != nil && (left.RequestID != right.RequestID ||
		!slices.Equal(left.Command, right.Command) ||
		left.Intent != right.Intent ||
		left.SpokeInstanceUID != right.SpokeInstanceUID ||
		left.LocalProjectUID != right.LocalProjectUID ||
		left.Status != right.Status ||
		left.HubProjectUID != right.HubProjectUID ||
		left.EnrollmentID != right.EnrollmentID ||
		!left.ExpiresAt.Equal(right.ExpiresAt)) {
		return false
	}
	if (leftLeave == nil) != (rightLeave == nil) {
		return false
	}
	if leftLeave == nil {
		return true
	}
	if leftLeave.Enabled != rightLeave.Enabled || leftLeave.PushEnabled != rightLeave.PushEnabled ||
		leftLeave.CreatedCredential != rightLeave.CreatedCredential ||
		len(leftLeave.Preparations) != len(rightLeave.Preparations) {
		return false
	}
	for i, preparation := range leftLeave.Preparations {
		otherPreparation := rightLeave.Preparations[i]
		if preparation.ID != otherPreparation.ID || !slices.Equal(preparation.Keys, otherPreparation.Keys) {
			return false
		}
	}
	return true
}
