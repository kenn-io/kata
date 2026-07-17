package kata

import (
	"context"
	"sync"

	"go.kenn.io/kata/internal/config"
)

// FederationCredential is the secret material used by one spoke project to
// communicate with its hub.
type FederationCredential struct {
	HubURL        string
	HubProjectID  int64
	Token         string
	Capabilities  string
	Actor         string
	AllowInsecure bool
}

// FederationCredentialStore supplies service-scoped federation secrets.
// Implementations must be safe for concurrent use. Callers that need
// credentials to survive process restarts should provide a durable store;
// otherwise each Service receives its own in-memory store.
type FederationCredentialStore interface {
	FederationCredential(context.Context, string) (FederationCredential, bool, error)
	StoreFederationCredential(context.Context, string, FederationCredential) error
	DeleteFederationCredential(context.Context, string) error
}

type serviceCredentialStoreAdapter struct {
	store FederationCredentialStore
}

func (a serviceCredentialStoreAdapter) FederationCredential(
	ctx context.Context, projectUID string,
) (config.FederationCredential, bool, error) {
	credential, ok, err := a.store.FederationCredential(ctx, projectUID)
	if err != nil || !ok {
		return config.FederationCredential{}, ok, err
	}
	return internalFederationCredential(credential), true, nil
}

func (a serviceCredentialStoreAdapter) StoreFederationCredential(
	ctx context.Context, projectUID string, credential config.FederationCredential,
) error {
	return a.store.StoreFederationCredential(ctx, projectUID, publicFederationCredential(credential))
}

func (a serviceCredentialStoreAdapter) DeleteFederationCredential(ctx context.Context, projectUID string) error {
	return a.store.DeleteFederationCredential(ctx, projectUID)
}

func internalFederationCredential(credential FederationCredential) config.FederationCredential {
	return config.FederationCredential{
		HubURL:        credential.HubURL,
		HubProjectID:  credential.HubProjectID,
		Token:         credential.Token,
		Capabilities:  credential.Capabilities,
		Actor:         credential.Actor,
		AllowInsecure: credential.AllowInsecure,
	}
}

func publicFederationCredential(credential config.FederationCredential) FederationCredential {
	return FederationCredential{
		HubURL:        credential.HubURL,
		HubProjectID:  credential.HubProjectID,
		Token:         credential.Token,
		Capabilities:  credential.Capabilities,
		Actor:         credential.Actor,
		AllowInsecure: credential.AllowInsecure,
	}
}

type memoryFederationCredentialStore struct {
	mu      sync.RWMutex
	entries map[string]FederationCredential
}

func newMemoryFederationCredentialStore() *memoryFederationCredentialStore {
	return &memoryFederationCredentialStore{entries: make(map[string]FederationCredential)}
}

func (s *memoryFederationCredentialStore) FederationCredential(
	_ context.Context, projectUID string,
) (FederationCredential, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	credential, ok := s.entries[projectUID]
	return credential, ok, nil
}

func (s *memoryFederationCredentialStore) StoreFederationCredential(
	_ context.Context, projectUID string, credential FederationCredential,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[projectUID] = credential
	return nil
}

func (s *memoryFederationCredentialStore) DeleteFederationCredential(
	_ context.Context, projectUID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, projectUID)
	return nil
}
