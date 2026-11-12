package daemon

import (
	"context"
	"net/http"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/httpurl"
)

// FederationHubClientFactory constructs an origin-pinned enrollment transport.
type FederationHubClientFactory func(context.Context, config.FederationCredential) (*http.Client, error)

// fetchFederationAdoptionMetadata verifies that the enrollment reaches the
// expected hub project and returns its advertised event features. It does not
// change the credential or project state.
func fetchFederationAdoptionMetadata(ctx context.Context, hubURL string, projectID int64, projectUID string, credential config.FederationCredential, factory FederationHubClientFactory) (string, error) {
	base, err := httpurl.CanonicalHTTPBaseURL(hubURL)
	credentialBase, credentialErr := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
	if err != nil || credentialErr != nil || base != credentialBase || projectID <= 0 || credential.HubProjectID != projectID || credential.Token == "" {
		return "", federationReplicaError(ErrFederationReplicaCredentialConflict, "federation enrollment target does not match the requested hub", "")
	}
	if factory == nil {
		return "", federationReplicaError(ErrFederationReplicaHubUnavailable, "federation transport is unavailable", "")
	}
	ctx, cancel := context.WithTimeout(ctx, federationRebindRequestTimeout)
	defer cancel()
	client, err := factory(ctx, credential)
	if err != nil || client == nil {
		return "", federationReplicaError(ErrFederationReplicaHubUnavailable, "federation transport is unavailable", "")
	}
	// Reject redirects even when the supplied client permits them; the
	// enrollment credential is valid only for the selected hub origin.
	probe := *client
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	metadata, features, err := readFederationHubMetadata(ctx, &probe, base, projectID)
	if err != nil {
		return "", err
	}
	if metadata.ProjectID != projectID || metadata.ProjectUID != projectUID {
		return "", federationReplicaError(ErrFederationReplicaBindingConflict, "hub metadata identifies a different federation project", "")
	}
	return features, nil
}
