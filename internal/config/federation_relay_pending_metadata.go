package config

import (
	"context"
	"strings"
)

// FederationRelayPendingMetadataReader resolves one chosen local project name
// before a replica exists. It returns only redacted metadata, never tokens or an
// inventory. Active replicas use their existing UID-based credential lookup.
type FederationRelayPendingMetadataReader interface {
	PendingRelayCredentialMetadata(context.Context, string) (string, FederationCredentialMetadata, bool, error)
}

func (homeFederationCredentialStore) PendingRelayCredentialMetadata(_ context.Context, name string) (string, FederationCredentialMetadata, bool, error) {
	credentials, err := ReadFederationCredentials()
	if err != nil {
		return "", FederationCredentialMetadata{}, false, err
	}
	name = strings.TrimSpace(name)
	var uid string
	var metadata FederationCredentialMetadata
	for projectUID, credential := range credentials.Projects {
		if !credential.RelayEnrollmentPending || credential.SpokeProjectName != name {
			continue
		}
		if uid != "" {
			return "", FederationCredentialMetadata{}, false, ErrFederationCredentialConflict
		}
		uid = projectUID
		metadata = credential.Metadata()
	}
	return uid, metadata, uid != "", nil
}
