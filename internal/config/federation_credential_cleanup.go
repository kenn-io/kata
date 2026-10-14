package config

import "context"

// FederationCredentialRemover removes exactly an observed credential without
// deleting a concurrent reconnect or rebind. Missing entries are safe retries.
type FederationCredentialRemover interface {
	DeleteFederationCredentialIfUnchanged(context.Context, string, FederationCredential) error
}

func (homeFederationCredentialStore) DeleteFederationCredentialIfUnchanged(_ context.Context, projectUID string, expected FederationCredential) error {
	if projectUID == "" {
		return ErrFederationCredentialConflict
	}
	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()
	credentials, err := readFederationCredentials()
	if err != nil {
		return err
	}
	current, found := credentials.Projects[projectUID]
	if !found {
		return nil
	}
	if !current.Equal(expected) {
		return ErrFederationCredentialConflict
	}
	delete(credentials.Projects, projectUID)
	return writeFederationCredentials(credentials)
}
